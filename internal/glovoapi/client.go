package glovoapi

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is the Glovo customer API origin.
const DefaultBaseURL = "https://api.glovoapp.com"

// maxBodyBytes bounds a response body. A larger body is truncated and then
// fails to decode, so it is an error rather than a partial result.
const maxBodyBytes = 16 << 20

// AuthError means the Glovo session is missing, expired or rejected. The
// caller should ask the user to sign in again.
type AuthError struct{ Msg string }

func (e *AuthError) Error() string { return e.Msg }

// APIError is a non-2xx Glovo response. Path and Detail are redacted: they
// carry no customer, basket, product or order identifiers.
type APIError struct {
	Method string
	Path   string
	Status int
	Detail string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s %s -> %d: %s", e.Method, e.Path, e.Status, e.Detail)
}

// RateLimited reports a 429 response.
func (e *APIError) RateLimited() bool { return e.Status == http.StatusTooManyRequests }

func isRateLimited(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.RateLimited()
}

// Location is the browsing location used for the Glovo-Location-* and
// Glovo-Delivery-Location-* headers: the defaults merged with the session's
// location object.
type Location map[string]any

// DefaultLocationFromEnv returns the upstream DEFAULT_LOCATION, honouring the
// GLOVO_COUNTRY_CODE, GLOVO_CITY_CODE, GLOVO_LATITUDE, GLOVO_LONGITUDE and
// GLOVO_LANGUAGE_CODE overrides.
func DefaultLocationFromEnv(getenv func(string) string) Location {
	pick := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	return Location{
		"countryCode":  pick("GLOVO_COUNTRY_CODE", "ES"),
		"cityCode":     pick("GLOVO_CITY_CODE", "BCN"),
		"latitude":     pick("GLOVO_LATITUDE", "41.3874"),
		"longitude":    pick("GLOVO_LONGITUDE", "2.1686"),
		"accuracy":     "0",
		"languageCode": pick("GLOVO_LANGUAGE_CODE", "en"),
	}
}

// Client talks to the Glovo customer API on behalf of one session file.
// It is safe for concurrent use.
type Client struct {
	httpClient      *http.Client
	baseURL         string
	sessionPath     string
	defaultLocation Location
	now             func() time.Time
	sleep           func(context.Context, time.Duration) error
	newID           func() string

	// Egress. proxyURL, when set, is the only route to baseURL: the
	// transport is built here and never consults the environment.
	proxyURL     *url.URL
	requireProxy bool
	customHTTP   bool
	proxyTLS     *tls.Config // tests only: trust roots for the upstream
	optErr       error

	refreshMu sync.Mutex // serialises refreshes
	mu        sync.Mutex // guards session
	session   Session
	loadErr   error
	bits      map[string]string
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets the HTTP client. The default is a client with a 30s
// timeout. It cannot be combined with WithProxy or WithRequireProxy, because
// a caller-supplied transport could route around the proxy.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h == nil {
			c.setOptErr(errors.New("glovoapi: WithHTTPClient: nil client"))
			return
		}
		c.httpClient, c.customHTTP = h, true
	}
}

// WithProxy sends every request, token refresh included, through the HTTP
// forward proxy at proxyURL. The client builds its own transport, a clone of
// http.DefaultTransport whose Proxy is http.ProxyURL(proxyURL), so
// HTTPS_PROXY, HTTP_PROXY and NO_PROXY are never consulted and cannot route
// Glovo traffic around the proxy. A request the proxy cannot carry fails; it
// is never retried without the proxy.
//
// Only "http://host:port" is accepted: the proxy speaks plain HTTP CONNECT
// and TLS to Glovo stays end to end. Other schemes, userinfo, an empty host,
// a missing port, a path, a query or a fragment are refused by New.
func WithProxy(proxyURL *url.URL) Option {
	return func(c *Client) {
		if err := validateProxyURL(proxyURL); err != nil {
			c.setOptErr(err)
			return
		}
		u := *proxyURL
		c.proxyURL = &u
	}
}

