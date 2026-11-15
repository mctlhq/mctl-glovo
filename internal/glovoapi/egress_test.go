package glovoapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// testProxy is a forward proxy for tests. It tunnels CONNECT and forwards
// absolute-form plain HTTP requests, and records both. Forwarded plain
// requests get a marker header so the upstream can tell them from direct
// ones; tunnelled connections are recognised by their source address.
type testProxy struct {
	srv *httptest.Server

	mu      sync.Mutex
	seen    []string // "CONNECT host:port" or "METHOD absolute-url"
	dialled map[string]bool
}

const viaProxyHeader = "X-Test-Via-Proxy"

func newTestProxy(t *testing.T) *testProxy {
	t.Helper()
	p := &testProxy{dialled: map[string]bool{}}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *testProxy) url(t *testing.T) *url.URL {
	t.Helper()
	u, err := url.Parse(p.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (p *testProxy) record(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, s)
}

func (p *testProxy) log() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

func (p *testProxy) dialledFrom(addr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dialled[addr]
}

func (p *testProxy) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.record("CONNECT " + r.Host)
		up, err := net.Dial("tcp", r.Host) // #nosec G704 -- test proxy, loopback upstreams only
		if err != nil {
			http.Error(w, "dial", http.StatusBadGateway)
			return
		}
		p.mu.Lock()
		p.dialled[up.LocalAddr().String()] = true
		p.mu.Unlock()
		hj, ok := w.(http.Hijacker)
		if !ok {
			_ = up.Close()
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		down, buf, err := hj.Hijack()
		if err != nil {
			_ = up.Close()
			return
		}
		_, _ = io.WriteString(down, "HTTP/1.1 200 Connection established\r\n\r\n")
		go func() {
			_, _ = io.Copy(up, buf)
			_ = up.Close()
		}()
		go func() {
			_, _ = io.Copy(down, up)
			_ = down.Close()
		}()
		return
	}
	if !r.URL.IsAbs() {
		http.Error(w, "not a proxy request", http.StatusBadRequest)
		return
	}
	p.record(r.Method + " " + r.URL.String())
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Header.Set(viaProxyHeader, "1")
	resp, err := (&http.Transport{Proxy: nil}).RoundTrip(out)
	if err != nil {
		http.Error(w, "upstream", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// newFakeTLS is newFake over TLS. remotes records the source address of
// every connection the upstream accepted.
func newFakeTLS(t *testing.T) (*fakeGlovo, func() []string) {
	t.Helper()
	f := &fakeGlovo{t: t, replies: map[string][]reply{}}
	var mu sync.Mutex
	var remotes []string
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateNew {
			mu.Lock()
			remotes = append(remotes, c.RemoteAddr().String())
			mu.Unlock()
		}
	}
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), remotes...)
	}
}

// newEgressClient is newTestClient without WithHTTPClient, so the client
// builds its own transport.
func newEgressClient(t *testing.T, baseURL string, s Session, opts ...Option) (*Client, error) {
	t.Helper()
	path := writeSessionFile(t, s)
	all := append([]Option{WithBaseURL(baseURL), WithDefaultLocation(testLocation())}, opts...)
	c, err := New(path, all...)
	if err != nil {
		return nil, err
	}
	c.now = func() time.Time { return testNow }
	var idMu sync.Mutex
	ids := 0
	c.newID = func() string {
		idMu.Lock()
		defer idMu.Unlock()
		ids++
		return fmt.Sprintf("00000000-0000-4000-8000-%012d", ids)
	}
	c.sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	c.initBits()
	if err := c.Reload(); err != nil {
		t.Fatal(err)
	}
	return c, nil
}

func expiredSession() Session {
	s := testSession()
	s["accessToken"] = expiredToken
	return s
}

func refreshReply() reply {
	return reply{200, fmt.Sprintf(`{"accessToken":%q}`, rotatedToken)}
}

