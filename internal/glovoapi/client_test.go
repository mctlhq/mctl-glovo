package glovoapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// All tokens, ids and addresses below are synthetic.

var testNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func testJWT(exp int64) string {
	enc := base64.RawURLEncoding
	return "Bearer " + enc.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." +
		enc.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp))) + ".sig"
}

var (
	validToken   = testJWT(testNow.Unix() + 3600)
	expiredToken = testJWT(testNow.Unix() - 60)
	rotatedToken = testJWT(testNow.Unix() + 7200)
)

type recorded struct {
	Method string
	URI    string
	Header http.Header
	Body   string
}

type reply struct {
	status int
	body   string
}

// fakeGlovo is an httptest stand-in for the Glovo API. Replies are queued per
// "METHOD /path" (path without query); an unqueued request gets 200 {}.
type fakeGlovo struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	reqs    []recorded
	replies map[string][]reply
}

func newFake(t *testing.T) *fakeGlovo {
	f := &fakeGlovo{t: t, replies: map[string][]reply{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGlovo) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.reqs = append(f.reqs, recorded{Method: r.Method, URI: r.RequestURI, Header: r.Header.Clone(), Body: string(body)})
	key := r.Method + " " + r.URL.EscapedPath()
	rep := reply{status: 200, body: "{}"}
	if q := f.replies[key]; len(q) > 0 {
		rep = q[0]
		if len(q) > 1 {
			f.replies[key] = q[1:]
		}
	}
	f.mu.Unlock()
	w.WriteHeader(rep.status)
	_, _ = io.WriteString(w, rep.body)
}

// on queues replies; the last one repeats.
func (f *fakeGlovo) on(method, path string, replies ...reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[method+" "+path] = replies
}

func (f *fakeGlovo) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}

func (f *fakeGlovo) last() recorded {
	r := f.requests()
	if len(r) == 0 {
		f.t.Fatal("no request recorded")
	}
	return r[len(r)-1]
}

func (f *fakeGlovo) count(method, pathPrefix string) int {
	n := 0
	for _, r := range f.requests() {
		if r.Method == method && strings.HasPrefix(r.URI, pathPrefix) {
			n++
		}
	}
	return n
}

func testSession() Session {
	return Session{
		"accessToken":  validToken,
		"refreshToken": "refresh-test-old",
		"customerId":   json.Number("4242"),
		"keepMe":       "unknown-field",
		"location":     map[string]any{"cityCode": "TST", "latitude": "10.5", "longitude": "-3.25", "countryCode": "XX"},
	}
}