// WithRequireProxy makes a client without WithProxy a constructor error, and
// requires an https base URL so that nothing crosses the proxy in clear
// text. The cluster service sets it; glovo-home on the owner's machine does
// not, and talks to Glovo directly.
func WithRequireProxy() Option { return func(c *Client) { c.requireProxy = true } }

// withProxyTLSConfig sets the TLS config of the proxy transport. Tests use it
// to trust an httptest TLS upstream.
func withProxyTLSConfig(cfg *tls.Config) Option { return func(c *Client) { c.proxyTLS = cfg } }

func (c *Client) setOptErr(err error) {
	if c.optErr == nil {
		c.optErr = err
	}
}

func validateProxyURL(u *url.URL) error {
	switch {
	case u == nil:
		return errors.New("glovoapi: proxy URL is nil")
	case u.Scheme != "http":
		return fmt.Errorf("glovoapi: proxy URL scheme must be http, got %q", u.Scheme)
	case u.User != nil:
		return errors.New("glovoapi: proxy URL must not carry userinfo")
	case u.Hostname() == "":
		return errors.New("glovoapi: proxy URL has no host")
	case u.Port() == "":
		return errors.New("glovoapi: proxy URL has no port")
	case u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return errors.New("glovoapi: proxy URL must be http://host:port with nothing else")
	}
	return nil
}

// WithBaseURL overrides DefaultBaseURL, e.g. for tests.
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithDefaultLocation overrides DefaultLocationFromEnv(os.Getenv).
func WithDefaultLocation(l Location) Option { return func(c *Client) { c.defaultLocation = l } }

// WithSession installs an in-memory session instead of loading the file.
func WithSession(s Session) Option { return func(c *Client) { c.session = s } }

// WithClock replaces time.Now for token expiry and session timestamps.
// Tests use it; nil is refused.
func WithClock(now func() time.Time) Option {
	return func(c *Client) {
		if now == nil {
			c.setOptErr(errors.New("glovoapi: WithClock: nil clock"))
			return
		}
		c.now = now
	}
}

// WithIDGenerator replaces the random UUID source used for search ids and
// the per-process device identity. Tests use it; nil is refused.
func WithIDGenerator(newID func() string) Option {
	return func(c *Client) {
		if newID == nil {
			c.setOptErr(errors.New("glovoapi: WithIDGenerator: nil generator"))
			return
		}
		c.newID = newID
	}
}

// WithSleep replaces the pause between order-history pages and retries.
// Tests use it; nil is refused.
func WithSleep(sleep func(context.Context, time.Duration) error) Option {
	return func(c *Client) {
		if sleep == nil {
			c.setOptErr(errors.New("glovoapi: WithSleep: nil sleep"))
			return
		}
		c.sleep = sleep
	}
}

// New returns a client for the session file at sessionPath. It does not read
// the file; call Reload for that. It fails on an invalid option, on
// WithProxy or WithRequireProxy combined with WithHTTPClient, and on
// WithRequireProxy without WithProxy or with a non-https base URL.
func New(sessionPath string, opts ...Option) (*Client, error) {
	c := &Client{
		httpClient:  &http.Client{Timeout: 30 * time.Second},
		baseURL:     DefaultBaseURL,
		sessionPath: sessionPath,
		now:         time.Now,
		sleep:       sleepCtx,
		newID:       randomUUID,
	}
	for _, o := range opts {
		o(c)
	}
	if c.optErr != nil {
		return nil, c.optErr
	}
	if err := c.configureEgress(); err != nil {
		return nil, err
	}
	if c.defaultLocation == nil {
		c.defaultLocation = DefaultLocationFromEnv(os.Getenv)
	}
	c.initBits()
	return c, nil
}

// ErrProxyRequired is returned by New for a client built with
// WithRequireProxy but no usable WithProxy.
var ErrProxyRequired = errors.New("glovoapi: an egress proxy is required and none is configured")

