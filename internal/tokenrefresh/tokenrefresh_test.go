package tokenrefresh

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testUser = "user-123"
	testKey  = "key-456"
)

var testRSA = func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}()

func pkcs1PEM(k *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

// fakeClock advances only when slept on, and records every sleep.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
	onSlp  func(n int) // called before each sleep with the sleep count
}

func newClock() *fakeClock { return &fakeClock{now: time.Now()} }

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	n := len(c.sleeps)
	c.now = c.now.Add(d)
	c.mu.Unlock()
	if c.onSlp != nil {
		c.onSlp(n)
	}
	return ctx.Err()
}
func (c *fakeClock) slept() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

type env struct {
	dir      string
	keyFile  string
	tokFile  string
	readyF   string
	clock    *fakeClock
	logBuf   *bytes.Buffer
	handler  atomic.Value // http.HandlerFunc
	srv      *httptest.Server
	requests atomic.Int32
	start    time.Time
}

func (e *env) startPlus(secs int) time.Time { return e.start.Add(time.Duration(secs) * time.Second) }

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{dir: t.TempDir(), clock: newClock(), logBuf: &bytes.Buffer{}}
	e.keyFile = filepath.Join(e.dir, "key")
	if err := os.WriteFile(e.keyFile, pkcs1PEM(testRSA), 0o600); err != nil {
		t.Fatal(err)
	}
	e.tokFile = filepath.Join(e.dir, "tok", "token")
	if err := os.Mkdir(filepath.Dir(e.tokFile), 0o755); err != nil { //nolint:gosec // test dir
		t.Fatal(err)
	}
	e.readyF = filepath.Join(e.dir, "ready")
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.requests.Add(1)
		e.handler.Load().(http.HandlerFunc)(w, r)
	}))
	t.Cleanup(e.srv.Close)
	e.respond(200, "tok-1", 100)
	return e
}

func (e *env) respond(code int, token string, expiresIn int) {
	e.handler.Store(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		if code == 200 {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": token, "token_type": "Bearer", "expires_in": expiresIn})
		}
	}))
}

func (e *env) cfg() Config {
	return Config{Issuer: e.srv.URL, UserID: testUser, KeyID: testKey, Scope: "openid urn:zitadel:iam:org:project:id:42:aud",
		KeyFile: e.keyFile, TokenFile: e.tokFile, ReadyFile: e.readyF, AllowLoopbackHTTP: true}
}