// The proxy carries every request in absolute form over plain HTTP: the
// pre-request refresh, the call, the 401, the refresh after it and the retry.
func TestProxyCarriesEveryRequestPlainHTTP(t *testing.T) {
	f := newFake(t)
	p := newTestProxy(t)
	c, err := newEgressClient(t, f.srv.URL, expiredSession(), WithProxy(p.url(t)))
	if err != nil {
		t.Fatal(err)
	}
	f.on("POST", "/oauth/refresh", refreshReply())
	f.on("GET", "/v3/customer/orders/9001", reply{401, `{}`}, reply{200, `{"id":9001}`})
	if _, err := c.GetOrder(context.Background(), "9001"); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) != 4 || f.count("POST", "/oauth/refresh") != 2 {
		t.Fatalf("upstream saw %d requests (%d refreshes), want refresh, order, refresh, order", len(reqs), f.count("POST", "/oauth/refresh"))
	}
	for _, r := range reqs {
		if r.Header.Get(viaProxyHeader) != "1" {
			t.Errorf("%s %s reached the upstream without the proxy", r.Method, r.URI)
		}
	}
	log := p.log()
	if len(log) != len(reqs) {
		t.Fatalf("proxy saw %d requests, upstream %d: %v", len(log), len(reqs), log)
	}
	for i, want := range []string{"POST " + f.srv.URL + "/oauth/refresh", "GET " + f.srv.URL + "/v3/customer/orders/9001"} {
		if log[i] != want {
			t.Errorf("proxy log[%d] = %q, want %q", i, log[i], want)
		}
	}
}

// Over TLS the client tunnels through CONNECT, and every connection the
// upstream accepted was opened by the proxy.
func TestProxyTunnelsTLSWithConnect(t *testing.T) {
	f, remotes := newFakeTLS(t)
	p := newTestProxy(t)
	pool := x509.NewCertPool()
	pool.AddCert(f.srv.Certificate())
	c, err := newEgressClient(t, f.srv.URL, expiredSession(),
		WithProxy(p.url(t)), WithRequireProxy(),
		withProxyTLSConfig(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}))
	if err != nil {
		t.Fatal(err)
	}
	f.on("POST", "/oauth/refresh", refreshReply())
	if _, err := c.GetBaskets(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.count("POST", "/oauth/refresh"); n != 2 {
		t.Fatalf("%d refreshes reached the upstream, want 2", n)
	}
	host := strings.TrimPrefix(f.srv.URL, "https://")
	log := p.log()
	if len(log) == 0 {
		t.Fatal("proxy saw nothing")
	}
	for _, l := range log {
		if l != "CONNECT "+host {
			t.Errorf("proxy log %q, want only CONNECT %s", l, host)
		}
	}
	rs := remotes()
	if len(rs) == 0 {
		t.Fatal("upstream recorded no connections")
	}
	for _, r := range rs {
		if !p.dialledFrom(r) {
			t.Errorf("upstream connection from %s was not opened by the proxy", r)
		}
	}
}

// HTTPS_PROXY, HTTP_PROXY and NO_PROXY never override the explicit proxy.
func TestExplicitProxyBeatsEnvironment(t *testing.T) {
	f := newFake(t)
	p := newTestProxy(t)
	decoy := newTestProxy(t)
	t.Setenv("HTTPS_PROXY", decoy.srv.URL)
	t.Setenv("HTTP_PROXY", decoy.srv.URL)
	t.Setenv("NO_PROXY", "*")
	t.Setenv("no_proxy", "*")
	c, err := newEgressClient(t, f.srv.URL, expiredSession(), WithProxy(p.url(t)))
	if err != nil {
		t.Fatal(err)
	}
	f.on("POST", "/oauth/refresh", refreshReply())
	if _, err := c.GetOrders(context.Background(), nil, 0); err != nil {
		t.Fatal(err)
	}
	if n := len(p.log()); n != 2 {
		t.Errorf("explicit proxy saw %d requests, want refresh and orders", n)
	}
	if n := len(decoy.log()); n != 0 {
		t.Errorf("environment proxy saw %d requests", n)
	}
	for _, r := range f.requests() {
		if r.Header.Get(viaProxyHeader) != "1" {
			t.Errorf("%s %s went around the proxy", r.Method, r.URI)
		}
	}
}

