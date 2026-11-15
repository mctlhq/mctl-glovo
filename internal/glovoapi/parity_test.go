package glovoapi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func obj(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func isNilResult(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map:
		return rv.IsNil()
	}
	return false
}

// A field that must be a list but holds an object or a string is a
// ShapeError with no partial result, where upstream throws a TypeError.
func TestCompactorsRejectNonArrayLists(t *testing.T) {
	bad := map[string]any{"not": "a list"}
	cases := map[string]func() (any, error){
		"store wall entries": func() (any, error) {
			return CompactStoreWall(obj("data", obj("stores", obj("entries", bad))))
		},
		"store wall widgets": func() (any, error) {
			return CompactStoreWall(obj("data", obj("stores", obj("entries", []any{}), "widgets", bad)))
		},
		"menu elements":        func() (any, error) { return CompactMenu(obj("data", obj("elements", bad)), 0) },
		"menu nested elements": func() (any, error) { return CompactMenu(obj("elements", []any{obj("name", "a", "elements", "x")}), 0) },
		"search results":       func() (any, error) { return CompactSearch(obj("results", bad), SearchOptions{}) },
		"search products": func() (any, error) {
			return CompactSearch(obj("results", []any{obj("products", bad)}), SearchOptions{})
		},
		"content body": func() (any, error) {
			return CompactStoreContent(obj("data", obj("body", bad)), StoreContentCompactOptions{})
		},
		"content elements": func() (any, error) {
			return CompactStoreContent(obj("data", obj("body", []any{obj("data", obj("elements", bad))})), StoreContentCompactOptions{})
		},
		"content actions": func() (any, error) {
			tile := obj("type", "PRODUCT_TILE", "data", obj("id", 1), "actions", bad)
			return CompactStoreContent(obj("body", []any{obj("data", obj("elements", []any{tile}))}), StoreContentCompactOptions{})
		},
		"fee ranges":     func() (any, error) { return CompactStoreOrderOptions(obj("ranges", bad), nil, nil, nil) },
		"restrictions":   func() (any, error) { return CompactStoreOrderOptions(nil, obj("restrictions", bad), nil, nil) },
		"info sections":  func() (any, error) { return CompactStoreOrderOptions(nil, nil, obj("sections", bad), nil) },
		"similar stores": func() (any, error) { return CompactStoreOrderOptions(nil, nil, nil, obj("stores", "x")) },
		"option groups":  func() (any, error) { return OptionGroupsFromProduct(obj("product", obj("attributeGroups", bad))) },
		"option attributes": func() (any, error) {
			return OptionGroupsFromProduct(obj("product", obj("attributeGroups", []any{obj("id", "g", "attributes", bad)})))
		},
		"product footer": func() (any, error) {
			return CompactProductView(obj("data", obj("footer", obj("data", obj("elements", bad)))))
		},
		"required sections": func() (any, error) {
			return CompactProductView(obj("data", obj("actions", obj("primary", obj("events", bad)))))
		},
		"basket products":     func() (any, error) { return CompactBasket(obj("basketId", "b", "products", bad)) },
		"order card body":     func() (any, error) { return CompactOrderCard(obj("orderId", "1", "content", obj("body", bad))) },
		"order card products": func() (any, error) { return CompactOrderCard(obj("orderId", "1", "products", bad)) },
		"order detail lines":  func() (any, error) { return CompactOrderDetail(obj("id", 1, "boughtProducts", bad)) },
		"order pricing": func() (any, error) {
			return CompactOrderDetail(obj("id", 1, "pricingBreakdown", obj("lines", bad)))
		},
		"reorder lines":   func() (any, error) { return CompactReorderPreview(obj("id", 1, "boughtProducts", bad)) },
		"saved addresses": func() (any, error) { return CompactSavedLocations(obj("addresses", bad), Location{}, "") },
		"saved data addresses": func() (any, error) {
			return CompactSavedLocations(obj("data", obj("addresses", "x")), Location{}, "")
		},
		"saved fields": func() (any, error) {
			return CompactSavedLocations(obj("addresses", []any{obj("address", obj("id", 1, "fields", bad))}), Location{}, "")
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			v, err := fn()
			var se *ShapeError
			if !errors.Is(err, ErrUnexpectedShape) || !errors.As(err, &se) {
				t.Fatalf("err = %v, want a ShapeError", err)
			}
			if !isNilResult(v) {
				t.Errorf("partial result %+v returned with the error", v)
			}
		})
	}
}