func (e *env) refresher(t *testing.T) *Refresher {
	t.Helper()
	r, err := New(e.cfg(), Options{Clock: e.clock, Logger: slog.New(slog.NewJSONHandler(e.logBuf, nil)), Jitter: func() float64 { return 0 }})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *env) token() (string, bool) {
	b, err := os.ReadFile(e.tokFile)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// stopAfter cancels the run after n sleeps.
func (e *env) stopAfter(cancel context.CancelFunc, n int) {
	e.clock.onSlp = func(c int) {
		if c >= n {
			cancel()
		}
	}
}

func TestSuccessWritesTokenAtomicallyWithMode(t *testing.T) {
	e := newEnv(t)
	e.respond(200, "abc.def.ghi", 100)
	r := e.refresher(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.stopAfter(cancel, 1)
	if err := r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	got, ok := e.token()
	if !ok || got != "abc.def.ghi" {
		t.Fatalf("token file = %q, %v", got, ok)
	}
	fi, _ := os.Stat(e.tokFile)
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
	ents, _ := os.ReadDir(filepath.Dir(e.tokFile))
	if len(ents) != 1 {
		t.Fatalf("temp files left behind: %v", ents)
	}
	if _, err := os.Stat(e.readyF); err != nil {
		t.Fatalf("ready file missing: %v", err)
	}
}

func TestCustomFileMode(t *testing.T) {
	e := newEnv(t)
	cfg := e.cfg()
	cfg.FileMode = 0o600
	r, err := New(cfg, Options{Clock: e.clock, Jitter: func() float64 { return 0 }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.stopAfter(cancel, 1)
	_ = r.Run(ctx)
	fi, _ := os.Stat(e.tokFile)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
}

func b64d(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAssertionClaimsAndSignature(t *testing.T) {
	e := newEnv(t)
	var form map[string][]string
	e.handler.Store(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/oauth/v2/token" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("content type %q", ct)
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		_ = r.ParseForm()
		form = r.PostForm
		_, _ = w.Write([]byte(`{"access_token":"t.o.k","token_type":"Bearer","expires_in":100}`))
	}))
	r := e.refresher(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.stopAfter(cancel, 1)
	_ = r.Run(ctx)

	if form["grant_type"][0] != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
		t.Fatalf("grant_type %v", form["grant_type"])
	}
	if form["scope"][0] != "openid urn:zitadel:iam:org:project:id:42:aud" {
		t.Fatalf("scope %v", form["scope"])
	}
	parts := strings.Split(form["assertion"][0], ".")
	if len(parts) != 3 {
		t.Fatal("assertion is not a JWT")
	}
	var hdr map[string]string
	_ = json.Unmarshal(b64d(t, parts[0]), &hdr)
	if hdr["alg"] != "RS256" || hdr["kid"] != testKey {
		t.Fatalf("header %v", hdr)
	}
	var cl struct {
		Iss, Sub, Aud string
		Iat, Exp      int64
	}
	if err := json.Unmarshal(b64d(t, parts[1]), &cl); err != nil {
		t.Fatal(err)
	}
	if cl.Iss != testUser || cl.Sub != testUser || cl.Aud != e.srv.URL {
		t.Fatalf("claims %+v", cl)
	}
	if cl.Exp-cl.Iat != int64(assertionTTL/time.Second) || cl.Exp-cl.Iat > 3600 {
		t.Fatalf("lifetime %d", cl.Exp-cl.Iat)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&testRSA.PublicKey, crypto.SHA256, sum[:], b64d(t, parts[2])); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
}

func TestRenewsAtHalfLifetime(t *testing.T) {
	e := newEnv(t)
	e.respond(200, "tok", 1000)
	r := e.refresher(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.stopAfter(cancel, 3)
	_ = r.Run(ctx)
	s := e.clock.slept()
	if len(s) < 3 {
		t.Fatalf("sleeps %v", s)
	}
	for _, d := range s {
		if d != 500*time.Second {
			t.Fatalf("renewal at %v, want half of 1000s: %v", d, s)
		}
	}
	if e.requests.Load() != 3 {
		t.Fatalf("requests = %d, want 3", e.requests.Load())
	}
}

func TestRenewDelayClampAndJitter(t *testing.T) {
	e := newEnv(t)
	r := e.refresher(t)
	if d := r.renewDelay(12 * time.Hour); d != 6*time.Hour {
		t.Fatalf("12h lifetime -> %v, want 6h (half)", d)
	}
	if d := r.renewDelay(48 * time.Hour); d != maxRenew {
		t.Fatalf("ceiling: %v", d)
	}
	// ttl <= minRenew: the floor is min(minRenew, ttl/2), so renewal is still
	// at half the lifetime, strictly before expiry.
	if d := r.renewDelay(2 * time.Second); d != time.Second {
		t.Fatalf("short lifetime: %v, want half (1s)", d)
	}
	if d := r.renewDelay(minRenew); d != minRenew/2 {
		t.Fatalf("lifetime == minRenew: %v, want half", d)
	}
	if d := r.renewDelay(4 * time.Second); d != 2*time.Second {
		t.Fatalf("4s lifetime: %v", d)
	}
	if d := r.renewDelay(20 * time.Second); d != 10*time.Second {
		t.Fatalf("20s lifetime: %v", d)
	}
	r.jitter = func() float64 { return 0.999 }
	d := r.renewDelay(1000 * time.Second)
	if d >= 500*time.Second || d < 450*time.Second {
		t.Fatalf("jittered delay %v not in [450s,500s)", d)
	}
}

func TestFailureKeepsTokenUntilExpiryThenClears(t *testing.T) {
	e := newEnv(t)
	e.respond(200, "good", 100) // renew at 50s
	r := e.refresher(t)
	failing := false
	e.clock.onSlp = func(n int) {
		if n == 1 { // first sleep done: the renewal at 50s is due; from now the issuer fails
			failing = true
			e.respond(500, "", 0)
		}
		if failing {
			// while failing and before expiry the old token must still be on disk
			if elapsed := e.clock.Now(); elapsed.Before(e.startPlus(100 - 5)) {
				if got, ok := e.token(); !ok || got != "good" {
					t.Errorf("token dropped before expiry: %q %v", got, ok)
				}
			}
		}
	}
	e.start = e.clock.Now()
	err := r.Run(context.Background())
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("Run = %v, want ErrExpired", err)
	}
	if _, ok := e.token(); ok {
		t.Fatal("expired token file still present")
	}
	if _, err := os.Stat(e.readyF); err == nil {
		t.Fatal("ready file still present")
	}
	// cleared exactly at expiry (lifetime minus the 5s skew), not at a later retry boundary
	if end := e.clock.Now(); !end.Equal(e.startPlus(95)) {
		t.Fatalf("cleared at %v, want expiry %v", end.Sub(e.start), 95*time.Second)
	}
}

func TestFirstFetchFailureNeverWritesAndBacksOff(t *testing.T) {
	e := newEnv(t)
	e.respond(503, "", 0)
	r := e.refresher(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.stopAfter(cancel, 6)
	_ = r.Run(ctx)
	if _, ok := e.token(); ok {
		t.Fatal("token file written after failures")
	}
	s := e.clock.slept()
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second} // jitter 0 -> half step
	for i, w := range want {
		if s[i] != w {
			t.Fatalf("backoff %v, want %v", s, want)
		}
	}
	// cap
	r.jitter = func() float64 { return 0.999999 }
	if d := r.retryDelay(40); d > backoffMax || d < backoffMax*9/10 {
		t.Fatalf("capped retry delay %v", d)
	}
}

func TestStartRemovesStaleTokenFile(t *testing.T) {
	e := newEnv(t)
	if err := os.WriteFile(e.tokFile, []byte("stale-token"), 0o644); err != nil { //nolint:gosec // test file
		t.Fatal(err)
	}
	e.respond(500, "", 0)
	r := e.refresher(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.stopAfter(cancel, 2)
	_ = r.Run(ctx)
	if got, ok := e.token(); ok {
		t.Fatalf("stale token survived start: %q", got)
	}
}

func TestBadResponsesNeverWrite(t *testing.T) {
	cases := map[string]func(http.ResponseWriter, *http.Request){
		"400": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		},
		"500 with token": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"access_token":"a.b.c","token_type":"Bearer","expires_in":100}`))
		},
		"401":     func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) },
		"500":     func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) },
		"garbage": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>nope")) },
		"empty token": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"access_token":"","expires_in":100}`))
		},
		"no token":   func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"expires_in":100}`)) },
		"no expires": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"access_token":"a.b.c"}`)) },
		"zero expires": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"access_token":"a.b.c","expires_in":0}`))
		},
		"string expires": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"access_token":"a.b.c","expires_in":"100"}`))
		},
		"wrong type": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"access_token":"a.b.c","token_type":"mac","expires_in":100}`))
		},
		"bad chars": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"access_token":"a b\nc","expires_in":100}`))
		},
		"oversize": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"access_token":"a.b.c","expires_in":100,"pad":"` + strings.Repeat("x", maxResponse) + `"}`))
		},
		"huge token": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"access_token":"` + strings.Repeat("a", maxTokenBytes+1) + `","expires_in":100}`))
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.handler.Store(http.HandlerFunc(h))
			r := e.refresher(t)
			ctx, cancel := context.WithCancel(context.Background())
			e.stopAfter(cancel, 2)
			_ = r.Run(ctx)
			if e.requests.Load() == 0 {
				t.Fatal("no request made")
			}
			if got, ok := e.token(); ok {
				t.Fatalf("token file written: %q", got)
			}
			if _, err := os.Stat(e.readyF); err == nil {
				t.Fatal("ready file written")
			}
		})
	}
}