func writeSessionFile(t *testing.T, s Session) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "glovo", "session.json")
	if s != nil {
		if err := SaveSession(path, s, testNow); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func newTestClient(t *testing.T, f *fakeGlovo, s Session) (*Client, string) {
	t.Helper()
	path := writeSessionFile(t, s)
	ids := 0
	var idMu sync.Mutex
	c := New(path,
		WithBaseURL(f.srv.URL),
		WithHTTPClient(f.srv.Client()),
		WithDefaultLocation(Location{"countryCode": "ES", "cityCode": "BCN", "latitude": "41.3874", "longitude": "2.1686", "accuracy": "0", "languageCode": "en"}),
	)
	c.now = func() time.Time { return testNow }
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
	return c, path
}

func jsonEqual(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("request body is not JSON: %q", got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad expectation %q", want)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("body\n got: %s\nwant: %s", got, want)
	}
}

func TestRequestHeaders(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, testSession())
	ctx := context.Background()
	if _, err := c.GetStore(ctx, "example-market"); err != nil {
		t.Fatal(err)
	}
	h := f.last().Header
	want := map[string]string{
		"Accept":                            "application/json, text/plain, */*",
		"Content-Type":                      "application/json",
		"User-Agent":                        "Glovo/10.0.0 (iPhone; iOS 17.5.1; Scale/3.00)",
		"Glovo-Api-Version":                 "14",
		"Glovo-App-Context":                 "customer",
		"Glovo-App-Development-State":       "prod",
		"Glovo-App-Platform":                "iOS",
		"Glovo-App-Type":                    "customer",
		"Glovo-App-Version":                 "10.0.0",
		"Glovo-Device-Urn":                  "glv:device:00000000-0000-4000-8000-000000000002",
		"Glovo-Language-Code":               "en",
		"Glovo-Location-Country-Code":       "XX",
		"Glovo-Location-City-Code":          "TST",
		"Glovo-Delivery-Location-Latitude":  "10.5",
		"Glovo-Delivery-Location-Longitude": "-3.25",
		"Glovo-Delivery-Location-Timestamp": fmt.Sprint(testNow.UnixMilli()),
		"Glovo-Delivery-Location-Accuracy":  "0",
		"Glovo-Perseus-Client-Id":           "00000000-0000-4000-8000-000000000001",
		"Glovo-Perseus-Session-Id":          "00000000-0000-4000-8000-000000000001",
		"Glovo-Perseus-Session-Timestamp":   fmt.Sprint(testNow.UnixMilli()),
		"Glovo-Perseus-Consent":             "essential_functional_marketing",
		"Glovo-Request-Id":                  "00000000-0000-4000-8000-000000000003",
		"Glovo-Request-Ttl":                 "7500",
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
	if h.Get("Authorization") != "" {
		t.Error("public call sent Authorization")
	}

	if _, err := c.GetSavedLocations(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.last().Header.Get("Authorization"); got != validToken {
		t.Errorf("Authorization = %q, want the raw session token", got)
	}
}

func TestSessionIdentityBitsOverrideGenerated(t *testing.T) {
	f := newFake(t)
	s := testSession()
	s["deviceUrn"] = "glv:device:session-test"
	s["location"].(map[string]any)["perseusClientId"] = "perseus-from-location"
	c, _ := newTestClient(t, f, s)
	if _, err := c.GetStoreFees(context.Background(), "1", "2"); err != nil {
		t.Fatal(err)
	}
	h := f.last().Header
	if h.Get("Glovo-Device-Urn") != "glv:device:session-test" || h.Get("Glovo-Perseus-Client-Id") != "perseus-from-location" {
		t.Errorf("identity headers: %v", h)
	}
}

func TestEndpointRequests(t *testing.T) {
	ctx := context.Background()
	type call struct {
		name   string
		do     func(c *Client) error
		method string
		uri    string
		body   string // "" means no body
		auth   bool
	}
	ignore := func(_ Value, err error) error { return err }
	cases := []call{
		{"SearchAddresses", func(c *Client) error { return ignore(c.SearchAddresses(ctx, "test street", true)) },
			"GET", "/v3/addresslookup/pub/address?address=test+street&allowFallback=true", "", false},
		{"ResolveAddress", func(c *Client) error { return ignore(c.ResolveAddress(ctx, "place/with spaces", "google")) },
			"GET", "/v3/addresslookup/pub/place%2Fwith%20spaces?provider=google", "", false},
		{"ResolveAddressNoProvider", func(c *Client) error { return ignore(c.ResolveAddress(ctx, "p1", "")) },
			"GET", "/v3/addresslookup/pub/p1", "", false},
		{"ReverseAddress", func(c *Client) error { return ignore(c.ReverseAddress(ctx, 41.4, 2.17, false)) },
			"GET", "/v3/addresslookup/pub/coordinates?latitude=41.4&longitude=2.17&allowFallback=false", "", false},
		{"DeliveryPointInfo", func(c *Client) error { return ignore(c.DeliveryPointInfo(ctx, 41.4, 2.17, "ES")) },
			"GET", "/customer_profile/api/v1/guest/address_book/delivery_point_info?latitude=41.4&longitude=2.17&countryCode=ES", "", false},
		{"GetSavedLocations", func(c *Client) error { return ignore(c.GetSavedLocations(ctx)) },
			"GET", "/customer_profile/api/v1/address_book/me/addresses", "", true},
		{"BrowseStores", func(c *Client) error {
			return ignore(c.BrowseStores(ctx, BrowseStoresParams{CategoryID: 1, Offset: 24, Limit: 24, PreviousStoreIDs: []string{"11", "12"}}))
		}, "POST", "/v1/web/store_wall/category/1?moduleId=StoreFeeds&offset=24&limit=24&handlingType=DELIVERY",
			`{"filters":[],"previousStoreIds":["11","12"]}`, false},
		{"BrowseStoresDefaults", func(c *Client) error { return ignore(c.BrowseStores(ctx, BrowseStoresParams{})) },
			"POST", "/v1/web/store_wall/category/4?moduleId=StoreFeeds&offset=0&limit=50&handlingType=DELIVERY",
			`{"filters":[],"previousStoreIds":[]}`, false},
		{"SearchStores", func(c *Client) error { return ignore(c.SearchStores(ctx, "  pizza place ")) },
			"POST", "/v1/web/store_wall/search?searchQuery=pizza+place",
			`{"searchContext":{"searchId":"00000000-0000-4000-8000-000000000003"}}`, false},
		{"GetStore", func(c *Client) error { return ignore(c.GetStore(ctx, "example market")) },
			"GET", "/v3/stores/example%20market?includeClosed=true&includeDisabled=false", "", false},
		{"GetStoreMenu", func(c *Client) error {
			return ignore(c.GetStoreMenu(ctx, "1001", "2001", MenuOptions{Translation: "es", Consents: "c1"}))
		}, "GET", "/v3/stores/1001/addresses/2001/node/store_menu?translation=es&consents=c1", "", false},
		{"GetStoreContentV4", func(c *Client) error {
			return ignore(c.GetStoreContent(ctx, "1001", "2001", StoreContentOptions{Auth: true}))
		}, "GET", "/v4/stores/1001/addresses/2001/content/main", "", true},
		{"GetStoreContentV3Slug", func(c *Client) error {
			return ignore(c.GetStoreContent(ctx, "1001", "2001", StoreContentOptions{UseV3: true, ContentSlug: "dairy", Translation: "en"}))
		}, "GET", "/v3/stores/1001/addresses/2001/content?translation=en&nodeType=DEEP_LINK&link=dairy", "", false},
		{"GetStoreCatalog", func(c *Client) error {
			return ignore(c.GetStoreCatalog(ctx, "1001", "2001", "/v3/stores/1001/addresses/2001/content?nodeType=DEEP_LINK&link=dairy", true))
		}, "GET", "/v3/stores/1001/addresses/2001/content?nodeType=DEEP_LINK&link=dairy", "", true},
		{"GetStoreFees", func(c *Client) error { return ignore(c.GetStoreFees(ctx, "1001", "2001")) },
			"GET", "/v1/stores/1001/addresses/2001/node/store_fees", "", false},
		{"GetStoreRestrictions", func(c *Client) error { return ignore(c.GetStoreRestrictions(ctx, "1001", "2001")) },
			"GET", "/v4/stores/1001/addresses/2001/restrictions", "", false},
		{"GetStoreInfo", func(c *Client) error { return ignore(c.GetStoreInfo(ctx, "1001", "2001", "")) },
			"GET", "/v3/stores/1001/addresses/2001/store_info_screen?translation=en", "", false},
		{"GetSimilarStores", func(c *Client) error { return ignore(c.GetSimilarStores(ctx, "1001", 0)) },
			"GET", "/v1/store-view/web/similar-stores?limit=5&storeId=1001&city_changed=false", "", false},
		{"SearchStoreItems", func(c *Client) error { return ignore(c.SearchStoreItems(ctx, "1001", "2001", "leche entera")) },
			"GET", "/v3/stores/1001/addresses/2001/search?query=leche+entera&searchId=00000000-0000-4000-8000-000000000003", "", false},
		{"GetProduct", func(c *Client) error {
			return ignore(c.GetProduct(ctx, ProductParams{StoreID: "1001", StoreAddressID: "2001", ProductID: "6001", ExternalID: "ext-6001", CategoryID: json.Number("9")}))
		}, "POST", "/v4/stores/1001/addresses/2001/products/6001/view?productExternalId=ext-6001",
			`{"editingMode":false,"origin":"STORE","categoryId":9,"translation":null,"trackingOrigin":null,"quantity":1}`, false},
		{"GetBaskets", func(c *Client) error { return ignore(c.GetBaskets(ctx)) },
			"GET", "/v1/authenticated/customers/4242/baskets", "", true},
		{"GetBasketByStore", func(c *Client) error { return ignore(c.GetBasketByStore(ctx, "1001")) },
			"GET", "/v1/authenticated/customers/4242/baskets/stores/1001", "", true},
		{"GetOrders", func(c *Client) error { return ignore(c.GetOrders(ctx, "cursor-b", 0)) },
			"GET", "/v3/customer/orders-list?offset=cursor-b&limit=15", "", true},
		{"GetOrder", func(c *Client) error { return ignore(c.GetOrder(ctx, "9001")) },
			"GET", "/v3/customer/orders/9001", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			c, _ := newTestClient(t, f, testSession())
			if err := tc.do(c); err != nil {
				t.Fatal(err)
			}
			reqs := f.requests()
			if len(reqs) != 1 {
				t.Fatalf("%d requests, want 1", len(reqs))
			}
			r := reqs[0]
			if r.Method != tc.method || r.URI != tc.uri {
				t.Errorf("request %s %s, want %s %s", r.Method, r.URI, tc.method, tc.uri)
			}
			if tc.body == "" {
				if r.Body != "" {
					t.Errorf("unexpected body %q", r.Body)
				}
			} else {
				jsonEqual(t, r.Body, tc.body)
			}
			if got := r.Header.Get("Authorization") != ""; got != tc.auth {
				t.Errorf("Authorization present = %v, want %v", got, tc.auth)
			}
		})
	}
}

func TestInputValidationBeforeNetwork(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, testSession())
	ctx := context.Background()
	checks := map[string]error{}
	_, checks["short address"] = c.SearchAddresses(ctx, " ab ", true)
	_, checks["empty place"] = c.ResolveAddress(ctx, "", "")
	_, checks["no country"] = c.DeliveryPointInfo(ctx, 1, 2, "")
	_, checks["empty store query"] = c.SearchStores(ctx, "   ")
	_, checks["foreign origin"] = c.GetStoreCatalog(ctx, "1001", "2001", "https://evil.example.test/v3/stores/1001/addresses/2001/content", true)
	_, checks["other store"] = c.GetStoreCatalog(ctx, "1001", "2001", "/v3/stores/9999/addresses/2001/content", true)
	_, checks["dot segments"] = c.GetStoreCatalog(ctx, "1001", "2001", "/v3/stores/1001/addresses/2001/content/../../../../customer/orders-list", true)
	for name, err := range checks {
		if err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("%d requests reached the network", n)
	}
}