func (c *Client) configureEgress() error {
	base, err := url.Parse(c.baseURL)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil {
		return errors.New("glovoapi: base URL must be an http(s) origin")
	}
	if (c.proxyURL != nil || c.requireProxy) && c.customHTTP {
		return errors.New("glovoapi: WithHTTPClient cannot be combined with WithProxy or WithRequireProxy")
	}
	if c.requireProxy {
		if c.proxyURL == nil {
			return ErrProxyRequired
		}
		if base.Scheme != "https" {
			return errors.New("glovoapi: WithRequireProxy needs an https base URL")
		}
	}
	if c.proxyURL != nil {
		tr, err := newTransport()
		if err != nil {
			return err
		}
		tr.Proxy = http.ProxyURL(c.proxyURL)
		if c.proxyTLS != nil {
			tr.TLSClientConfig = http1Only(c.proxyTLS)
		}
		c.httpClient = &http.Client{Timeout: 30 * time.Second, Transport: tr}
	} else if !c.customHTTP {
		tr, err := newTransport()
		if err != nil {
			return err
		}
		c.httpClient = &http.Client{Timeout: 30 * time.Second, Transport: tr}
	}
	// Copy the client so the redirect guard never mutates a caller's value.
	hc := *c.httpClient
	hc.CheckRedirect = c.checkRedirect
	c.httpClient = &hc
	return nil
}

// ErrRedirectRefused is returned when a response redirects off the base URL
// origin, or to a request the mutation allowlist would refuse.
var ErrRedirectRefused = errors.New("glovoapi: refusing a redirect")

// checkRedirect keeps redirects on the base URL origin and re-applies the
// path and mutation checks to the redirected request, so that a 307/308
// cannot carry a POST to an endpoint send would refuse.
func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("%w: more than 10 redirects", ErrRedirectRefused)
	}
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return ErrRedirectRefused
	}
	u := req.URL
	if u.Scheme != base.Scheme || u.Host != base.Host || u.User != nil {
		return fmt.Errorf("%w: off the API origin", ErrRedirectRefused)
	}
	p := u.EscapedPath()
	prefix := strings.TrimRight(base.EscapedPath(), "/")
	if prefix != "" {
		switch {
		case p == prefix:
			// Exactly the base path is the API root, not outside it.
			p = "/"
		case strings.HasPrefix(p, prefix+"/"):
			p = strings.TrimPrefix(p, prefix)
		default:
			return fmt.Errorf("%w: outside the API base path", ErrRedirectRefused)
		}
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	if err := checkPathSegments(p); err != nil {
		return fmt.Errorf("%w: %v", ErrRedirectRefused, err)
	}
	if err := checkMutation(req.Method, p); err != nil {
		return fmt.Errorf("%w: %v", ErrRedirectRefused, err)
	}
	return nil
}

// initBits generates the per-process identity used when the session has
// none, like the module-level bits in api.mjs.
func (c *Client) initBits() {
	id := c.newID()
	c.bits = map[string]string{
		"deviceUrn":               "glv:device:" + c.newID(),
		"perseusClientId":         id,
		"perseusSessionId":        id,
		"perseusSessionTimestamp": strconv.FormatInt(c.now().UnixMilli(), 10),
	}
}

// Reload reads the session file, like reload in api.mjs:121.
//
// A missing file, or one with no token and no location (for example "{}"),
// leaves the client signed out with no error, as upstream does. A file that
// cannot be read or is malformed (not JSON, not an object, a non-string
// token) also leaves the client signed out, but the error is returned and
// kept: AuthStatus reports it as session_error instead of failing. The
// client stays usable either way, so SetLocation can write a fresh session
// over a broken file.
//
// Reload waits for a token refresh in flight and holds the session lock
// while it reads, so a file read cannot race a refresh or a location write
// and put back a session that was already rotated away.
func (c *Client) Reload() error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	s, err := LoadSession(c.sessionPath)
	c.session, c.loadErr = nil, nil
	switch {
	case err == nil:
		c.session = s
		return nil
	case errors.Is(err, os.ErrNotExist), errors.Is(err, ErrNoSessionData):
		return nil
	default:
		c.loadErr = err
		return err
	}
}

