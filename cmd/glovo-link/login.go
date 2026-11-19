package main

// Loopback login (mctlhq/mctl-glovo#42, part B2).
//
// The helper runs an OAuth authorization-code login with PKCE (S256) against
// mctl-api, asking for the link resource (RFC 8707) with a redirect to a
// listener on 127.0.0.1. The result is a fresh-login access token for the
// link audience. mctl-api issues no refresh token for that audience, and the
// helper would discard one anyway: the token lives in this process's memory
// only, is never written anywhere and is never printed.
//
// mctl-api shows a consent page in the browser for every client kind on the
// link resource, so the helper never expects an immediate code: it waits for
// the browser to come back to the loopback listener, up to a timeout.
//
// Nothing here decides anything. The checks on the token's claims are a
// pre-filter that stops a useless request early; the link routes verify the
// token against mctl-api's JWKS (exact aud, exact iss, EdDSA, auth_time) and
// that verdict is the only one that authorizes.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	defaultIssuer = "https://api.mctl.ai"
	// maxAuthAge is the server's hard cap on GLOVO_LINK_MAX_AUTH_AGE
	// (authn.MaxFreshLoginAge, 15m), so the helper never refuses a token the
	// link routes would accept whatever the configured value; the server
	// stays the authority and enforces the configured age (default 5m).
	maxAuthAge = 15 * time.Minute
	// clockLeeway is the future skew tolerated on auth_time, the same as the
	// server's authn.Leeway: a laptop clock a little behind mctl-api must not
	// turn a fresh login into "too old".
	clockLeeway  = 30 * time.Second
	loginTimeout = 5 * time.Minute
	maxLoginBody = 16 << 10
	maxTokenLen  = 8 << 10
)

// Fixed login texts. No text carries a token, a code or server output.
const (
	msgClientID       = "refused: -client-id is required (the OAuth client ID registered for this helper)"
	msgLoginDiscovery = "refused: the MCTL authorization server metadata could not be used"
	msgLoginListen    = "refused: the loopback listener could not be started"
	msgLoginDenied    = "refused: the MCTL login was not completed"
	msgLoginState     = "refused: the login answer did not match this login attempt"
	msgLoginTimeout   = "refused: the MCTL login did not finish in time"
	msgLoginToken     = "refused: MCTL did not issue a usable link token" // #nosec G101 -- fixed refusal text, not a credential
	msgTokenResource  = "refused: the token is not for the link resource" // #nosec G101 -- fixed refusal text, not a credential
	msgTokenStale     = "refused: the MCTL login is too old; run glovo-link again"
	msgForwardOrigin  = "refused: the token may only be sent to the link server" // #nosec G101 -- fixed refusal text, not a credential
)

var (
	jwtShape  = regexp.MustCompile(`^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`)
	errRandom = errors.New("random source failed")
)

type asMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
}

// loginParams is what one login needs.
type loginParams struct {
	Issuer   string
	ClientID string
	Resource string
	Timeout  time.Duration
	Open     func(string) error
	Now      func() time.Time
}

func randomB64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", errRandom
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// sameHTTPSOrigin reports whether raw is an https URL on origin, with no
// userinfo, query or fragment.
func sameHTTPSOrigin(raw, origin string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return "https://"+u.Host == origin
}

// discover reads RFC 8414 metadata. The endpoints must live on the issuer's
// own origin and S256 must be offered, else the login does not start.
func discover(ctx context.Context, hc *http.Client, issuer string) (*asMetadata, string) {
	origin, ok := checkServer(issuer)
	if !ok || origin != issuer {
		return nil, msgLoginDiscovery
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/oauth-authorization-server", nil)
	if err != nil {
		return nil, msgLoginDiscovery
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, msgLoginDiscovery
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, msgLoginDiscovery
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxLoginBody+1))
	if err != nil || len(data) > maxLoginBody {
		return nil, msgLoginDiscovery
	}
	var md asMetadata
	if json.Unmarshal(data, &md) != nil || md.Issuer != issuer {
		return nil, msgLoginDiscovery
	}
	if !sameHTTPSOrigin(md.AuthorizationEndpoint, issuer) || !sameHTTPSOrigin(md.TokenEndpoint, issuer) {
		return nil, msgLoginDiscovery
	}
	s256 := false
	for _, m := range md.CodeChallengeMethods {
		if m == "S256" {
			s256 = true
		}
	}
	if !s256 {
		return nil, msgLoginDiscovery
	}
	return &md, ""
}

type callbackResult struct {
	code string
	msg  string
}