// Absent and null lists stay empty, as with upstream `x || []`.
func TestCompactorsAcceptAbsentAndNullLists(t *testing.T) {
	for name, fn := range map[string]func() error{
		"store wall":  func() error { _, err := CompactStoreWall(obj("data", obj("stores", obj("entries", Null)))); return err },
		"menu":        func() error { _, err := CompactMenu(obj(), 0); return err },
		"search":      func() error { _, err := CompactSearch(obj("results", Null), SearchOptions{}); return err },
		"content":     func() error { _, err := CompactStoreContent(obj(), StoreContentCompactOptions{}); return err },
		"options":     func() error { _, err := CompactStoreOrderOptions(nil, nil, nil, nil); return err },
		"basket":      func() error { _, err := CompactBasket(obj("basketId", "b", "products", Null)); return err },
		"order":       func() error { _, err := CompactOrderDetail(obj("id", 1)); return err },
		"saved":       func() error { _, err := CompactSavedLocations(obj("addresses", Null), Location{}, ""); return err },
		"saved empty": func() error { _, err := CompactSavedLocations(obj(), Location{}, ""); return err },
	} {
		if err := fn(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestClientReadsFailOnNonArrayLists(t *testing.T) {
	ctx := context.Background()
	t.Run("order options", func(t *testing.T) {
		f := newFake(t)
		c, _ := newTestClient(t, f, testSession())
		f.on("GET", "/v4/stores/1001/addresses/2001/restrictions", reply{200, `{"restrictions":{}}`})
		if res, err := c.StoreOrderOptions(ctx, "1001", "2001", "", 0); !errors.Is(err, ErrUnexpectedShape) || res != nil {
			t.Fatalf("got %v, %v", res, err)
		}
	})
	t.Run("order history card", func(t *testing.T) {
		f := newFake(t)
		c, _ := newTestClient(t, f, testSession())
		f.on("GET", "/v3/customer/orders-list", reply{200, `{"orders":[{"orderId":"1","content":{"body":{}}}]}`})
		if res, err := c.GetAllOrderCards(ctx, OrderCardsOptions{MaxRetries: -1}); !errors.Is(err, ErrUnexpectedShape) || res != nil {
			t.Fatalf("got %v, %v", res, err)
		}
	})
	t.Run("saved locations", func(t *testing.T) {
		f := newFake(t)
		c, _ := newTestClient(t, f, testSession())
		f.on("GET", "/customer_profile/api/v1/address_book/me/addresses", reply{200, `{"addresses":{}}`})
		if res, err := c.SavedLocations(ctx, ""); !errors.Is(err, ErrUnexpectedShape) || res != nil {
			t.Fatalf("got %v, %v", res, err)
		}
	})
	t.Run("basket write", func(t *testing.T) {
		for name, ref := range map[string]func(c *Client) error{
			"add": func(c *Client) error {
				_, err := c.AddToBasket(ctx, AddToBasketParams{StoreID: "1001", ProductID: "1", ExternalID: "e", StoreProductID: "s"})
				return err
			},
			"set": func(c *Client) error {
				_, err := c.SetQuantity(ctx, BasketLineRef{StoreID: "1001", ProductID: "1"}, 2)
				return err
			},
		} {
			f := newFake(t)
			c, _ := newTestClient(t, f, testSession())
			f.on("GET", basketPath+"/stores/1001", reply{200, `{"basketId":"b1","products":{"bp":1}}`})
			if err := ref(c); !errors.Is(err, ErrUnexpectedShape) {
				t.Errorf("%s: err = %v", name, err)
			}
			if n := len(f.requests()); n != 1 {
				t.Errorf("%s: %d requests, want only the basket read", name, n)
			}
		}
	})
	t.Run("option groups", func(t *testing.T) {
		f := newFake(t)
		c, _ := newTestClient(t, f, testSession())
		_, err := c.AddToBasket(ctx, AddToBasketParams{
			StoreID: "1001", ProductID: "1", ExternalID: "e", StoreProductID: "s",
			ProductView:     obj("product", obj("attributeGroups", "x")),
			SelectedOptions: []SelectedOption{{GroupID: "g", OptionID: "o"}},
		})
		if !errors.Is(err, ErrUnexpectedShape) || len(f.requests()) != 0 {
			t.Fatalf("err = %v, %d requests", err, len(f.requests()))
		}
	})
}

func TestReloadEmptySessionIsSignedOut(t *testing.T) {
	for _, body := range []string{`{}`, `{"accessToken":"","location":null}`, `{"customerId":1}`} {
		f := newFake(t)
		c, path := newTestClient(t, f, nil)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := c.Reload(); err != nil {
			t.Fatalf("%s: Reload = %v, want signed out", body, err)
		}
		if st := c.AuthStatus(); st.SignedIn || st.SessionError != "" {
			t.Errorf("%s: status %+v", body, st)
		}
		if c.Location()["countryCode"] != "ES" {
			t.Errorf("%s: location %v", body, c.Location())
		}
	}
}

func TestMalformedSessionIsReportedAndRepairable(t *testing.T) {
	f := newFake(t)
	c, path := newTestClient(t, f, testSession())
	if err := os.WriteFile(path, []byte(`{"accessToken":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Reload(); !errors.Is(err, ErrSessionMalformed) {
		t.Fatalf("Reload = %v", err)
	}
	st := c.AuthStatus()
	if st.SignedIn || st.SessionError == "" || strings.Contains(st.SessionError, "accessToken") {
		t.Fatalf("status %+v", st)
	}
	if c.Session() != nil {
		t.Fatal("malformed file kept the old session")
	}
	if _, err := c.SetLocation(LocationUpdate{CountryCode: "ES", CityCode: "BCN", Latitude: "41.4", Longitude: "2.17"}); err != nil {
		t.Fatal(err)
	}
	if st := c.AuthStatus(); st.SessionError != "" || !st.HasLocation {
		t.Errorf("after repair %+v", st)
	}
	if err := c.Reload(); err != nil {
		t.Fatalf("repaired file: %v", err)
	}
	assertMode(t, path, 0o600)
}

func TestDotSegmentsAreRefused(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	c, _ := newTestClient(t, f, testSession())
	if _, err := c.GetProduct(ctx, ProductParams{StoreID: "1001", StoreAddressID: "2001", ProductID: "..", ExternalID: "e"}); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("POST with a .. product id: %v", err)
	}
	if _, err := c.GetStore(ctx, ".."); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("GET with a .. store id: %v", err)
	}
	if _, err := c.GetStoreFees(ctx, ".", "2001"); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("GET with a . store id: %v", err)
	}
	if _, err := c.GetOrder(ctx, ""); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("GET with an empty order id: %v", err)
	}
	if _, err := c.SetQuantity(ctx, BasketLineRef{StoreID: "..", BasketProductID: "x"}, 1); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("basket read with a .. store id: %v", err)
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("%d requests reached the network", n)
	}
	for _, p := range []string{
		"/v1/authenticated/customers/../baskets",
		"/v1/authenticated/customers/1/baskets/../products",
		"/v1/authenticated/customers/1/baskets/b/../b/products/quantity",
		"/v4/stores/1/addresses/2/products/../view",
	} {
		for _, m := range []string{"POST", "PUT", "PATCH"} {
			if checkMutation(m, p) == nil {
				t.Errorf("%s %s allowed", m, p)
			}
		}
	}
}

func TestStoreCategoryIDFromStore(t *testing.T) {
	cases := []struct {
		store Value
		want  string
	}{
		{nil, ""},
		{obj(), ""},
		{obj("categoryId", Null), ""},
		{obj("categoryId", json.Number("4")), "4"},
		{obj("data", obj("categoryId", "1")), "1"},
		{obj("store", obj("categoryId", json.Number("3"))), "3"},
	}
	for _, tc := range cases {
		if got := StoreCategoryIDFromStore(tc.store); got != tc.want {
			t.Errorf("StoreCategoryIDFromStore(%v) = %q, want %q", tc.store, got, tc.want)
		}
	}
}

func TestCreateBasketRefusesMissingCategory(t *testing.T) {
	for _, category := range []string{StoreCategoryIDFromStore(obj()), "null", "undefined", "0"} {
		f := newFake(t)
		c, _ := newTestClient(t, f, testSession())
		f.on("GET", basketPath+"/stores/1001", reply{404, `{}`})
		_, err := c.AddToBasket(context.Background(), AddToBasketParams{
			StoreID: "1001", StoreAddressID: "2001", StoreCategoryID: category,
			ProductID: "1", ExternalID: "e", StoreProductID: "s",
		})
		if err == nil || !strings.Contains(err.Error(), "storeCategoryId") {
			t.Errorf("category %q: err = %v", category, err)
		}
		if n := f.count("POST", basketPath); n != 0 {
			t.Errorf("category %q: basket created", category)
		}
	}
}

func TestOrderDatesLikeDateParse(t *testing.T) {
	zone := time.FixedZone("UTC+2", 2*3600)
	old := localZone
	localZone = func() *time.Location { return zone }
	t.Cleanup(func() { localZone = old })
	cases := map[string]string{
		"2026-01-02T10:00:00Z":          "2026-01-02T10:00:00.000Z",
		"2026-01-02T10:00:00.250+01:00": "2026-01-02T09:00:00.250Z",
		"2026-01-02T10:00:00+0100":      "2026-01-02T09:00:00.000Z",
		"2026-01-02T10:00:00":           "2026-01-02T08:00:00.000Z", // no zone: local
		"2026-01-02T10:00:00.5":         "2026-01-02T08:00:00.500Z",
		"2026-01-02T10:00":              "2026-01-02T08:00:00.000Z",
		"2026-01-02 10:00:00":           "2026-01-02T08:00:00.000Z", // space form: local
		"2026-01-02 10:00:00Z":          "2026-01-02T10:00:00.000Z",
		"2026-01-02":                    "2026-01-02T00:00:00.000Z", // date only: UTC
		"Fri, 02 Jan 2026 10:00:00 GMT": "2026-01-02T10:00:00.000Z",
		"1767348000":                    "2026-01-02T10:00:00.000Z",
	}
	for in, want := range cases {
		res, err := CompactOrderDetail(obj("id", 1, "createdAt", in))
		if err != nil {
			t.Fatal(err)
		}
		if res.Date == nil || *res.Date != want {
			got := "<nil>"
			if res.Date != nil {
				got = *res.Date
			}
			t.Errorf("%q -> %s, want %s", in, got, want)
		}
	}
	res, err := CompactOrderDetail(obj("id", 1, "createdAt", "yesterday"))
	if err != nil || res.Date != nil {
		t.Errorf("unparseable date: %v %v", res.Date, err)
	}
}

func TestJSToFixed2(t *testing.T) {
	for in, want := range map[float64]string{
		1.125: "1.13", 1.005: "1.00", 2.5: "2.50", 0.125: "0.13", 0.004: "0.00",
		-1.125: "-1.13", 1234.565: "1234.57", 0: "0.00", 10: "10.00",
	} {
		if got := jsToFixed2(in); got != want {
			t.Errorf("jsToFixed2(%v) = %q, want %q", in, got, want)
		}
	}
	it := CompactItemOf(obj("id", 1, "price", json.Number("1.125")))
	if it.Price != "€1.13" {
		t.Errorf("price %v", it.Price)
	}
}

// An id carrying an encoded slash or backslash never reaches a mutation
// path, and encoded dot segments are refused on every path.
func TestEncodedSlashAndDotSegmentsAreRefused(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	c, _ := newTestClient(t, f, testSession())
	f.on("GET", basketPath+"/stores/1001", reply{200, fixtureText(t, "basket")})
	if _, err := c.SetQuantity(ctx, BasketLineRef{StoreID: "1001", BasketProductID: "x/../../orders/1"}, 1); err != nil {
		t.Fatalf("a basket product id is a body field, not a path segment: %v", err)
	}
	if _, err := c.GetProduct(ctx, ProductParams{StoreID: "1001", StoreAddressID: "2001", ProductID: "x/../../../v1/checkout", ExternalID: "e"}); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("POST with a slash in the product id: %v", err)
	}
	if _, err := c.GetProduct(ctx, ProductParams{StoreID: `a\b`, StoreAddressID: "2001", ProductID: "1"}); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("POST with a backslash in the store id: %v", err)
	}
	for _, p := range []string{
		"/v1/authenticated/customers/1/baskets/x%2F..%2F..%2Forders/products",
		"/v1/authenticated/customers/1/baskets/x%2f..%2forders/products/quantity",
		"/v1/authenticated/customers/1%5C2/baskets",
		"/v4/stores/1/addresses/2/products/a%2Fb/view",
	} {
		for _, m := range []string{"POST", "PUT", "PATCH"} {
			if err := checkMutation(m, p); !errors.Is(err, ErrUnsafePath) {
				t.Errorf("%s %s: %v, want ErrUnsafePath", m, p, err)
			}
		}
	}
	for _, p := range []string{"/v3/stores/%2e%2e/addresses", "/v3/customer/orders/%2E", "/v3/stores/%zz"} {
		if err := checkPathSegments(p); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("%s: %v, want ErrUnsafePath", p, err)
		}
	}
	// A read may carry an opaque id with a slash: it is escaped, not split.
	if err := checkPathSegments("/v3/addresslookup/pub/place%2Fwith%20spaces"); err != nil {
		t.Errorf("read with an escaped slash: %v", err)
	}
	if n := f.count("POST", "/v4/stores/1001/addresses/2001/products/x%2F..%2F..%2F..%2Fv1%2Fcheckout/view"); n != 0 {
		t.Errorf("refused POST reached the network %d times", n)
	}
}

// A basket read without a basketId is malformed; writing to it would hit
// /baskets/undefined/...
func TestAddToBasketRefusesBasketWithoutID(t *testing.T) {
	for name, body := range map[string]string{
		"bump":   `{"products":[{"ids":{"id":"5001","basketProductId":"bp-1"},"quantity":{"increments":1}}]}`,
		"append": `{"products":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			c, _ := newTestClient(t, f, testSession())
			f.on("GET", basketPath+"/stores/1001", reply{200, body})
			_, err := c.AddToBasket(context.Background(), AddToBasketParams{StoreID: "1001", ProductID: "5001", ExternalID: "e", StoreProductID: "s"})
			if err == nil || !strings.Contains(err.Error(), "basketId") {
				t.Fatalf("err %v, want a missing basketId error", err)
			}
			if n := len(f.requests()); n != 1 {
				t.Fatalf("%d requests: only the basket read may reach the network", n)
			}
		})
	}
}

// A negative add quantity is refused before the network, so an add never
// decrements a line.
func TestAddToBasketRefusesNegativeQuantity(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, testSession())
	f.on("GET", basketPath+"/stores/1001", reply{200, fixtureText(t, "basket")})
	_, err := c.AddToBasket(context.Background(), AddToBasketParams{StoreID: "1001", ProductID: "5001", ExternalID: "ext-5001", StoreProductID: "sp-5001", Quantity: -2})
	if err == nil {
		t.Fatal("negative quantity accepted")
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("%d requests reached the network", n)
	}
}