func TestRedirectRefused(t *testing.T) {
	e := newEnv(t)
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed.Store(true)
		_, _ = w.Write([]byte(`{"access_token":"evil.tok.en","expires_in":100}`))
	}))
	defer target.Close()
	e.handler.Store(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	r := e.refresher(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.stopAfter(cancel, 1)
	_ = r.Run(ctx)
	if followed.Load() {
		t.Fatal("redirect was followed (assertion sent to the redirect target)")
	}
	if _, ok := e.token(); ok {
		t.Fatal("token written from a redirect")
	}
}

// captureStd redirects os.Stdout and os.Stderr into one pipe for the duration
// of fn and returns what was written.
func captureStd(t *testing.T, fn func()) string {
	t.Helper()
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = wr, wr
	done := make(chan string, 1)
	go func() {
		var b bytes.Buffer
		_, _ = b.ReadFrom(rd)
		done <- b.String()
	}()
	func() {
		defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
		fn()
	}()
	_ = wr.Close()
	return <-done
}

// TestErrorsAndLogsNeverLeakSecrets runs a successful refresh, a failing
// refresh (500 with a secret body) and an expiry, and asserts that neither
// the access token, the signed assertions, the response body nor key
// material reach logs, errors, stdout or stderr.
func TestErrorsAndLogsNeverLeakSecrets(t *testing.T) {
	e := newEnv(t)
	const bodySecret = "SECRET-BODY-TOKEN-xyz"
	const accessSecret = "ACCESS-SECRET-tok.abc123.def456"
	var mu sync.Mutex
	var assertions []string
	var calls int
	e.handler.Store(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		_ = r.ParseForm()
		mu.Lock()
		assertions = append(assertions, r.PostForm.Get("assertion"))
		calls++
		n := calls
		mu.Unlock()
		if n == 1 { // success
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": accessSecret, "token_type": "Bearer", "expires_in": 100})
			return
		}
		w.WriteHeader(500)
		_, _ = w.Write([]byte(bodySecret))
	}))
	r := e.refresher(t)
	var runErr error
	stdout := captureStd(t, func() {
		runErr = r.Run(context.Background()) // success, failed renewals, expiry
	})
	if !errors.Is(runErr, ErrExpired) {
		t.Fatalf("Run = %v, want ErrExpired", runErr)
	}
	// direct failure and bad-response errors too
	_, _, _, ferr := r.fetch(context.Background())
	if ferr == nil {
		t.Fatal("fetch should fail against the 500 handler")
	}
	mu.Lock()
	if len(assertions) < 2 || assertions[0] == "" {
		mu.Unlock()
		t.Fatalf("test did not capture assertions: %d", len(assertions))
	}
	forbidden := append([]string(nil), assertions...)
	mu.Unlock()
	for _, a := range forbidden { // also each assertion's signature segment
		if parts := strings.Split(a, "."); len(parts) == 3 {
			forbidden = append(forbidden, parts[2][:24])
		}
	}
	pemText := string(pkcs1PEM(testRSA))
	forbidden = append(forbidden, bodySecret, accessSecret, "PRIVATE KEY", "BEGIN", testKey, testUser,
		base64.StdEncoding.EncodeToString(testRSA.D.Bytes())[:16],
		base64.RawURLEncoding.EncodeToString(testRSA.D.Bytes())[:16],
		base64.StdEncoding.EncodeToString(testRSA.N.Bytes())[:16])
	for _, ln := range strings.Split(pemText, "\n") {
		if len(ln) >= 20 && !strings.HasPrefix(ln, "-----") {
			forbidden = append(forbidden, ln[:20])
			break
		}
	}
	out := e.logBuf.String() + stdout + ferr.Error() + runErr.Error()
	if !strings.Contains(e.logBuf.String(), "token refreshed") {
		t.Fatal("success path was not logged; the test would prove nothing")
	}
	for _, s := range forbidden {
		if s != "" && strings.Contains(out, s) {
			t.Fatalf("output leaks %q", s)
		}
	}
}