// Session returns a shallow copy of the current session, or nil.
func (c *Client) Session() Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == nil {
		return nil
	}
	out := make(Session, len(c.session))
	for k, v := range c.session {
		out[k] = v
	}
	return out
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func randomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// AuthStatus is the glovo_auth_status result (authStatus in api.mjs).
type AuthStatus struct {
	SignedIn         bool `json:"signed_in"`
	AccessTokenValid bool `json:"access_token_valid"`
	SecondsLeft      int  `json:"seconds_left"`
	HasCustomer      bool `json:"has_customer"`
	HasLocation      bool `json:"has_location"`
	// SessionError is set when the session file exists but could not be
	// loaded. Upstream reports such a file as plain signed out.
	SessionError string `json:"session_error,omitempty"`
}

// AuthStatus reports the session state without any network call.
func (c *Client) AuthStatus() AuthStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.session
	st := tokenStatus(s, c.now())
	var m map[string]any = s
	loc := get(m, "location")
	return AuthStatus{
		SignedIn:         truthy(get(m, "refreshToken")) || st.Valid,
		AccessTokenValid: st.Valid,
		SecondsLeft:      int(math.Max(0, math.Floor(st.SecondsLeft+0.5))),
		HasCustomer:      truthy(coalesce(get(m, "customerId"), get(m, "customer", "id"))),
		HasLocation:      truthy(get(loc, "cityCode")) || truthy(get(loc, "latitude")),
		SessionError:     sessionErrorText(c.loadErr),
	}
}

func sessionErrorText(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrSessionMalformed):
		return ErrSessionMalformed.Error()
	default:
		return "Glovo session file could not be read."
	}
}

// Location returns the defaults merged with the session location.
func (c *Client) Location() Location {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.locationLocked()
}

func (c *Client) locationLocked() Location {
	out := make(Location, len(c.defaultLocation)+4)
	for k, v := range c.defaultLocation {
		out[k] = v
	}
	if sl, ok := get(map[string]any(c.session), "location").(map[string]any); ok {
		for k, v := range sl {
			out[k] = v
		}
	}
	return out
}

// headerList keeps header names exactly as glovo-skill spells them.
type headerList [][2]string

// webVersion is glovo-skill 0.2.1's WEB_VERSION: the client identity the
// session was issued to. Glovo rejects a refresh that presents another one.
const webVersion = "v1.2476.1"

// undiciHeaders are the headers Node's fetch (undici, Node 22 and later)
// adds to every glovo-skill request, in the order and spelling it sends
// them (testdata/golden/undici_wire.json in internal/glovotools). User-Agent
// and Accept-Encoding use Go's canonical spelling: net/http looks both up
// by that key, and would otherwise add its own "Go-http-client/1.1" and
// "gzip". Header names are case-insensitive on the wire.
var undiciHeaders = headerList{
	{"accept-language", "*"},
	{"sec-fetch-mode", "cors"},
	{"User-Agent", "node"},
	{"Accept-Encoding", "gzip, deflate"},
}