func TestRequireProxyConstructorErrors(t *testing.T) {
	proxy, _ := url.Parse("http://127.0.0.1:3128")
	path := writeSessionFile(t, nil)
	cases := []struct {
		name string
		opts []Option
		is   error
	}{
		{"no proxy", []Option{WithRequireProxy()}, ErrProxyRequired},
		{"no proxy, custom client", []Option{WithRequireProxy(), WithHTTPClient(&http.Client{})}, nil},
		{"proxy and custom client", []Option{WithProxy(proxy), WithHTTPClient(&http.Client{})}, nil},
		{"http base", []Option{WithRequireProxy(), WithProxy(proxy), WithBaseURL("http://api.glovoapp.com")}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(path, tc.opts...)
			if err == nil || c != nil {
				t.Fatalf("New = %v, %v; want an error", c, err)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Errorf("error %v, want %v", err, tc.is)
			}
		})
	}
	if _, err := New(path, WithRequireProxy(), WithProxy(proxy)); err != nil {
		t.Errorf("require mode with a proxy and the default https base: %v", err)
	}
	if _, err := New(path); err != nil {
		t.Errorf("direct mode (glovo-home): %v", err)
	}
}

func TestProxyURLValidation(t *testing.T) {
	path := writeSessionFile(t, nil)
	bad := []string{
		"https://127.0.0.1:3128",
		"socks5://127.0.0.1:1080",
		"http://user:pass@127.0.0.1:3128",
		"http://user@127.0.0.1:3128",
		"http://:3128",
		"http://127.0.0.1",
		"http://127.0.0.1:3128/path",
		"http://127.0.0.1:3128?x=1",
		"http://127.0.0.1:3128#frag",
		"127.0.0.1:3128",
	}
	for _, raw := range bad {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if c, err := New(path, WithProxy(u)); err == nil || c != nil {
			t.Errorf("WithProxy(%q) accepted", raw)
		}
	}
	if _, err := New(path, WithProxy(nil)); err == nil {
		t.Error("WithProxy(nil) accepted")
	}
	for _, raw := range []string{"http://127.0.0.1:3128", "http://127.0.0.1:3128/", "http://[fd7a::1]:3128"} {
		u, _ := url.Parse(raw)
		if _, err := New(path, WithProxy(u)); err != nil {
			t.Errorf("WithProxy(%q): %v", raw, err)
		}
	}
}

