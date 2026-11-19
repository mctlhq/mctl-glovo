package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIDP is a stand-in for mctl-api's authorization server: metadata,
// an authorize endpoint that answers a consent page (never an immediate
// code), a consent step, and a token endpoint that checks PKCE. Its tokens
// are unsigned: the helper never verifies signatures, the server does.
type fakeIDP struct {
	srv  *httptest.Server
	link *fakeServer

	mu      sync.Mutex
	reqs    map[string]url.Values // by request id
	codes   map[string]url.Values // by code
	issued  string
	tokenQ  url.Values
	authQ   url.Values
	authN   int
	tokenN  int
	consent bool // the consent page was served before any code

	// hooks
	tamperState  bool
	deny         bool
	noConsent    bool
	consentDelay time.Duration
	aud          string
	iss          string
	authAge      time.Duration
	omitToken    bool
	extra        map[string]any
	meta         func(m map[string]any)
}

func newFakeIDP(t *testing.T, link *fakeServer) *fakeIDP {
	t.Helper()
	i := &fakeIDP{link: link, reqs: map[string]url.Values{}, codes: map[string]url.Values{}}
	i.srv = httptest.NewTLSServer(http.HandlerFunc(i.serve))
	t.Cleanup(i.srv.Close)
	return i
}

func (i *fakeIDP) issuedToken() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.issued
}