func (c *Client) baseHeaders(auth bool) headerList {
	c.mu.Lock()
	defer c.mu.Unlock()
	loc := Value(map[string]any(c.locationLocked()))
	sess := Value(map[string]any(c.session))
	pick := func(key string) string {
		return jsString(or(get(loc, key), get(sess, key), c.bits[key]))
	}
	h := headerList{
		{"Accept", "application/json, text/plain, */*"},
		{"Content-Type", "application/json"},
		{"Glovo-Api-Version", "14"},
		{"Glovo-App-Context", "web"},
		{"Glovo-App-Development-State", "prod"},
		{"Glovo-App-Platform", "web"},
		{"Glovo-App-Type", "customer"},
		{"Glovo-App-Version", jsString(or(get(loc, "appVersion"), webVersion))},
		{"Glovo-Client-Info", jsString(or(get(loc, "clientInfo"), "web-customer-web-react/"+webVersion+" project:customer-web"))},
		{"Glovo-Device-Urn", pick("deviceUrn")},
		{"Glovo-Language-Code", jsString(or(get(loc, "languageCode"), "en"))},
		{"Glovo-Location-Country-Code", jsString(get(loc, "countryCode"))},
		{"Glovo-Location-City-Code", jsString(get(loc, "cityCode"))},
		{"Glovo-Delivery-Location-Latitude", jsString(get(loc, "latitude"))},
		{"Glovo-Delivery-Location-Longitude", jsString(get(loc, "longitude"))},
		{"Glovo-Delivery-Location-Timestamp", strconv.FormatInt(c.now().UnixMilli(), 10)},
		{"Glovo-Delivery-Location-Accuracy", jsString(or(get(loc, "accuracy"), "0"))},
		{"Glovo-Perseus-Client-Id", pick("perseusClientId")},
		{"Glovo-Perseus-Session-Id", pick("perseusSessionId")},
		{"Glovo-Perseus-Session-Timestamp", pick("perseusSessionTimestamp")},
		{"Glovo-Perseus-Consent", jsString(or(get(loc, "perseusConsent"), "essential_functional_marketing"))},
		{"Glovo-Request-Id", c.newID()},
		{"Glovo-Request-TTL", "7500"},
	}
	if tok := get(sess, "accessToken"); auth && truthy(tok) {
		h = append(h, [2]string{"Authorization", jsString(tok)})
	}
	return append(h, undiciHeaders...)
}

// newTransport is the default transport: http.DefaultTransport's settings
// (including HTTPS_PROXY from the environment) but HTTP/1.1 only, as Node's
// fetch speaks, and without net/http's transparent gzip, because the
// request asks for "gzip, deflate" like undici and decodeBody decodes it.
func newTransport() (*http.Transport, error) {
	dt, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("glovoapi: http.DefaultTransport is not an *http.Transport")
	}
	tr := dt.Clone()
	tr.ForceAttemptHTTP2 = false
	tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	tr.DisableCompression = true
	tr.TLSClientConfig = http1Only(tr.TLSClientConfig)
	return tr, nil
}

// http1Only returns a copy of cfg that offers only HTTP/1.1 in ALPN. A
// config copied from a transport that has already spoken HTTP/2 lists
// "h2", and a server that picks it would then get HTTP/1.1 bytes.
func http1Only(cfg *tls.Config) *tls.Config {
	if cfg == nil {
		cfg = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		cfg = cfg.Clone()
	}
	cfg.NextProtos = []string{"http/1.1"}
	return cfg
}

// decodeBody undoes the Content-Encoding the server chose from "gzip,
// deflate". An encoding it does not know is an error, never raw bytes read
// as JSON.
func decodeBody(resp *http.Response) (io.ReadCloser, error) {
	switch enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
		return resp.Body, nil
	case "gzip", "x-gzip":
		return gzip.NewReader(resp.Body)
	case "deflate":
		// HTTP deflate is zlib-wrapped; some servers send raw DEFLATE.
		br := bufio.NewReader(resp.Body)
		if hdr, err := br.Peek(2); err == nil && (uint16(hdr[0])<<8|uint16(hdr[1]))%31 == 0 && hdr[0]&0x0f == 8 {
			return zlib.NewReader(br)
		}
		return flate.NewReader(br), nil
	default:
		return nil, fmt.Errorf("unsupported Content-Encoding %q", enc)
	}
}

// mutationAllowlist is every non-GET request this client may send. Anything
// else, in particular any order, checkout or payment endpoint, is refused
// before it reaches the network.
var mutationAllowlist = []struct {
	method string
	path   *regexp.Regexp
}{
	{http.MethodPost, regexp.MustCompile(`^/oauth/refresh$`)},
	{http.MethodPost, regexp.MustCompile(`^/v1/web/store_wall/(category/[^/?]+|search)(\?|$)`)},
	{http.MethodPost, regexp.MustCompile(`^/v4/stores/[^/?]+/addresses/[^/?]+/products/[^/?]+/view(\?|$)`)},
	{http.MethodPost, regexp.MustCompile(`^/v1/authenticated/customers/[^/?]+/baskets$`)},
	{http.MethodPut, regexp.MustCompile(`^/v1/authenticated/customers/[^/?]+/baskets/[^/?]+/products$`)},
	{http.MethodPatch, regexp.MustCompile(`^/v1/authenticated/customers/[^/?]+/baskets/[^/?]+/products/quantity$`)},
}