func TestNoCustomerIDIsAuthError(t *testing.T) {
	f := newFake(t)
	s := testSession()
	delete(s, "customerId")
	c, _ := newTestClient(t, f, s)
	_, err := c.GetBaskets(context.Background())
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("err = %v, want AuthError", err)
	}
	s["customer"] = map[string]any{"id": "cust-nested"}
	c2, _ := newTestClient(t, f, s)
	if _, err := c2.GetBaskets(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.last().URI, "/v1/authenticated/customers/cust-nested/") {
		t.Errorf("uri %s", f.last().URI)
	}
}

func TestFailClosedResponses(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		reply reply
		check func(t *testing.T, err error)
	}{
		{"server error", reply{500, `{"error":{"exceptionName":"InternalError"}}`}, func(t *testing.T, err error) {
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != 500 || apiErr.Detail != "InternalError" {
				t.Errorf("err = %#v", err)
			}
		}},
		{"rate limited", reply{429, `slow down`}, func(t *testing.T, err error) {
			if !isRateLimited(err) {
				t.Errorf("err = %v, want rate limited", err)
			}
		}},
		{"not found without allowNotFound", reply{404, `{}`}, func(t *testing.T, err error) {
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != 404 || apiErr.Detail != "request_failed" {
				t.Errorf("err = %#v", err)
			}
		}},
		{"malformed JSON", reply{200, `{"stores":`}, func(t *testing.T, err error) {
			if err == nil || !strings.Contains(err.Error(), "malformed JSON") {
				t.Errorf("err = %v", err)
			}
		}},
		{"empty body", reply{200, ``}, func(t *testing.T, err error) {
			if err == nil {
				t.Error("empty 200 body accepted")
			}
		}},
		{"trailing data", reply{200, `{} {}`}, func(t *testing.T, err error) {
			if err == nil {
				t.Error("trailing data accepted")
			}
		}},
		{"redirect status", reply{302, ``}, func(t *testing.T, err error) {
			if err == nil {
				t.Error("3xx accepted")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			c, _ := newTestClient(t, f, testSession())
			f.on("GET", "/v1/stores/1001/addresses/2001/node/store_fees", tc.reply)
			v, err := c.GetStoreFees(ctx, "1001", "2001")
			if v != nil {
				t.Errorf("value %v returned with error", v)
			}
			tc.check(t, err)
		})
	}
}