// TestDefaultTransportIgnoresProxyEnvironment proves the assertion goes
// straight to the issuer even when proxy variables are set.
func TestDefaultTransportIgnoresProxyEnvironment(t *testing.T) {
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxied.Add(1)
		w.WriteHeader(502)
	}))
	t.Cleanup(proxy.Close)
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy", "ALL_PROXY", "NO_PROXY", "no_proxy"} {
		t.Setenv(k, "")
	}
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	e := newEnv(t)
	e.respond(200, "direct-token", 100)
	// production default transport: no Options.Client
	r := e.refresher(t)
	tr, ok := r.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("default transport is %T", r.client.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("default transport must not use a proxy")
	}
	// Go exempts loopback from env proxies, so the structural check above is
	// the one that catches a ProxyFromEnvironment regression; this confirms
	// the request still reaches the issuer directly.
	tok, _, _, err := r.fetch(context.Background())
	if err != nil || tok != "direct-token" {
		t.Fatalf("fetch = %q, %v", tok, err)
	}
	if proxied.Load() != 0 {
		t.Fatal("request went through the proxy")
	}
}

// TestExpiryCountsFromBeforeTheRequest: a slow response must not extend the
// token's life on disk.
func TestExpiryCountsFromBeforeTheRequest(t *testing.T) {
	e := newEnv(t)
	e.start = e.clock.Now()
	var calls atomic.Int32
	e.handler.Store(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			e.clock.mu.Lock()
			e.clock.now = e.clock.now.Add(30 * time.Second) // slow response
			e.clock.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "slow", "token_type": "Bearer", "expires_in": 100})
			return
		}
		w.WriteHeader(500)
	}))
	r := e.refresher(t)
	if err := r.Run(context.Background()); !errors.Is(err, ErrExpired) {
		t.Fatalf("Run = %v", err)
	}
	// expiry = request start + 100s - 5s skew, NOT receipt (+30s) + 100s
	if end := e.clock.Now(); !end.Equal(e.startPlus(95)) {
		t.Fatalf("cleared at +%v, want +95s", end.Sub(e.start))
	}
}