// ErrUnsafePath is returned for a request path with an empty, "." or ".."
// segment, also percent-encoded. seg escapes "/" but not dots, so an id of
// ".." would otherwise climb out of its path. Mutations are stricter still;
// see checkMutation.
var ErrUnsafePath = errors.New("glovoapi: refusing a request path with an empty, '.', '..' or slash-carrying segment")

func checkPathSegments(p string) error {
	raw := strings.SplitN(p, "?", 2)[0]
	if !strings.HasPrefix(raw, "/") {
		return ErrUnsafePath
	}
	if raw == "/" {
		// The API root: reachable only as a redirect to the base path.
		return nil
	}
	for _, s := range strings.Split(raw[1:], "/") {
		dec, err := url.PathUnescape(s)
		if err != nil || s == "" || dec == "" || dec == "." || dec == ".." {
			return ErrUnsafePath
		}
	}
	return nil
}

// checkMutationSegments refuses a mutation path with a segment that carries
// an encoded "/" or "\". seg escapes "/" to %2F but does not refuse it, so a
// basket or product id such as "x/../../orders" would reach the server as
// one segment that a proxy or router decoding %2F before routing could
// split and climb out of. Glovo ids never contain a slash. Reads keep
// accepting such ids (a place id is opaque provider text).
func checkMutationSegments(p string) error {
	raw := strings.SplitN(p, "?", 2)[0]
	for _, s := range strings.Split(raw, "/") {
		dec, err := url.PathUnescape(s)
		if err != nil || strings.ContainsAny(dec, "/\\") {
			return ErrUnsafePath
		}
	}
	return nil
}

func checkMutation(method, p string) error {
	if method == http.MethodGet {
		return nil
	}
	if err := checkMutationSegments(p); err != nil {
		return err
	}
	// Match the cleaned path, so that a ".." segment cannot make a path look
	// like an allowed one while resolving to another.
	parts := strings.SplitN(p, "?", 2)
	cleaned := path.Clean(parts[0])
	if len(parts) == 2 {
		cleaned += "?" + parts[1]
	}
	for _, a := range mutationAllowlist {
		if a.method == method && a.path.MatchString(cleaned) && cleaned == p {
			return nil
		}
	}
	return fmt.Errorf("glovoapi: refusing %s %s: not an allowed basket or read endpoint", method, safePath(parts[0]))
}

type callOpts struct {
	method        string
	body          any
	auth          bool
	allowNotFound bool
}

