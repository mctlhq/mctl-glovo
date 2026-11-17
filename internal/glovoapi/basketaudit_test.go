package glovoapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// hitLog records every request an upstream got.
type hitLog struct {
	mu   sync.Mutex
	hits []string // "METHOD path proto"
}

func (h *hitLog) add(r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hits = append(h.hits, r.Method+" "+r.URL.EscapedPath()+" "+r.Proto)
}

func (h *hitLog) all() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.hits...)
}

// basketWrites are the hits that changed a basket, by any spelling of the
// path (case, encoding).
func (h *hitLog) basketWrites() []string {
	var out []string
	for _, x := range h.all() {
		f := strings.Fields(x)
		p := strings.ToLower(f[1])
		if f[0] != http.MethodGet && (strings.Contains(p, "basket") || strings.Contains(p, "%62asket")) {
			out = append(out, x)
		}
	}
	return out
}

type redir struct {
	status   int
	location string
}

// redirector is an upstream that answers chosen paths with a redirect.
type redirector struct {
	up     *httptest.Server
	log    *hitLog
	mu     sync.Mutex
	routes map[string]redir
}

func newRedirector(t *testing.T, h2 bool) *redirector {
	t.Helper()
	r := &redirector{log: &hitLog{}, routes: map[string]redir{}}
	r.up = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.log.add(req)
		r.mu.Lock()
		rd, ok := r.routes[req.URL.Path]
		r.mu.Unlock()
		if ok {
			w.Header().Set("Location", rd.location)
			w.WriteHeader(rd.status)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	if h2 {
		r.up.EnableHTTP2 = true
		r.up.StartTLS()
	} else {
		r.up.Start()
	}
	t.Cleanup(r.up.Close)
	return r
}

func (r *redirector) route(path string, status int, location string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes[path] = redir{status, location}
}

const (
	customerBaskets = "/v1/authenticated/customers/4242/baskets"
	searchPath      = "/v1/web/store_wall/search"
	categoryPath    = "/v1/web/store_wall/category/1"
)

// Every redirect status, every way to spell the target, same host and
// cross host, one hop and a chain, HTTP/1.1 and HTTP/2: a default hosted
// client never lets a basket change reach any upstream, and nothing leaves
// the API origin. A 301/302/303 turns the POST into a GET (net/http), which
// is a read; a 307/308 keeps the POST and must be refused at the hop.
func TestHostedRedirectsNeverReachABasketWrite(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		proto := "http1"
		if h2 {
			proto = "h2"
		}
		for _, status := range []int{301, 302, 303, 307, 308} {
			for _, target := range []string{"abs", "rel", "dots", "query", "encoded", "upper", "deep", "cross", "chain"} {
				t.Run(proto+"/"+strconv.Itoa(status)+"/"+target, func(t *testing.T) {
					r := newRedirector(t, h2)
					other := newRedirector(t, h2)
					loc := map[string]string{
						"abs":     r.up.URL + customerBaskets,
						"rel":     customerBaskets,
						"dots":    "/v1/web/store_wall/../../authenticated/customers/4242/baskets",
						"query":   customerBaskets + "?x=1",
						"encoded": "/v1/authenticated/customers/4242/%62askets",
						"upper":   "/V1/AUTHENTICATED/CUSTOMERS/4242/BASKETS",
						"deep":    customerBaskets + "/b1/products",
						"cross":   other.up.URL + customerBaskets,
						"chain":   categoryPath,
					}[target]
					r.route(searchPath, status, loc)
					if target == "chain" {
						// search -> category -> baskets, the same status twice
						r.route(categoryPath, status, customerBaskets)
					}
					c := hostedBasketClient(t, &fakeGlovo{srv: r.up}, false)
					_, err := c.SearchStores(context.Background(), "milk")
					if w := r.log.basketWrites(); len(w) != 0 {
						t.Fatalf("a basket write reached the upstream: %v (err %v)", w, err)
					}
					if h := other.log.all(); len(h) != 0 {
						t.Fatalf("a request left the API origin: %v", h)
					}
					if status == 307 || status == 308 {
						if !errors.Is(err, ErrRedirectRefused) {
							t.Fatalf("err = %v, want ErrRedirectRefused (the POST survives a %d)", err, status)
						}
					}
					if h2 {
						for _, h := range r.log.all() {
							if !strings.HasSuffix(h, "HTTP/2.0") {
								t.Fatalf("not an HTTP/2 hit: %q", h)
							}
						}
					}
				})
			}
		}
	}
}