func (i *fakeIDP) counts() (authorize, token int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.authN, i.tokenN
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (i *fakeIDP) mint() string {
	aud := i.aud
	if aud == "" {
		aud = i.link.srv.URL + "/link"
	}
	iss := i.iss
	if iss == "" {
		iss = i.srv.URL
	}
	claims, _ := json.Marshal(map[string]any{
		"iss": iss, "aud": aud, "sub": "pid-test",
		"auth_time": time.Now().Add(-i.authAge).Unix(),
	})
	return b64([]byte(`{"alg":"EdDSA","typ":"JWT"}`)) + "." + b64(claims) + "." + b64([]byte("sig-for-"+aud))
}

func (i *fakeIDP) serve(w http.ResponseWriter, r *http.Request) {
	i.mu.Lock()
	defer i.mu.Unlock()
	switch r.URL.Path {
	case "/.well-known/oauth-authorization-server":
		m := map[string]any{
			"issuer":                           i.srv.URL,
			"authorization_endpoint":           i.srv.URL + "/authorize",
			"token_endpoint":                   i.srv.URL + "/token",
			"code_challenge_methods_supported": []string{"S256"},
		}
		if i.meta != nil {
			i.meta(m)
		}
		_ = json.NewEncoder(w).Encode(m)
	case "/authorize":
		i.authN++
		q := r.URL.Query()
		i.authQ = q
		if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" {
			w.WriteHeader(400)
			return
		}
		id := "req-" + q.Get("state")[:8]
		i.reqs[id] = q
		i.consent = true
		w.Header().Set("X-Req", id)
		_, _ = w.Write([]byte("<html>consent page</html>")) // no code yet
	case "/consent":
		q, ok := i.reqs[r.URL.Query().Get("req")]
		if r.Method != http.MethodPost || !ok {
			w.WriteHeader(400)
			return
		}
		state := q.Get("state")
		if i.tamperState {
			state = "forged-state"
		}
		loc, _ := url.Parse(q.Get("redirect_uri"))
		v := url.Values{"state": {state}}
		if i.deny {
			v.Set("error", "access_denied")
		} else {
			code := "code-" + state
			i.codes[code] = q
			v.Set("code", code)
		}
		loc.RawQuery = v.Encode()
		w.Header().Set("Location", loc.String())
		w.WriteHeader(302)
	case "/token":
		i.tokenN++
		_ = r.ParseForm()
		i.tokenQ = r.PostForm
		q, ok := i.codes[r.PostForm.Get("code")]
		delete(i.codes, r.PostForm.Get("code"))
		if !ok || r.PostForm.Get("grant_type") != "authorization_code" ||
			r.PostForm.Get("redirect_uri") != q.Get("redirect_uri") ||
			r.PostForm.Get("client_id") != q.Get("client_id") ||
			r.PostForm.Get("resource") != q.Get("resource") {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if b64(sum[:]) != q.Get("code_challenge") {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		resp := map[string]any{"token_type": "Bearer", "expires_in": 600}
		if !i.omitToken {
			i.issued = i.mint()
			resp["access_token"] = i.issued
		}
		for k, v := range i.extra {
			resp[k] = v
		}
		_ = json.NewEncoder(w).Encode(resp)
	default:
		w.WriteHeader(404)
	}
}

// browser plays the user: open the authorize address, see the consent page,
// approve, and follow the redirect to the helper's loopback listener.
func (i *fakeIDP) browser(u string) error {
	go func() {
		c := i.link.client()
		resp, err := c.Get(u)
		if err != nil {
			return
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("Location") != "" {
			return
		}
		if i.noConsent {
			return
		}
		time.Sleep(i.consentDelay)
		pr, err := c.Post(i.srv.URL+"/consent?req="+url.QueryEscape(resp.Header.Get("X-Req")), "text/plain", nil)
		if err != nil {
			return
		}
		_ = pr.Body.Close()
		if loc := pr.Header.Get("Location"); loc != "" {
			if cb, err := http.Get(loc); err == nil { // #nosec G107 -- the helper's own loopback address
				_ = cb.Body.Close()
			}
		}
	}()
	return nil
}

func happy(t *testing.T, f *fakeServer) result {
	t.Helper()
	return runHelper(t, f, []string{"-session", writeSession(t, sessionJSON, 0o600)}, testCode+"\ny\n")
}

// A consent page comes first and the code only after approval; the request
// is the contract: PKCE S256, the link resource, a loopback redirect, the
// caller's client ID.
func TestLoginRequestShape(t *testing.T) {
	f := newFakeServer(t)
	f.idp.consentDelay = 150 * time.Millisecond // the helper must wait for the user
	r := happy(t, f)
	if r.code != 0 || r.stdout != "linked\n" {
		t.Fatalf("%d %q %q", r.code, r.stdout, r.stderr)
	}
	i := f.idp
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.consent {
		t.Fatal("no consent step was served")
	}
	q := i.authQ
	if q.Get("resource") != f.srv.URL+"/link" {
		t.Fatalf("resource %q", q.Get("resource"))
	}
	if q.Get("client_id") != testClient || q.Get("code_challenge_method") != "S256" || q.Get("response_type") != "code" {
		t.Fatalf("authorize query %v", q)
	}
	ru, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || ru.Scheme != "http" || !strings.HasPrefix(ru.Host, "127.0.0.1:") || ru.Path != "/callback" {
		t.Fatalf("redirect_uri %q", q.Get("redirect_uri"))
	}
	if len(q.Get("state")) < 40 || len(q.Get("code_challenge")) != 43 {
		t.Fatal("state or challenge too short")
	}
	if i.tokenQ.Get("resource") != q.Get("resource") || i.tokenQ.Get("redirect_uri") != q.Get("redirect_uri") {
		t.Fatalf("token request %v", i.tokenQ)
	}
	if !strings.Contains(r.stderr, "o***@e***.com") {
		t.Fatal("masked identity not shown")
	}
	// Exactly these parameters: no client-side prompt=login, max_age or scope.
	want := map[string]bool{"response_type": true, "client_id": true, "redirect_uri": true, "code_challenge": true, "code_challenge_method": true, "state": true, "resource": true}
	for k := range q {
		if !want[k] {
			t.Fatalf("unexpected authorize parameter %q", k)
		}
	}
	wantTok := map[string]bool{"grant_type": true, "code": true, "redirect_uri": true, "client_id": true, "code_verifier": true, "resource": true}
	for k := range i.tokenQ {
		if !wantTok[k] {
			t.Fatalf("unexpected token parameter %q", k)
		}
	}
}

// The listener is bound to 127.0.0.1 only: not reachable on any other
// local address.
func TestListenerBindsLoopbackOnly(t *testing.T) {
	f := newFakeServer(t)
	e := f.env(t)
	redirects := make(chan string, 1)
	e.open = func(u string) error {
		pu, _ := url.Parse(u)
		redirects <- pu.Query().Get("redirect_uri")
		return nil // no browser: the login will time out
	}
	e.loginTimeout = 700 * time.Millisecond
	done := make(chan result, 1)
	go func() {
		done <- runEnv(t, f, e, []string{"-server", f.srv.URL, "-client-id", testClient}, testCode+"\ny\n")
	}()
	redirect := <-redirects
	ru, err := url.Parse(redirect)
	if err != nil || ru.Hostname() != "127.0.0.1" {
		t.Fatalf("redirect %q", redirect)
	}
	addrs, _ := net.InterfaceAddrs()
	tried := 0
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.To4() == nil {
			continue
		}
		tried++
		c, err := net.DialTimeout("tcp", net.JoinHostPort(ipn.IP.String(), ru.Port()), time.Second)
		if err == nil {
			_ = c.Close()
			t.Fatalf("listener reachable on %s", ipn.IP)
		}
	}
	t.Logf("non-loopback addresses probed: %d", tried)
	if r := <-done; r.stdout != msgLoginTimeout+"\n" {
		t.Fatalf("%q", r.stdout)
	}
}

func TestClientIDHasNoDefault(t *testing.T) {
	f := newFakeServer(t)
	var out, errb strings.Builder
	code := run([]string{"-server", f.srv.URL}, strings.NewReader(testCode+"\ny\n"), &out, &errb, f.env(t))
	if code != 2 || out.String() != msgClientID+"\n" {
		t.Fatalf("%d %q", code, out.String())
	}
	if a, _ := f.idp.counts(); a != 0 || f.totalCalls() != 0 {
		t.Fatal("a login or link call ran without a client ID")
	}
}

// A state that does not match this attempt ends it before any token request.
func TestStateMismatchRefused(t *testing.T) {
	f := newFakeServer(t)
	f.idp.tamperState = true
	r := happy(t, f)
	if r.code == 0 || r.stdout != msgLoginState+"\n" {
		t.Fatalf("%d %q", r.code, r.stdout)
	}
	if _, tn := f.idp.counts(); tn != 0 || f.totalCalls() != 0 {
		t.Fatalf("token calls %d, link calls %d after a state mismatch", tn, f.totalCalls())
	}
}

// PKCE: the IdP checks the verifier against the S256 challenge; a plain
// or wrong challenge makes the exchange fail and nothing reaches the link
// server.
func TestPKCEMismatchRefused(t *testing.T) {
	f := newFakeServer(t)
	f.idp.meta = func(m map[string]any) {}
	e := f.env(t)
	// Tamper at the authorize step: the fake browser swaps the challenge
	// for a different one before it records the request.
	e.open = func(u string) error {
		pu, _ := url.Parse(u)
		q := pu.Query()
		q.Set("code_challenge", b64([]byte("0123456789abcdef0123456789abcdef")))
		pu.RawQuery = q.Encode()
		return f.idp.browser(pu.String())
	}
	r := runEnv(t, f, e, []string{"-server", f.srv.URL, "-client-id", testClient, "-session", writeSession(t, sessionJSON, 0o600)}, testCode+"\ny\n")
	if r.code == 0 || r.stdout != msgLoginToken+"\n" {
		t.Fatalf("%d %q", r.code, r.stdout)
	}
	if f.totalCalls() != 0 {
		t.Fatal("link server called after a failed exchange")
	}
}

// The challenge the helper sends is the S256 of a verifier it then proves.
func TestChallengeIsS256OfVerifier(t *testing.T) {
	f := newFakeServer(t)
	r := happy(t, f)
	if r.code != 0 {
		t.Fatalf("%d %q", r.code, r.stdout)
	}
	f.idp.mu.Lock()
	defer f.idp.mu.Unlock()
	sum := sha256.Sum256([]byte(f.idp.tokenQ.Get("code_verifier")))
	if b64(sum[:]) != f.idp.authQ.Get("code_challenge") || len(f.idp.tokenQ.Get("code_verifier")) < 43 {
		t.Fatal("challenge is not S256 of the verifier")
	}
}

func TestMissingTokenRefused(t *testing.T) {
	f := newFakeServer(t)
	f.idp.omitToken = true
	r := happy(t, f)
	if r.code == 0 || r.stdout != msgLoginToken+"\n" || f.totalCalls() != 0 {
		t.Fatalf("%d %q, link calls %d", r.code, r.stdout, f.totalCalls())
	}
}

func TestWrongResourceTokenRefused(t *testing.T) {
	for name, set := range map[string]func(i *fakeIDP){
		"aud is the MCP resource": func(i *fakeIDP) { i.aud = "https://glovo.mctl.ai/mcp" },
		"aud is another host":     func(i *fakeIDP) { i.aud = "https://other.example/link" },
		"iss is another issuer":   func(i *fakeIDP) { i.iss = "https://evil.example" },
	} {
		f := newFakeServer(t)
		set(f.idp)
		r := happy(t, f)
		if r.code == 0 || r.stdout != msgTokenResource+"\n" || f.totalCalls() != 0 {
			t.Errorf("%s: %d %q, link calls %d", name, r.code, r.stdout, f.totalCalls())
		}
	}
}

func TestStaleTokenRefused(t *testing.T) {
	f := newFakeServer(t)
	f.idp.authAge = 10 * time.Minute
	r := happy(t, f)
	if r.code == 0 || r.stdout != msgTokenStale+"\n" || f.totalCalls() != 0 {
		t.Fatalf("%d %q, link calls %d", r.code, r.stdout, f.totalCalls())
	}
}

// A token that ages out while the user reads the confirmation is not sent
// to upload.
func TestTokenAgingDuringConfirmationNotUploaded(t *testing.T) {
	f := newFakeServer(t)
	e := f.env(t)
	calls := 0
	e.now = func() time.Time { // fresh at login and preview, stale at upload
		calls++
		if calls > 1 {
			return time.Now().Add(maxAuthAge + time.Minute)
		}
		return time.Now()
	}
	r := runEnv(t, f, e, []string{"-server", f.srv.URL, "-client-id", testClient, "-session", writeSession(t, sessionJSON, 0o600)}, testCode+"\ny\n")
	if r.code == 0 || r.stdout != msgTokenStale+"\n" {
		t.Fatalf("%d %q", r.code, r.stdout)
	}
	if _, up := f.counts(); up != 0 {
		t.Fatal("uploaded with a stale token")
	}
}

// The server is the authority: its refusals map to fixed texts, and a
// token the helper's pre-filter accepted can still be refused.
func TestServerTokenRefusalsAreFixedTexts(t *testing.T) {
	f := newFakeServer(t)
	f.freshRequired = true
	r := happy(t, f)
	if r.code == 0 || r.stdout != refusals["fresh_login_required"]+"\n" {
		t.Fatalf("%d %q", r.code, r.stdout)
	}
	noMarkers(t, r)
	if _, up := f.counts(); up != 0 {
		t.Fatal("uploaded after a server refusal")
	}
}

func TestPreFilterIsNotAcceptance(t *testing.T) {
	// A token the helper's checks pass is still sent for verification:
	// an unsigned one from the fake is refused by a server that does not
	// recognize it.
	f := newFakeServer(t)
	f.idp.mu.Lock()
	f.idp.issued = ""
	f.idp.mu.Unlock()
	tok := f.idp.mint()
	if msg := prefilter(tok, f.idp.srv.URL, f.srv.URL+"/link", time.Now()); msg != "" {
		t.Fatalf("prefilter: %q", msg)
	}
	rt := target{base: f.srv.URL, token: tok}
	var out struct{}
	if msg := post(t.Context(), f.client(), rt, f.srv.URL+"/link/preview", map[string]any{"schema": previewSchema, "code": testCode}, &out); msg != refusals["invalid_token"] {
		t.Fatalf("got %q", msg)
	}
}

func TestConsentDeniedAndTimeout(t *testing.T) {
	f := newFakeServer(t)
	f.idp.deny = true
	r := happy(t, f)
	if r.code == 0 || r.stdout != msgLoginDenied+"\n" || f.totalCalls() != 0 {
		t.Fatalf("denied: %d %q", r.code, r.stdout)
	}

	f = newFakeServer(t)
	f.idp.noConsent = true // the browser never returns
	e := f.env(t)
	e.loginTimeout = 300 * time.Millisecond
	r = runEnv(t, f, e, []string{"-server", f.srv.URL, "-client-id", testClient, "-session", writeSession(t, sessionJSON, 0o600)}, testCode+"\ny\n")
	if r.code == 0 || r.stdout != msgLoginTimeout+"\n" || f.totalCalls() != 0 {
		t.Fatalf("timeout: %d %q", r.code, r.stdout)
	}
	if _, tn := f.idp.counts(); tn != 0 {
		t.Fatal("token request without a code")
	}
}

func TestDiscoveryRefusals(t *testing.T) {
	for name, mut := range map[string]func(m map[string]any){
		"issuer mismatch":  func(m map[string]any) { m["issuer"] = "https://evil.example" },
		"foreign token":    func(m map[string]any) { m["token_endpoint"] = "https://evil.example/token" },
		"foreign authz":    func(m map[string]any) { m["authorization_endpoint"] = "https://evil.example/authorize" },
		"http token":       func(m map[string]any) { m["token_endpoint"] = "http://127.0.0.1/token" },
		"no S256":          func(m map[string]any) { m["code_challenge_methods_supported"] = []string{"plain"} },
		"no method list":   func(m map[string]any) { delete(m, "code_challenge_methods_supported") },
		"empty authz":      func(m map[string]any) { m["authorization_endpoint"] = "" },
		"authz with query": func(m map[string]any) { m["authorization_endpoint"] = m["issuer"].(string) + "/authorize?x=1" },
	} {
		f := newFakeServer(t)
		f.idp.meta = mut
		r := happy(t, f)
		if r.code == 0 || r.stdout != msgLoginDiscovery+"\n" {
			t.Errorf("%s: %d %q", name, r.code, r.stdout)
		}
		if a, _ := f.idp.counts(); a != 0 || f.totalCalls() != 0 {
			t.Errorf("%s: login or link call after refused metadata", name)
		}
	}
}

// No refresh token is issued for the link audience, and one that arrives is
// neither shown nor forwarded.
func TestRefreshTokenIsDiscarded(t *testing.T) {
	f := newFakeServer(t)
	f.idp.extra = map[string]any{"refresh_token": "MARKER-RT"}
	r := happy(t, f)
	if r.code != 0 {
		t.Fatalf("%d %q", r.code, r.stdout)
	}
	noMarkers(t, r)
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, _ := json.Marshal(f.lastBody); strings.Contains(string(b), "MARKER-RT") {
		t.Fatal("refresh token forwarded")
	}
}

// The token goes to the link server's origin only: not to another host, not
// along a redirect.
func TestTokenNeverLeavesTheLinkOrigin(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	sink := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	defer sink.Close()
	f := newFakeServer(t)
	hc := f.client()
	tok := f.idp.mint()
	for _, u := range []string{
		sink.URL + "/link/preview",
		f.srv.URL + ".evil.example/link/preview",
		strings.Replace(f.srv.URL, "https://", "https://user:pw@", 1) + "/link/preview",
		f.srv.URL + "/../x",
		f.srv.URL, // not a link route path
	} {
		var out struct{}
		if msg := post(t.Context(), hc, target{base: f.srv.URL, token: tok}, u, map[string]any{}, &out); msg != msgForwardOrigin {
			t.Errorf("%s: %q", u, msg)
		}
	}
	// No token, no request.
	var out struct{}
	if msg := post(t.Context(), hc, target{base: f.srv.URL}, f.srv.URL+"/link/preview", map[string]any{}, &out); msg != msgForwardOrigin {
		t.Errorf("empty token: %q", msg)
	}
	// A redirect from the link server is not followed.
	f.mu.Lock()
	f.upload = func(w http.ResponseWriter) {
		w.Header().Set("Location", sink.URL+"/steal")
		w.WriteHeader(307)
	}
	f.mu.Unlock()
	r := happy(t, f)
	if r.code == 0 {
		t.Fatal("redirect accepted")
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("the sink saw %d requests", hits)
	}
}

// The callback listener answers once, only to its own Host and path.
func TestCallbackHandlerHardening(t *testing.T) {
	const state, issuer, hp = "state-value", "https://api.example", "127.0.0.1:5555"
	call := func(host, method, target string) (int, callbackResult, bool) {
		out := make(chan callbackResult, 1)
		h := callbackHandler(state, issuer, hp, out)
		req := httptest.NewRequest(method, target, nil)
		req.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		select {
		case r := <-out:
			return w.Code, r, true
		default:
			return w.Code, callbackResult{}, false
		}
	}
	if c, _, got := call("evil.example", "GET", "/callback?code=c&state="+state); c != 400 || got {
		t.Fatalf("foreign Host: %d %v", c, got)
	}
	if c, _, got := call(hp, "GET", "/favicon.ico"); c != 404 || got {
		t.Fatalf("other path: %d %v", c, got)
	}
	if c, _, got := call(hp, "POST", "/callback?code=c&state="+state); c != 405 || got {
		t.Fatalf("POST: %d %v", c, got)
	}
	if _, r, got := call(hp, "GET", "/callback?code=c&state=x"); !got || r.msg != msgLoginState {
		t.Fatalf("bad state: %+v", r)
	}
	if _, r, got := call(hp, "GET", "/callback?code=c"); !got || r.msg != msgLoginState {
		t.Fatalf("no state: %+v", r)
	}
	if _, r, got := call(hp, "GET", "/callback?code=c&state="+state+"&iss=https%3A%2F%2Fevil.example"); !got || r.msg != msgLoginState {
		t.Fatalf("wrong iss: %+v", r)
	}
	if _, r, got := call(hp, "GET", "/callback?state="+state); !got || r.msg != msgLoginDenied {
		t.Fatalf("no code: %+v", r)
	}
	if _, r, got := call(hp, "GET", "/callback?code=a&code=b&state="+state); !got || r.msg != msgLoginDenied {
		t.Fatalf("two codes: %+v", r)
	}
	if _, r, got := call(hp, "GET", "/callback?code=c&state="+state+"&iss=https%3A%2F%2Fapi.example"); !got || r.code != "c" || r.msg != "" {
		t.Fatalf("good answer: %+v", r)
	}
	// Only the first answer counts.
	out := make(chan callbackResult, 1)
	h := callbackHandler(state, issuer, hp, out)
	for _, target := range []string{"/callback?code=first&state=" + state, "/callback?code=second&state=" + state} {
		req := httptest.NewRequest("GET", target, nil)
		req.Host = hp
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if r := <-out; r.code != "first" || len(out) != 0 {
		t.Fatalf("second answer counted: %+v", r)
	}
}

// Declining after the login uploads nothing, and the token is gone with the
// process: nothing is written under the home directory.
func TestDeclineAfterLoginWritesNothing(t *testing.T) {
	f := newFakeServer(t)
	e := f.env(t)
	r := runEnv(t, f, e, []string{"-server", f.srv.URL, "-client-id", testClient, "-session", writeSession(t, sessionJSON, 0o600)}, testCode+"\nn\n")
	if r.code == 0 || r.stdout != msgDeclined+"\n" {
		t.Fatalf("%d %q", r.code, r.stdout)
	}
	if _, up := f.counts(); up != 0 {
		t.Fatal("uploaded after declining")
	}
	if ents, err := os.ReadDir(e.home); err != nil || len(ents) != 0 {
		t.Fatalf("files written under home: %v", ents)
	}
}