func (c *Client) send(ctx context.Context, method, path string, body any, auth bool) (int, []byte, string, error) {
	if err := checkPathSegments(path); err != nil {
		return 0, nil, "", err
	}
	if err := checkMutation(method, path); err != nil {
		return 0, nil, "", err
	}
	var reader io.Reader
	if body != nil {
		b, err := marshalJSON(body)
		if err != nil {
			return 0, nil, "", fmt.Errorf("encode request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, nil, "", fmt.Errorf("build request: %w", err)
	}
	headers := c.baseHeaders(auth)
	usedToken := ""
	for _, kv := range headers {
		req.Header[kv[0]] = []string{kv[1]}
		if kv[0] == "Authorization" {
			usedToken = kv[1]
		}
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// *url.Error repeats the full request URL, customer and basket ids
		// included; report its cause under the redacted path instead.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, nil, "", fmt.Errorf("%s %s: %w", method, safePath(strings.SplitN(path, "?", 2)[0]), err)
	}
	defer func() { _ = resp.Body.Close() }()
	decoded, err := decodeBody(resp)
	if err != nil {
		return 0, nil, "", fmt.Errorf("%s %s: %w", method, safePath(strings.SplitN(path, "?", 2)[0]), err)
	}
	data, err := io.ReadAll(io.LimitReader(decoded, maxBodyBytes))
	if err != nil {
		return 0, nil, "", fmt.Errorf("%s %s: read body: %w", method, safePath(strings.SplitN(path, "?", 2)[0]), err)
	}
	return resp.StatusCode, data, usedToken, nil
}

// call mirrors GlovoClient.call: it returns the decoded body, or nil for 204
// and for an allowed 404.
func (c *Client) call(ctx context.Context, path string, o callOpts) (Value, error) {
	if o.method == "" {
		o.method = http.MethodGet
	}
	if o.auth {
		if err := c.ensureAuth(ctx); err != nil {
			return nil, err
		}
	}
	for attempt := 0; ; attempt++ {
		status, data, usedToken, err := c.send(ctx, o.method, path, o.body, o.auth)
		if err != nil {
			return nil, err
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			if o.auth && attempt < 1 && c.hasRefreshToken() {
				if err := c.refreshAfterReject(ctx, usedToken); err != nil {
					return nil, err
				}
				continue
			}
			return nil, &AuthError{Msg: fmt.Sprintf("Glovo auth failed (%d). Run glovo_login first.", status)}
		}
		if status == http.StatusNoContent || (o.allowNotFound && status == http.StatusNotFound) {
			return nil, nil
		}
		if status < 200 || status > 299 {
			text := data
			if len(text) > 300 {
				text = text[:300]
			}
			return nil, &APIError{
				Method: o.method,
				Path:   safePath(strings.SplitN(path, "?", 2)[0]),
				Status: status,
				Detail: safeErrorBody(string(text)),
			}
		}
		v, err := decodeJSON(data)
		if err != nil {
			return nil, fmt.Errorf("%s %s -> %d: malformed JSON body: %w", o.method, safePath(strings.SplitN(path, "?", 2)[0]), status, err)
		}
		return v, nil
	}
}

func (c *Client) hasRefreshToken() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return truthy(get(map[string]any(c.session), "refreshToken"))
}

func (c *Client) ensureAuth(ctx context.Context) error {
	c.mu.Lock()
	s := c.session
	st := tokenStatus(s, c.now())
	c.mu.Unlock()
	if s == nil {
		return &AuthError{Msg: "Not signed in yet. Run glovo_login first."}
	}
	if st.SecondsLeft > 60 {
		return nil
	}
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	// Another caller may have refreshed while this one waited.
	c.mu.Lock()
	st = tokenStatus(c.session, c.now())
	c.mu.Unlock()
	if st.SecondsLeft > 60 {
		return nil
	}
	return c.refreshLocked(ctx)
}

// refreshAfterReject refreshes after a 401/403, unless a concurrent caller
// already replaced the token that was rejected.
func (c *Client) refreshAfterReject(ctx context.Context, rejected string) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	c.mu.Lock()
	current := jsString(get(map[string]any(c.session), "accessToken"))
	c.mu.Unlock()
	if rejected != "" && current != rejected {
		return nil
	}
	return c.refreshLocked(ctx)
}

// Refresh exchanges the refresh token for a new access token and persists
// the rotated session.
func (c *Client) Refresh(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	return c.refreshLocked(ctx)
}

