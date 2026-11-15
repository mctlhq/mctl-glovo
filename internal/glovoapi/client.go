package glovoapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
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

	refreshMu sync.Mutex // serialises refreshes
	mu        sync.Mutex // guards session
	session   Session
	bits      map[string]string
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets the HTTP client. The default is a client with a 30s
// timeout.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.httpClient = h } }

// WithBaseURL overrides DefaultBaseURL, e.g. for tests.
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithDefaultLocation overrides DefaultLocationFromEnv(os.Getenv).
func WithDefaultLocation(l Location) Option { return func(c *Client) { c.defaultLocation = l } }

// WithSession installs an in-memory session instead of loading the file.
func WithSession(s Session) Option { return func(c *Client) { c.session = s } }

// New returns a client for the session file at sessionPath. It does not read
// the file; call Reload for that.
func New(sessionPath string, opts ...Option) *Client {
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
	if c.defaultLocation == nil {
		c.defaultLocation = DefaultLocationFromEnv(os.Getenv)
	}
	c.initBits()
	return c
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

// Reload reads the session file. A missing file leaves the client signed out
// (the documented "not signed in" state). Any other failure, including a
// malformed file, is returned: unlike upstream, it is not treated as signed
// out.
func (c *Client) Reload() error {
	s, err := LoadSession(c.sessionPath)
	c.mu.Lock()
	defer c.mu.Unlock()
	if errors.Is(err, os.ErrNotExist) {
		c.session = nil
		return nil
	}
	if err != nil {
		c.session = nil
		return err
	}
	c.session = s
	return nil
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
		{"User-Agent", "Glovo/10.0.0 (iPhone; iOS 17.5.1; Scale/3.00)"},
		{"Glovo-Api-Version", "14"},
		{"Glovo-App-Context", "customer"},
		{"Glovo-App-Development-State", "prod"},
		{"Glovo-App-Platform", "iOS"},
		{"Glovo-App-Type", "customer"},
		{"Glovo-App-Version", "10.0.0"},
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
	return h
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

func checkMutation(method, path string) error {
	if method == http.MethodGet {
		return nil
	}
	for _, a := range mutationAllowlist {
		if a.method == method && a.path.MatchString(path) {
			return nil
		}
	}
	return fmt.Errorf("glovoapi: refusing %s %s: not an allowed basket or read endpoint", method, safePath(strings.SplitN(path, "?", 2)[0]))
}

type callOpts struct {
	method        string
	body          any
	auth          bool
	allowNotFound bool
}

func (c *Client) send(ctx context.Context, method, path string, body any, auth bool) (int, []byte, string, error) {
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
		return 0, nil, "", fmt.Errorf("%s %s: %w", method, safePath(strings.SplitN(path, "?", 2)[0]), err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
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
		return &AuthError{Msg: fmt.Sprintf("Glovo refresh failed (%d). Run glovo_login again.", status)}
	}
	v, err := decodeJSON(data)
	if err != nil {
		return &AuthError{Msg: "Glovo refresh failed (malformed response). Run glovo_login again."}
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