func TestShortLifetimeRenewsBeforeExpiry(t *testing.T) {
	e := newEnv(t)
	e.respond(200, "short", 4)
	r := e.refresher(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.stopAfter(cancel, 2)
	if err := r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v (token must be renewed, not expire)", err)
	}
	if e.requests.Load() < 2 {
		t.Fatalf("requests = %d, want a renewal before expiry", e.requests.Load())
	}
}

func TestKeyFileErrorsFailFastWithoutLeaking(t *testing.T) {
	good := pkcs1PEM(testRSA)
	small, _ := rsa.GenerateKey(rand.Reader, 1024) //nolint:gosec // deliberately short, to prove it is refused
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(testRSA)
	cases := map[string][]byte{
		"missing":     nil,
		"not pem":     []byte("SUPERSECRETKEYTEXT"),
		"bad der":     pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("SUPERSECRETKEYTEXT")}),
		"short key":   pkcs1PEM(small),
		"bad json":    []byte(`{"key": SUPERSECRETKEYTEXT`),
		"json no key": []byte(`{"userId":"u","keyId":"k"}`),
		"oversize":    bytes.Repeat([]byte("A"), maxKeyFile+10),
		"wrong type":  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: good}),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			if content == nil {
				_ = os.Remove(e.keyFile)
			} else if err := os.WriteFile(e.keyFile, content, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := New(e.cfg(), Options{})
			var ce *ConfigError
			if !errors.As(err, &ce) {
				t.Fatalf("New = %v, want ConfigError", err)
			}
			if strings.Contains(err.Error(), "SUPERSECRET") || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("error leaks key material: %v", err)
			}
		})
	}
	t.Run("pkcs8 ok", func(t *testing.T) {
		e := newEnv(t)
		_ = os.WriteFile(e.keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), 0o600)
		if _, err := New(e.cfg(), Options{}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("zitadel json ok, ids from file", func(t *testing.T) {
		e := newEnv(t)
		doc, _ := json.Marshal(map[string]string{"type": "serviceaccount", "userId": "u9", "keyId": "k9", "key": string(good)})
		_ = os.WriteFile(e.keyFile, doc, 0o600)
		cfg := e.cfg()
		cfg.UserID, cfg.KeyID = "", ""
		r, err := New(cfg, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if r.cfg.UserID != "u9" || r.cfg.KeyID != "k9" {
			t.Fatalf("ids %q %q", r.cfg.UserID, r.cfg.KeyID)
		}
	})
	t.Run("ids disagree with json", func(t *testing.T) {
		e := newEnv(t)
		doc, _ := json.Marshal(map[string]string{"userId": "other", "keyId": testKey, "key": string(good)})
		_ = os.WriteFile(e.keyFile, doc, 0o600)
		if _, err := New(e.cfg(), Options{}); err == nil {
			t.Fatal("accepted conflicting user id")
		}
	})
	t.Run("pem without ids", func(t *testing.T) {
		e := newEnv(t)
		cfg := e.cfg()
		cfg.UserID = ""
		if _, err := New(cfg, Options{}); err == nil {
			t.Fatal("accepted missing user id")
		}
	})
}

func TestConfigRefusals(t *testing.T) {
	e := newEnv(t)
	mut := map[string]func(*Config){
		"http issuer":       func(c *Config) { c.AllowLoopbackHTTP = false },
		"http non-loopback": func(c *Config) { c.Issuer = "http://example.com" },
		"ftp":               func(c *Config) { c.Issuer = "ftp://x" },
		"issuer path":       func(c *Config) { c.Issuer = "https://auth.example/x" },
		"issuer userinfo":   func(c *Config) { c.Issuer = "https://u:p@auth.example" },
		"no scope":          func(c *Config) { c.Scope = "" },
		"relative token":    func(c *Config) { c.TokenFile = "token" },
		"unclean token":     func(c *Config) { c.TokenFile = "/a/../b" },
		"ready is token":    func(c *Config) { c.ReadyFile = c.TokenFile },
		"no key file":       func(c *Config) { c.KeyFile = "" },
		"setuid mode":       func(c *Config) { c.FileMode = 0o4755 },
	}
	for name, m := range mut {
		t.Run(name, func(t *testing.T) {
			cfg := e.cfg()
			m(&cfg)
			_, err := New(cfg, Options{})
			var ce *ConfigError
			if !errors.As(err, &ce) {
				t.Fatalf("New = %v, want ConfigError", err)
			}
		})
	}
	cfg := e.cfg()
	cfg.Issuer = "https://auth.example.com/"
	if _, err := New(cfg, Options{}); err != nil {
		t.Fatalf("trailing slash https issuer refused: %v", err)
	}
}

func TestAudienceIsIssuerWithoutSlash(t *testing.T) {
	e := newEnv(t)
	cfg := e.cfg()
	cfg.Issuer = e.srv.URL + "/"
	r, err := New(cfg, Options{Clock: e.clock})
	if err != nil {
		t.Fatal(err)
	}
	if r.cfg.Issuer != e.srv.URL || r.tokURL != e.srv.URL+"/oauth/v2/token" {
		t.Fatalf("issuer %q token url %q", r.cfg.Issuer, r.tokURL)
	}
}

func TestConcurrentReadersNeverSeePartialToken(t *testing.T) {
	e := newEnv(t)
	var n atomic.Int32
	e.handler.Store(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tok := strings.Repeat(string(rune('a'+n.Add(1)%26)), 3000) // long, uniform, easy to spot tearing
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "token_type": "Bearer", "expires_in": 100})
	}))
	r := e.refresher(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.stopAfter(cancel, 300)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var bad atomic.Int32
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				b, err := os.ReadFile(e.tokFile)
				if err != nil {
					continue // not yet written is fine; absent is not a partial token
				}
				if len(b) != 3000 || bytes.Count(b, b[:1]) != 3000 {
					bad.Add(1)
				}
			}
		}()
	}
	_ = r.Run(ctx)
	close(stop)
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d torn reads", bad.Load())
	}
}

func TestSignalStopsRunWithoutClearingNothingElse(t *testing.T) {
	e := newEnv(t)
	r := e.refresher(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.stopAfter(cancel, 1)
	if err := r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
}

func TestAttemptCannotOutliveToken(t *testing.T) {
	// A hanging issuer must not keep an expired token valid: the attempt is
	// cut at the token's expiry and the file is cleared then.
	e := newEnv(t)
	r := e.refresher(t)
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	first := true
	e.handler.Store(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if first {
			first = false
			_, _ = w.Write([]byte(`{"access_token":"good","token_type":"Bearer","expires_in":8}`))
			return
		}
		<-hang
	}))
	// real clock for the hanging attempt so the deadline is exercised
	r.clock = realTimeClock{}
	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrExpired) {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("expired token still valid while the issuer hangs")
	}
	if _, ok := e.token(); ok {
		t.Fatal("token file still present")
	}
}

type realTimeClock struct{ realClock }
