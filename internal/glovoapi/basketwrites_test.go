package glovoapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// hostedBasketClient is a hosted client against f, with or without
// WithBasketWrites.
func hostedBasketClient(t *testing.T, f *fakeGlovo, writes bool) *Client {
	t.Helper()
	opts := []Option{
		WithBaseURL(f.srv.URL),
		WithHTTPClient(f.srv.Client()),
		WithSession(testSession()),
		WithRefreshCoordinator(hostedRC(testSession())),
	}
	if writes {
		opts = append(opts, WithBasketWrites())
	}
	c, err := New("", opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// writesSeen counts the requests the fake got that change a basket.
func writesSeen(f *fakeGlovo) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.reqs {
		if r.Method != http.MethodGet && strings.Contains(r.URI, "/baskets") {
			n++
		}
	}
	return n
}

// The three basket mutations (create POST, add PUT, quantity PATCH) are all
// refused by a default hosted client, before the network.
func TestHostedClientRefusesEveryBasketWriteByDefault(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		prep func(f *fakeGlovo)
		do   func(c *Client) error
	}{
		{"create POST", func(f *fakeGlovo) { f.on("GET", basketPath+"/stores/1001", reply{404, `{}`}) },
			func(c *Client) error {
				_, err := c.AddToBasket(ctx, AddToBasketParams{StoreID: "1001", StoreAddressID: "2001", StoreCategoryID: "4", ProductID: "6001", ExternalID: "e", StoreProductID: "s", Quantity: 1})
				return err
			}},
		{"append PUT", func(f *fakeGlovo) { f.on("GET", basketPath+"/stores/1001", reply{200, fixtureText(t, "basket")}) },
			func(c *Client) error {
				_, err := c.AddToBasket(ctx, AddToBasketParams{StoreID: "1001", ProductID: "9999", ExternalID: "e", StoreProductID: "s9", Quantity: 1})
				return err
			}},
		{"quantity PATCH", func(f *fakeGlovo) { f.on("GET", basketPath+"/stores/1001", reply{200, fixtureText(t, "basket")}) },
			func(c *Client) error {
				_, err := c.SetQuantity(ctx, BasketLineRef{StoreID: "1001", BasketProductID: "bp-test-1"}, 4)
				return err
			}},
		{"remove PATCH", func(f *fakeGlovo) { f.on("GET", basketPath+"/stores/1001", reply{200, fixtureText(t, "basket")}) },
			func(c *Client) error {
				_, err := c.RemoveFromBasket(ctx, BasketLineRef{StoreID: "1001", BasketProductID: "bp-test-1"})
				return err
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			tc.prep(f)
			if err := tc.do(hostedBasketClient(t, f, false)); !errors.Is(err, ErrBasketWritesDisabled) {
				t.Fatalf("default hosted client: err = %v, want ErrBasketWritesDisabled", err)
			}
			if n := writesSeen(f); n != 0 {
				t.Fatalf("%d basket write(s) reached the network", n)
			}
			// With the option the same call goes out: the refusal is the
			// option's absence, not a broken flow.
			f2 := newFake(t)
			tc.prep(f2)
			if err := tc.do(hostedBasketClient(t, f2, true)); err != nil {
				t.Fatalf("with WithBasketWrites: %v", err)
			}
			if n := writesSeen(f2); n != 1 {
				t.Fatalf("with WithBasketWrites: %d write(s), want 1", n)
			}
		})
	}
}

// Reads of baskets stay open on a default hosted client.
func TestHostedClientBasketReadsStayOpen(t *testing.T) {
	f := newFake(t)
	f.on("GET", basketPath, reply{200, `[]`})
	f.on("GET", basketPath+"/stores/1001", reply{200, fixtureText(t, "basket")})
	c := hostedBasketClient(t, f, false)
	if _, err := c.GetBaskets(context.Background()); err != nil {
		t.Fatalf("GetBaskets: %v", err)
	}
	if _, err := c.GetBasketByStore(context.Background(), "1001"); err != nil {
		t.Fatalf("GetBasketByStore: %v", err)
	}
}

// checkBasketWrite sees through dot segments and query strings, and a
// session-file client (glovo-home) is unaffected.
func TestCheckBasketWriteShapes(t *testing.T) {
	hosted, err := New("", WithRefreshCoordinator(hostedRC(testSession())), WithSession(testSession()))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		"/v1/authenticated/customers/1/baskets",
		"/v1/authenticated/customers/1/baskets/b/products",
		"/v1/authenticated/customers/1/baskets/b/products/quantity",
		"/v1/authenticated/customers/1/baskets?x=1",
		"/v1/x/../authenticated/customers/1/baskets/b/products",
	} {
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			if err := hosted.checkBasketWrite(m, p); !errors.Is(err, ErrBasketWritesDisabled) {
				t.Errorf("%s %s: %v", m, p, err)
			}
		}
		if err := hosted.checkBasketWrite(http.MethodGet, p); err != nil {
			t.Errorf("GET %s refused: %v", p, err)
		}
	}
	// Reads that POST (store wall, product view) are not basket writes.
	for _, p := range []string{"/v1/web/store_wall/search?searchQuery=x", "/v4/stores/1/addresses/2/products/3/view"} {
		if err := hosted.checkBasketWrite(http.MethodPost, p); err != nil {
			t.Errorf("POST %s refused: %v", p, err)
		}
	}
	file, err := New(writeSessionFile(t, testSession()))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.checkBasketWrite(http.MethodPut, "/v1/authenticated/customers/1/baskets/b/products"); err != nil {
		t.Errorf("session-file client refused: %v", err)
	}
}

// The mutation allowlist itself carries no checkout, payment or order path.
func TestMutationAllowlistHasNoOrderPath(t *testing.T) {
	for _, a := range mutationAllowlist {
		s := strings.ToLower(a.path.String())
		for _, bad := range []string{"checkout", "payment", "order", "pay", "purchase", "submit", "confirm"} {
			if strings.Contains(s, bad) {
				t.Errorf("%s %s names %q", a.method, a.path, bad)
			}
		}
	}
	// The only basket-changing entries are the three baskets routes.
	n := 0
	for _, a := range mutationAllowlist {
		if basketPathRe.MatchString(strings.TrimPrefix(strings.TrimSuffix(a.path.String(), "$"), "^")) {
			n++
		}
	}
	if n != 3 {
		t.Errorf("%d allowlist entries touch baskets, want 3 (create, add, quantity)", n)
	}
}

// A read POST (store search) that Glovo answers with a 307 to an allowed
// basket write endpoint must not carry the POST there on a default hosted
// client: the redirect hop applies the same refusal as a direct call.
func TestHostedClientRefusesRedirectIntoBasketWrite(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/v1/web/store_wall/search" {
			http.Redirect(w, r, basketPath, http.StatusTemporaryRedirect)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(up.Close)
	f := &fakeGlovo{srv: up}
	c := hostedBasketClient(t, f, false)
	_, err := c.SearchStores(context.Background(), "milk")
	if !errors.Is(err, ErrRedirectRefused) {
		t.Fatalf("err = %v, want ErrRedirectRefused", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, r := range seen {
		if strings.HasSuffix(r, "/baskets") {
			t.Fatalf("a basket write reached the network via a redirect: %v", seen)
		}
	}
}