func TestNoContentAndAllowedNotFoundAreAbsent(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, testSession())
	f.on("GET", "/v1/authenticated/customers/4242/baskets/stores/1001", reply{404, `{"error":{}}`})
	v, err := c.GetBasketByStore(context.Background(), "1001")
	if err != nil || v != nil {
		t.Fatalf("404 basket: %v %v", v, err)
	}
	f.on("GET", "/v1/stores/1/addresses/2/node/store_fees", reply{204, ``})
	v, err = c.GetStoreFees(context.Background(), "1", "2")
	if err != nil || v != nil {
		t.Fatalf("204: %v %v", v, err)
	}
}

func TestErrorRedaction(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, testSession())
	f.on("GET", "/v1/authenticated/customers/4242/baskets/stores/1001", reply{500,
		`{"error":{"message":"No basket basket-live-123 for customer 4242 and 0b6f2a3e-1c2d-4e5f-8a9b-0c1d2e3f4a5b"}}`})
	_, err := c.GetBasketByStore(context.Background(), "1001")
	if err == nil {
		t.Fatal("no error")
	}
	msg := err.Error()
	for _, leak := range []string{"4242", "basket-live-123", "0b6f2a3e"} {
		if strings.Contains(msg, leak) {
			t.Errorf("error leaks %q: %s", leak, msg)
		}
	}
	// Upstream safePath rewrites /baskets/stores/1001 to /baskets/[basket]/1001;
	// a store id is public, the customer id is what must not leak.
	if !strings.Contains(msg, "/authenticated/customers/[customer]/baskets/[basket]") {
		t.Errorf("path not redacted: %s", msg)
	}
	f.on("GET", "/v3/customer/orders/9001", reply{500, `not json at all 9001`})
	_, err = c.GetOrder(context.Background(), "9001")
	if err == nil || strings.Contains(err.Error(), "9001") {
		t.Errorf("order error: %v", err)
	}
}