func (c *Client) refreshLocked(ctx context.Context) error {
	c.mu.Lock()
	refreshToken := get(map[string]any(c.session), "refreshToken")
	c.mu.Unlock()
	if !truthy(refreshToken) {
		return &AuthError{Msg: "Glovo session expired. Run glovo_login again."}
	}
	status, data, _, err := c.send(ctx, http.MethodPost, "/oauth/refresh", map[string]any{"refreshToken": refreshToken}, false)
	if err != nil {
		return fmt.Errorf("glovo refresh failed: %w", err)
	}
	if status < 200 || status > 299 {
		if refreshRejected(status) {
			return &AuthError{Msg: fmt.Sprintf("Glovo refresh failed (%d). Run glovo_login again.", status)}
		}
		// 429, 5xx and anything else that is not a rejection of the refresh
		// token is transient: the session may still be good, so this is not
		// a re-login signal, and withRetry may retry it.
		text := data
		if len(text) > 300 {
			text = text[:300]
		}
		return &APIError{Method: http.MethodPost, Path: "/oauth/refresh", Status: status, Detail: safeErrorBody(string(text))}
	}
	v, err := decodeJSON(data)
	if err != nil {
		// A 2xx that is not JSON says nothing about the refresh token.
		return fmt.Errorf("glovo refresh failed: malformed JSON body: %w", err)
	}
	creds := v
	if truthy(get(v, "access", "accessToken")) {
		creds = get(v, "access")
	}
	access, ok := get(creds, "accessToken").(string)
	if !ok || access == "" {
		// Upstream would store an undefined token here; fail closed instead.
		return &AuthError{Msg: "Glovo refresh failed (no access token in response). Run glovo_login again."}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	next := make(Session, len(c.session)+6)
	for k, val := range c.session {
		next[k] = val
	}
	old := map[string]any(c.session)
	next["accessToken"] = access
	next["refreshToken"] = or(get(creds, "refreshToken"), get(old, "refreshToken"))
	setOrDelete(next, "expiresIn", get(creds, "expiresIn"))
	setOrDelete(next, "tokenType", or(get(creds, "tokenType"), get(old, "tokenType")))
	next["scope"] = orNull(coalesce(get(creds, "scope"), get(old, "scope")))
	next["createdAt"] = msNumber(c.now())
	if err := SaveSession(c.sessionPath, next, c.now()); err != nil {
		return fmt.Errorf("persist refreshed Glovo session: %w", err)
	}
	c.session = next
	return nil
}

// refreshRejected reports a refresh response that rejects the refresh token
// itself (invalid_grant and the like): only then is a new sign-in needed.
func refreshRejected(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusUnauthorized || status == http.StatusForbidden
}

// setOrDelete assigns v, or removes the key when v is undefined (which is how
// JSON.stringify treats an undefined property).
func setOrDelete(m Session, key string, v Value) {
	if v == nil {
		delete(m, key)
		return
	}
	m[key] = v
}

var (
	reCustomerPath   = regexp.MustCompile(`/authenticated/customers/[^/]+`)
	reBasketProducts = regexp.MustCompile(`/baskets/[^/]+/products/[^/]+`)
	reBasketProduct  = regexp.MustCompile(`/baskets/[^/]+/products`)
	reBasket         = regexp.MustCompile(`/baskets/[^/]+`)
	reOrder          = regexp.MustCompile(`/orders/[^/]+`)
	reUUID           = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	rePrefixedID     = regexp.MustCompile(`\b(?:basket|product|store|address|customer|bp|sp|p|e)-[A-Za-z0-9_-]+\b`)
	reDigits         = regexp.MustCompile(`\d+`)
)

// safePath mirrors safePath in api.mjs, plus order ids, which upstream left in
// error messages.
func safePath(p string) string {
	p = reCustomerPath.ReplaceAllString(p, "/authenticated/customers/[customer]")
	p = reBasketProducts.ReplaceAllString(p, "/baskets/[basket]/products/[products]")
	p = reBasketProduct.ReplaceAllString(p, "/baskets/[basket]/products")
	p = reBasket.ReplaceAllString(p, "/baskets/[basket]")
	p = reOrder.ReplaceAllString(p, "/orders/[order]")
	return p
}

func redactIdentifiers(v Value) string {
	s := ""
	if truthy(v) {
		s = jsString(v)
	}
	s = reUUID.ReplaceAllString(s, "[id]")
	s = rePrefixedID.ReplaceAllString(s, "[id]")
	return reDigits.ReplaceAllString(s, "#")
}

func safeErrorBody(text string) string {
	parsed, err := decodeJSON([]byte(text))
	if err != nil {
		r := []rune(redactIdentifiers(text))
		if len(r) > 180 {
			r = r[:180]
		}
		return string(r)
	}
	if name := get(parsed, "error", "exceptionName"); truthy(name) {
		return jsString(name)
	}
	if s := redactIdentifiers(or(get(parsed, "error", "staticCode"), get(parsed, "error", "message"))); s != "" {
		return s
	}
	return "request_failed"
}