// With the proxy down every call fails, refresh included, and the upstream
// is never contacted. Errors keep the redacted path only.
func TestProxyDownFailsClosed(t *testing.T) {
	f := newFake(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL, _ := url.Parse(dead.URL)
	dead.Close()
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("NO_PROXY", "*")

	c, err := newEgressClient(t, f.srv.URL, testSession(), WithProxy(deadURL))
	if err != nil {
		t.Fatal(err)
	}
	f.on("POST", "/oauth/refresh", refreshReply())
	ctx := context.Background()
	_, err = c.GetBaskets(ctx)
	if err == nil {
		t.Fatal("GetBaskets succeeded with the proxy down")
	}
	if strings.Contains(err.Error(), "4242") {
		t.Errorf("error leaks the customer id: %v", err)
	}
	if err := c.Refresh(ctx); err == nil {
		t.Error("Refresh succeeded with the proxy down")
	}
	expired, err := newEgressClient(t, f.srv.URL, expiredSession(), WithProxy(deadURL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := expired.GetOrders(ctx, nil, 0); err == nil {
		t.Error("pre-request refresh succeeded with the proxy down")
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("upstream contacted directly %d times", n)
	}
}

// hostGuard is a transport that refuses, and records, any request to a host
// other than the base URL's.
type hostGuard struct {
	t    *testing.T
	host string
	next http.RoundTripper

	mu   sync.Mutex
	seen int
}

func (g *hostGuard) RoundTrip(r *http.Request) (*http.Response, error) {
	g.mu.Lock()
	g.seen++
	g.mu.Unlock()
	if r.URL.Host != g.host {
		g.t.Errorf("request to %s %s, outside the base URL host %s", r.Method, r.URL.Host, g.host)
		return nil, errors.New("hostGuard: off-origin request")
	}
	return g.next.RoundTrip(r)
}

// Every request the client sends, refreshes included, goes to the base URL
// host through the client's one http.Client.
func TestEveryRequestStaysOnBaseURL(t *testing.T) {
	f := newFake(t)
	base, _ := url.Parse(f.srv.URL)
	g := &hostGuard{t: t, host: base.Host, next: f.srv.Client().Transport}
	path := writeSessionFile(t, expiredSession())
	c, err := New(path, WithBaseURL(f.srv.URL), WithHTTPClient(&http.Client{Transport: g}), WithDefaultLocation(testLocation()))
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return testNow }
	if err := c.Reload(); err != nil {
		t.Fatal(err)
	}
	f.on("POST", "/oauth/refresh", refreshReply())
	f.on("GET", "/v3/customer/orders/9001", reply{401, `{}`}, reply{200, `{"id":9001}`})
	ctx := context.Background()
	calls := []func() error{
		func() error { _, err := c.GetOrders(ctx, nil, 0); return err },
		func() error { _, err := c.GetOrder(ctx, "9001"); return err },
		func() error { _, err := c.GetBaskets(ctx); return err },
		func() error { _, err := c.SearchStores(ctx, "pizza"); return err },
		func() error { _, err := c.GetSavedLocations(ctx); return err },
		func() error {
			_, err := c.GetStoreCatalog(ctx, "1", "2", f.srv.URL+"/v3/stores/1/addresses/2/content/x", true)
			return err
		},
		func() error { return c.Refresh(ctx) },
	}
	for i, call := range calls {
		if err := call(); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if g.seen != len(f.requests()) || g.seen < len(calls)+2 {
		t.Errorf("guard saw %d requests, upstream %d", g.seen, len(f.requests()))
	}
	if f.count("POST", "/oauth/refresh") != 3 {
		t.Errorf("%d refreshes, want pre-request, after-401 and explicit", f.count("POST", "/oauth/refresh"))
	}
}

// A redirect off the API origin, or one that would carry a mutation to a
// path outside the allowlist, is refused and never followed.
func TestRedirectsStayOnOriginAndAllowlist(t *testing.T) {
	var evilHits int
	var evilMu sync.Mutex
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		evilMu.Lock()
		evilHits++
		evilMu.Unlock()
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(evil.Close)

	var upMu sync.Mutex
	var upPaths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upMu.Lock()
		upPaths = append(upPaths, r.Method+" "+r.URL.Path)
		upMu.Unlock()
		switch r.URL.Path {
		case "/v3/customer/orders/1":
			http.Redirect(w, r, evil.URL+"/steal", http.StatusFound)
		case "/v1/authenticated/customers/4242/baskets":
			http.Redirect(w, r, "/v1/checkout", http.StatusTemporaryRedirect)
		case "/v3/customer/orders/2":
			http.Redirect(w, r, "/v3/customer/orders/3", http.StatusFound)
		default:
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	}))
	t.Cleanup(up.Close)

	c, err := newEgressClient(t, up.URL, testSession())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.GetOrder(ctx, "1"); !errors.Is(err, ErrRedirectRefused) {
		t.Errorf("off-origin redirect: %v, want ErrRedirectRefused", err)
	}
	if _, err := c.call(ctx, "/v1/authenticated/customers/4242/baskets", callOpts{method: http.MethodPost, auth: true, body: map[string]any{}}); !errors.Is(err, ErrRedirectRefused) {
		t.Errorf("307 to a refused mutation: %v, want ErrRedirectRefused", err)
	}
	if _, err := c.GetOrder(ctx, "2"); err != nil {
		t.Errorf("same-origin GET redirect: %v", err)
	}
	evilMu.Lock()
	defer evilMu.Unlock()
	if evilHits != 0 {
		t.Errorf("redirect target off the origin was contacted %d times", evilHits)
	}
	upMu.Lock()
	defer upMu.Unlock()
	for _, p := range upPaths {
		if strings.Contains(p, "checkout") {
			t.Errorf("redirected mutation reached %s", p)
		}
	}
}