// The control for the matrix above: with the capability on, the same 307 does
// carry the POST to the same-host basket route (so the matrix can tell a
// refusal from a broken flow), and a cross-host hop is still refused.
func TestRedirectMatrixControlWithBasketWritesOn(t *testing.T) {
	r := newRedirector(t, false)
	r.route(searchPath, http.StatusTemporaryRedirect, r.up.URL+customerBaskets)
	c := hostedBasketClient(t, &fakeGlovo{srv: r.up}, true)
	if _, err := c.SearchStores(context.Background(), "milk"); err != nil {
		t.Fatalf("same-host 307 with writes on: %v", err)
	}
	if n := len(r.log.basketWrites()); n != 1 {
		t.Fatalf("with writes on %d basket write(s) arrived, want 1: %v", n, r.log.all())
	}

	r2 := newRedirector(t, false)
	other := newRedirector(t, false)
	r2.route(searchPath, http.StatusTemporaryRedirect, other.up.URL+customerBaskets)
	c2 := hostedBasketClient(t, &fakeGlovo{srv: r2.up}, true)
	if _, err := c2.SearchStores(context.Background(), "milk"); !errors.Is(err, ErrRedirectRefused) {
		t.Fatalf("cross-host with writes on: %v", err)
	}
	if h := other.log.all(); len(h) != 0 {
		t.Fatalf("cross-host request went out: %v", h)
	}
}

// Every HTTP method against every spelling of a basket path, straight into
// send: refused before the network on a default hosted client. GET stays
// open (the control).
func TestHostedSendRefusesEveryMethodOnBasketPaths(t *testing.T) {
	f := newFake(t)
	c := hostedBasketClient(t, f, false)
	paths := []string{
		customerBaskets,
		customerBaskets + "/",
		customerBaskets + "/b1/products",
		customerBaskets + "/b1/products/quantity",
		customerBaskets + "?x=1",
		customerBaskets + "/stores/1001",
		"/v1/authenticated/customers/4242/Baskets",
		"/v1/authenticated/customers/4242/%62askets",
		"/v1/authenticated/customers/4242/baskets/b1/products/../products",
		"/v1/authenticated/customers//baskets",
	}
	methods := []string{"POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "TRACE", "CONNECT", "post", "put", "patch", "delete", "PROPFIND", ""}
	for _, p := range paths {
		for _, m := range methods {
			_, _, _, err := c.send(context.Background(), m, p, map[string]any{"x": 1}, true)
			if err == nil {
				t.Errorf("%q %s: sent", m, p)
			}
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("%d request(s) reached the network: %v", n, f.requests())
	}
	// The three allowlisted shapes are refused by the basket gate itself, not
	// only by the allowlist.
	for m, p := range map[string]string{
		"POST":  customerBaskets,
		"PUT":   customerBaskets + "/b1/products",
		"PATCH": customerBaskets + "/b1/products/quantity",
	} {
		if _, _, _, err := c.send(context.Background(), m, p, map[string]any{}, true); !errors.Is(err, ErrBasketWritesDisabled) {
			t.Errorf("%s %s: err = %v, want ErrBasketWritesDisabled", m, p, err)
		}
	}
	// Control: a GET of a basket goes out.
	if _, _, _, err := c.send(context.Background(), http.MethodGet, customerBaskets, nil, true); err != nil {
		t.Fatalf("GET %s: %v", customerBaskets, err)
	}
	if n := len(f.requests()); n != 1 {
		t.Fatalf("%d request(s) after the control GET, want 1", n)
	}
}

// With the capability on only the three allowlisted shapes go out: DELETE, a
// look-alike path and every other method still do not.
func TestHostedSendWithBasketWritesOnStaysOnTheAllowlist(t *testing.T) {
	f := newFake(t)
	c := hostedBasketClient(t, f, true)
	for _, bad := range []struct{ m, p string }{
		{"DELETE", customerBaskets},
		{"DELETE", customerBaskets + "/b1/products"},
		{"POST", customerBaskets + "/b1/products"},
		{"PUT", customerBaskets},
		{"PATCH", customerBaskets + "/b1/products"},
		{"POST", "/v1/authenticated/customers/4242/orders"},
		{"POST", "/v1/authenticated/customers/4242/checkout"},
		{"POST", "/v1/authenticated/customers/4242/payments"},
		{"POST", customerBaskets + "/b1/checkout"},
	} {
		if _, _, _, err := c.send(context.Background(), bad.m, bad.p, map[string]any{}, true); err == nil {
			t.Errorf("%s %s: sent with writes on", bad.m, bad.p)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("%d request(s) reached the network: %v", n, f.requests())
	}
}

// The basket flag of the allowlist and the basket path shape agree: an entry
// is flagged exactly when its pattern is a baskets route, so a new entry
// cannot be added unflagged.
func TestMutationAllowlistBasketFlagsMatchPaths(t *testing.T) {
	flagged := 0
	for _, a := range mutationAllowlist {
		isBasketRoute := strings.Contains(a.path.String(), "/baskets")
		if a.basket != isBasketRoute {
			t.Errorf("%s %s: basket flag %v, route says %v", a.method, a.path, a.basket, isBasketRoute)
		}
		if a.basket {
			flagged++
		}
	}
	if flagged != 3 {
		t.Errorf("%d flagged entries, want 3", flagged)
	}
}