func TestRefreshOn401RetriesExactlyOnce(t *testing.T) {
	f := newFake(t)
	c, path := newTestClient(t, f, testSession())
	f.on("GET", "/v3/customer/orders/9001", reply{401, `{}`}, reply{200, `{"id":9001}`})
	f.on("POST", "/oauth/refresh", reply{200, fmt.Sprintf(`{"access":{"accessToken":%q,"refreshToken":"refresh-test-new","expiresIn":3600,"tokenType":"Bearer"}}`, rotatedToken)})
	v, err := c.GetOrder(context.Background(), "9001")
	if err != nil {
		t.Fatal(err)
	}
	if jsString(get(v, "id")) != "9001" {
		t.Fatalf("value %v", v)
	}
	if n := f.count("POST", "/oauth/refresh"); n != 1 {
		t.Fatalf("%d refreshes, want 1", n)
	}
	reqs := f.requests()
	if len(reqs) != 3 {
		t.Fatalf("%d requests, want order, refresh, order", len(reqs))
	}
	jsonEqual(t, reqs[1].Body, `{"refreshToken":"refresh-test-old"}`)
	if reqs[1].Header.Get("Authorization") != "" {
		t.Error("refresh sent Authorization")
	}
	if got := reqs[2].Header.Get("Authorization"); got != rotatedToken {
		t.Errorf("retry Authorization = %q", got)
	}
	persisted, err := LoadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if persisted["accessToken"] != rotatedToken || persisted["refreshToken"] != "refresh-test-new" || persisted["keepMe"] != "unknown-field" {
		t.Errorf("persisted session: %v", persisted)
	}
	if jsString(persisted["createdAt"]) != fmt.Sprint(testNow.UnixMilli()) || persisted["tokenType"] != "Bearer" {
		t.Errorf("persisted metadata: %v", persisted)
	}
	assertMode(t, path, 0o600)
}