// callbackHandler accepts exactly one answer on /callback: the state must
// match, an RFC 9207 iss (when sent) must be the issuer, and there must be a
// code. Anything else ends the attempt.
func callbackHandler(state, issuer, hostport string, out chan<- callbackResult) http.Handler {
	var once sync.Once
	finish := func(r callbackResult) { once.Do(func() { out <- r }) }
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		// A DNS-rebinding page reaches the listener under another Host.
		if r.Host != hostport {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.URL.Path != "/callback" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		got := q.Get("state")
		if len(got) == 0 || subtle.ConstantTimeCompare([]byte(got), []byte(state)) != 1 {
			finish(callbackResult{msg: msgLoginState})
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "glovo-link: login refused. Close this tab.\n")
			return
		}
		if iss, has := q["iss"]; has && (len(iss) != 1 || iss[0] != issuer) {
			finish(callbackResult{msg: msgLoginState})
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "glovo-link: login refused. Close this tab.\n")
			return
		}
		code := q.Get("code")
		if q.Get("error") != "" || code == "" || len(q["code"]) != 1 {
			finish(callbackResult{msg: msgLoginDenied})
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "glovo-link: login not completed. Close this tab.\n")
			return
		}
		finish(callbackResult{code: code})
		_, _ = io.WriteString(w, "glovo-link: login received. You can close this tab.\n")
	})
}

// login returns a link-audience access token or one fixed refusal text.
func login(ctx context.Context, hc *http.Client, p loginParams, stderr io.Writer) (string, string) {
	if p.ClientID == "" {
		return "", msgClientID
	}
	md, msg := discover(ctx, hc, p.Issuer)
	if msg != "" {
		return "", msg
	}
	verifier, err := randomB64(32)
	if err != nil {
		return "", msgLoginListen
	}
	state, err := randomB64(32)
	if err != nil {
		return "", msgLoginListen
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", msgLoginListen
	}
	hostport := ln.Addr().String()
	redirect := "http://" + hostport + "/callback"
	results := make(chan callbackResult, 1)
	srv := &http.Server{
		Handler:           callbackHandler(state, p.Issuer, hostport, results),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	au, _ := url.Parse(md.AuthorizationEndpoint)
	au.RawQuery = url.Values{
		"response_type":         {"code"},
		"client_id":             {p.ClientID},
		"redirect_uri":          {redirect},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		"resource":              {p.Resource},
	}.Encode()
	_, _ = io.WriteString(stderr, "\nSign in to MCTL in your browser and approve the consent page. If no browser opened, open this address:\n"+au.String()+"\n")
	if p.Open != nil {
		_ = p.Open(au.String()) // the address is printed above either way
	}

	wait := p.Timeout
	if wait <= 0 {
		wait = loginTimeout
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	var res callbackResult
	select {
	case res = <-results:
	case <-timer.C:
		return "", msgLoginTimeout
	case <-ctx.Done():
		return "", msgLoginTimeout
	}
	if res.msg != "" {
		return "", res.msg
	}

	token, msg := exchange(ctx, hc, md.TokenEndpoint, p, res.code, verifier, redirect)
	if msg != "" {
		return "", msg
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	if msg := prefilter(token, p.Issuer, p.Resource, now()); msg != "" {
		return "", msg
	}
	return token, ""
}

func exchange(ctx context.Context, hc *http.Client, tokenURL string, p loginParams, code, verifier, redirect string) (string, string) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"client_id":     {p.ClientID},
		"code_verifier": {verifier},
		"resource":      {p.Resource},
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", msgLoginToken
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", msgLoginToken
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", msgLoginToken
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxLoginBody+1))
	if err != nil || len(data) > maxLoginBody {
		return "", msgLoginToken
	}
	// refresh_token is deliberately not decoded: none is issued for the
	// link audience and this helper would not keep one.
	var tr struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if json.Unmarshal(data, &tr) != nil || !strings.EqualFold(tr.TokenType, "bearer") {
		return "", msgLoginToken
	}
	if tr.AccessToken == "" || len(tr.AccessToken) > maxTokenLen || !jwtShape.MatchString(tr.AccessToken) {
		return "", msgLoginToken
	}
	return tr.AccessToken, ""
}

// prefilter reads the token's claims WITHOUT verifying the signature. It is
// not proof of anything: it only refuses early a token the link routes
// would refuse anyway (another audience, another issuer, a login older than
// maxAuthAge, none at all). Passing it authorizes nothing; the link routes
// verify the token against mctl-api's JWKS and are the authority.
func prefilter(token, issuer, resource string, now time.Time) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return msgLoginToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return msgLoginToken
	}
	var c struct {
		Iss      string          `json:"iss"`
		Aud      json.RawMessage `json:"aud"`
		AuthTime json.RawMessage `json:"auth_time"`
	}
	if json.Unmarshal(raw, &c) != nil {
		return msgLoginToken
	}
	if c.Iss != issuer || !audMatches(c.Aud, resource) {
		return msgTokenResource
	}
	var at int64
	if json.Unmarshal(c.AuthTime, &at) != nil || at <= 0 {
		return msgTokenStale
	}
	age := now.Sub(time.Unix(at, 0))
	if age < -clockLeeway || age > maxAuthAge {
		return msgTokenStale
	}
	return ""
}

func audMatches(raw json.RawMessage, resource string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == resource
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil && len(many) == 1 {
		return many[0] == resource
	}
	return false
}