func TestSecond401IsAuthErrorAfterOneRefresh(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, testSession())
	f.on("GET", "/v3/customer/orders/9001", reply{401, `{}`})
	f.on("POST", "/oauth/refresh", reply{200, fmt.Sprintf(`{"accessToken":%q}`, rotatedToken)})
	_, err := c.GetOrder(context.Background(), "9001")
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("err = %v, want AuthError", err)
	}
	if n := f.count("POST", "/oauth/refresh"); n != 1 {
		t.Errorf("%d refreshes, want exactly 1", n)
	}
	if n := f.count("GET", "/v3/customer/orders/9001"); n != 2 {
		t.Errorf("%d order requests, want 2", n)
	}
}

func TestRefreshFailureIsError(t *testing.T) {
	cases := map[string]reply{
		"status":         {500, `{}`},
		"malformed":      {200, `{"accessToken":`},
		"no token":       {200, `{"refreshToken":"x"}`},
		"non-string tok": {200, `{"accessToken":{"nope":true}}`},
	}
	for name, rep := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			c, path := newTestClient(t, f, testSession())
			before, _ := os.ReadFile(path) // #nosec G304 -- test-owned temp or testdata path
			f.on("GET", "/v3/customer/orders/9001", reply{401, `{}`})
			f.on("POST", "/oauth/refresh", rep)
			v, err := c.GetOrder(context.Background(), "9001")
			var authErr *AuthError
			if v != nil || !errors.As(err, &authErr) {
				t.Fatalf("got %v, %v; want AuthError", v, err)
			}
			after, _ := os.ReadFile(path) // #nosec G304 -- test-owned temp or testdata path
			if !bytes.Equal(before, after) {
				t.Error("failed refresh rewrote the session file")
			}
			if n := f.count("GET", "/v3/customer/orders/9001"); n != 1 {
				t.Errorf("%d order requests after failed refresh, want 1", n)
			}
		})
	}
}

func TestExpiredTokenRefreshesBeforeRequest(t *testing.T) {
	f := newFake(t)
	s := testSession()
	s["accessToken"] = expiredToken
	c, _ := newTestClient(t, f, s)
	f.on("POST", "/oauth/refresh", reply{200, fmt.Sprintf(`{"accessToken":%q}`, rotatedToken)})
	if _, err := c.GetOrders(context.Background(), nil, 0); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) != 2 || reqs[0].URI != "/oauth/refresh" || reqs[1].Header.Get("Authorization") != rotatedToken {
		t.Fatalf("requests %+v", reqs)
	}
	if c.Session()["refreshToken"] != "refresh-test-old" {
		t.Error("refresh token not kept when the response has none")
	}
}

func TestNoRefreshWithoutRefreshTokenOrAuth(t *testing.T) {
	f := newFake(t)
	s := testSession()
	delete(s, "refreshToken")
	c, _ := newTestClient(t, f, s)
	f.on("GET", "/v3/customer/orders/9001", reply{401, `{}`})
	_, err := c.GetOrder(context.Background(), "9001")
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("err = %v", err)
	}
	f.on("GET", "/v3/stores/x", reply{403, `{}`})
	if _, err := c.GetStore(context.Background(), "x"); !errors.As(err, &authErr) {
		t.Fatalf("public 403: %v", err)
	}
	if n := f.count("POST", "/oauth/refresh"); n != 0 {
		t.Errorf("%d refreshes", n)
	}
}

func TestSignedOutAuthCallFails(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, nil)
	_, err := c.GetSavedLocations(context.Background())
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("err = %v", err)
	}
	if len(f.requests()) != 0 {
		t.Error("signed-out call reached the network")
	}
	if c.AuthStatus().SignedIn {
		t.Error("signed in without a session")
	}
}

func TestMutationAllowlist(t *testing.T) {
	refused := []struct{ method, path string }{
		{"POST", "/v3/customer/orders"},
		{"POST", "/v1/authenticated/customers/1/orders"},
		{"POST", "/v1/authenticated/customers/1/baskets/b1/checkout"},
		{"POST", "/v1/checkout"},
		{"PUT", "/v1/authenticated/customers/1/baskets/b1/payment"},
		{"DELETE", "/v1/authenticated/customers/1/baskets/b1"},
		{"POST", "/v1/authenticated/customers/1/baskets/b1/products"},
		{"PATCH", "/v1/authenticated/customers/1/baskets/b1"},
		{"POST", "/v1/authenticated/customers/1/baskets?x=/orders"},
		{"POST", "/v4/stores/1/addresses/2/products/3/view/../../../../../orders"},
	}
	for _, r := range refused {
		if checkMutation(r.method, r.path) == nil {
			t.Errorf("%s %s allowed", r.method, r.path)
		}
	}
	allowed := []struct{ method, path string }{
		{"GET", "/anything"},
		{"POST", "/oauth/refresh"},
		{"POST", "/v1/web/store_wall/category/4?moduleId=StoreFeeds"},
		{"POST", "/v1/web/store_wall/search?searchQuery=x"},
		{"POST", "/v4/stores/1/addresses/2/products/3/view"},
		{"POST", "/v1/authenticated/customers/1/baskets"},
		{"PUT", "/v1/authenticated/customers/1/baskets/b1/products"},
		{"PATCH", "/v1/authenticated/customers/1/baskets/b1/products/quantity"},
	}
	for _, r := range allowed {
		if err := checkMutation(r.method, r.path); err != nil {
			t.Errorf("%s %s refused: %v", r.method, r.path, err)
		}
	}
	f := newFake(t)
	c, _ := newTestClient(t, f, testSession())
	if _, err := c.call(context.Background(), "/v3/customer/orders", callOpts{method: "POST", body: map[string]any{}}); err == nil {
		t.Fatal("order POST allowed")
	}
	if len(f.requests()) != 0 {
		t.Fatal("refused mutation reached the network")
	}
}

func TestStoreOrderOptionsFailsOnAnyFailedRead(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, testSession())
	f.on("GET", "/v4/stores/1001/addresses/2001/restrictions", reply{503, `{}`})
	res, err := c.StoreOrderOptions(context.Background(), "1001", "2001", "", 0)
	if err == nil || res != nil {
		t.Fatalf("partial order options returned: %v %v", res, err)
	}
}
