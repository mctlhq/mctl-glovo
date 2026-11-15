package glovoapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const basketPath = "/v1/authenticated/customers/4242/baskets"

func fixtureText(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".json")) // #nosec G304 -- test-owned temp or testdata path
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAddToBasketCreatesBasket(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, testSession())
	f.on("GET", basketPath+"/stores/1001", reply{404, `{}`})
	_, err := c.AddToBasket(context.Background(), AddToBasketParams{
		StoreID: "1001", StoreAddressID: "2001", StoreCategoryID: "4",
		ProductID: "6001", ExternalID: "ext-6001", StoreProductID: "sp-6001", Quantity: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := f.last()
	if r.Method != "POST" || r.URI != basketPath {
		t.Fatalf("%s %s", r.Method, r.URI)
	}
	jsonEqual(t, r.Body, `{"products":[{"ids":{"id":"6001","externalId":"ext-6001","storeProductId":"sp-6001"},"quantity":{"increments":2}}],
		"storeId":1001,"storeAddressId":2001,"storeCategoryId":4,"handlingStrategy":"DELIVERY"}`)
}

func TestAddToBasketBumpsExistingLine(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, testSession())
	f.on("GET", basketPath+"/stores/1001", reply{200, fixtureText(t, "basket")})
	_, err := c.AddToBasket(context.Background(), AddToBasketParams{StoreID: "1001", ProductID: "5001", ExternalID: "ext-5001", StoreProductID: "sp-5001", Quantity: 3})
	if err != nil {
		t.Fatal(err)
	}
	r := f.last()
	if r.Method != "PATCH" || r.URI != basketPath+"/basket-test-1/products/quantity" {
		t.Fatalf("%s %s", r.Method, r.URI)
	}
	jsonEqual(t, r.Body, `{"handlingStrategy":"DELIVERY","basketVersion":3,"products":[{"basketProductId":"bp-test-1","quantity":5}]}`)
}

func TestAddToBasketAppendsNewLine(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, testSession())
	f.on("GET", basketPath+"/stores/1001", reply{200, fixtureText(t, "basket")})
	_, err := c.AddToBasket(context.Background(), AddToBasketParams{StoreID: "1001", ProductID: "7001", ExternalID: "ext-7001", StoreProductID: "sp-7001"})
	if err != nil {
		t.Fatal(err)
	}
	r := f.last()
	if r.Method != "PUT" || r.URI != basketPath+"/basket-test-1/products" {
		t.Fatalf("%s %s", r.Method, r.URI)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
		t.Fatal(err)
	}
	products, _ := body["products"].([]any)
	if body["basketId"] != "basket-test-1" || len(products) != 3 {
		t.Fatalf("body %s", r.Body)
	}
	last, _ := json.Marshal(products[2])
	jsonEqual(t, string(last), `{"ids":{"id":"7001","externalId":"ext-7001","storeProductId":"sp-7001"},"quantity":{"increments":1}}`)
}

func TestAddToBasketBasketReadFailureIsError(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, testSession())
	f.on("GET", basketPath+"/stores/1001", reply{500, `{}`})
	if _, err := c.AddToBasket(context.Background(), AddToBasketParams{StoreID: "1001", ProductID: "1", ExternalID: "e", StoreProductID: "s"}); err == nil {
		t.Fatal("no error")
	}
	if n := len(f.requests()); n != 1 {
		t.Fatalf("%d requests: a failed basket read must not fall through to a create", n)
	}
}

func TestSetQuantityAndRemove(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		do   func(c *Client) error
		body string
	}{
		{"set by product", func(c *Client) error {
			_, err := c.SetQuantity(ctx, BasketLineRef{StoreID: "1001", ProductID: "5004"}, 4)
			return err
		}, `{"handlingStrategy":"DELIVERY","basketVersion":3,"products":[{"basketProductId":"bp-test-2","quantity":4}]}`},
		{"set zero", func(c *Client) error {
			_, err := c.SetQuantity(ctx, BasketLineRef{StoreID: "1001", BasketProductID: "bp-test-1"}, 0)
			return err
		}, `{"handlingStrategy":"DELIVERY","basketVersion":3,"products":[{"basketProductId":"bp-test-1","quantity":0}]}`},
		{"remove", func(c *Client) error {
			_, err := c.RemoveFromBasket(ctx, BasketLineRef{StoreID: "1001", StoreProductID: "sp-5001"})
			return err
		}, `{"handlingStrategy":"DELIVERY","basketVersion":3,"products":[{"basketProductId":"bp-test-1","quantity":0}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			c, _ := newTestClient(t, f, testSession())
			f.on("GET", basketPath+"/stores/1001", reply{200, fixtureText(t, "basket")})
			if err := tc.do(c); err != nil {
				t.Fatal(err)
			}
			r := f.last()
			if r.Method != "PATCH" || r.URI != basketPath+"/basket-test-1/products/quantity" {
				t.Fatalf("%s %s", r.Method, r.URI)
			}
			jsonEqual(t, r.Body, tc.body)
		})
	}
	f := newFake(t)
	c, _ := newTestClient(t, f, testSession())
	f.on("GET", basketPath+"/stores/1001", reply{200, fixtureText(t, "basket")})
	if _, err := c.RemoveFromBasket(ctx, BasketLineRef{StoreID: "1001", ProductID: "nope"}); err == nil {
		t.Error("removing an absent line succeeded")
	}
	f.on("GET", basketPath+"/stores/1002", reply{404, `{}`})
	if _, err := c.SetQuantity(ctx, BasketLineRef{StoreID: "1002", ProductID: "5001"}, 1); err == nil {
		t.Error("set quantity without a basket succeeded")
	}
	if n := f.count("PATCH", basketPath); n != 0 {
		t.Errorf("%d PATCHes sent", n)
	}
}

func TestAddToBasketValidatesOptionsBeforeNetwork(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, testSession())
	_, err := c.AddToBasket(context.Background(), AddToBasketParams{
		StoreID: "1001", ProductID: "6001", ExternalID: "ext-6001", StoreProductID: "sp-6001",
		ProductView:     fixture(t, "product_view"),
		SelectedOptions: []SelectedOption{{GroupID: "no-such-group", OptionID: "x"}},
	})
	if err == nil {
		t.Fatal("invalid option accepted")
	}
	if len(f.requests()) != 0 {
		t.Fatal("invalid option reached the network")
	}
}

func TestGetAllOrderCardsWalksCursor(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, testSession())
	f.on("GET", "/v3/customer/orders-list", reply{200, fixtureText(t, "orders_page1")}, reply{200, fixtureText(t, "orders_page2")})
	res, err := c.GetAllOrderCards(context.Background(), OrderCardsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) != 2 || reqs[0].URI != "/v3/customer/orders-list?offset=0&limit=15" || reqs[1].URI != "/v3/customer/orders-list?offset=cursor-b&limit=15" {
		t.Fatalf("requests %+v", reqs)
	}
	if res.StoppedReason != "no_next_cursor" || len(res.Pages) != 2 {
		t.Errorf("stopped %q pages %d", res.StoppedReason, len(res.Pages))
	}
	for _, o := range res.Orders {
		if jsString(o.OrderID) == "9001" && res.Count != len(res.Orders) {
			t.Error("count mismatch")
		}
	}
	ids := map[string]int{}
	for _, o := range res.Orders {
		ids[jsString(o.OrderID)]++
	}
	if ids["9001"] != 1 || ids["9003"] != 1 {
		t.Errorf("order ids %v", ids)
	}
}

func TestGetAllOrderCardsFailedPageIsError(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, testSession())
	var mu sync.Mutex
	var sleeps []time.Duration
	c.sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		sleeps = append(sleeps, d)
		return nil
	}
	f.on("GET", "/v3/customer/orders-list", reply{200, fixtureText(t, "orders_page1")}, reply{429, `{}`})
	res, err := c.GetAllOrderCards(context.Background(), OrderCardsOptions{MaxRetries: 2})
	if err == nil || res != nil {
		t.Fatalf("partial history returned: %v, %v", res, err)
	}
	if !isRateLimited(err) {
		t.Errorf("err = %v", err)
	}
	if n := f.count("GET", "/v3/customer/orders-list"); n != 4 {
		t.Errorf("%d page requests, want 1 + 3 attempts", n)
	}
	want := []time.Duration{15 * time.Second, 30 * time.Second}
	if len(sleeps) != 2 || sleeps[0] != want[0] || sleeps[1] != want[1] {
		t.Errorf("sleeps %v, want %v", sleeps, want)
	}
}

func TestGetAllOrderCardsMalformedPageIsError(t *testing.T) {
	for name, body := range map[string]string{
		"no orders":    `{"pagination":{}}`,
		"orders str":   `{"orders":"none"}`,
		"invalid JSON": `{"orders":[`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			c, _ := newTestClient(t, f, testSession())
			f.on("GET", "/v3/customer/orders-list", reply{200, body})
			res, err := c.GetAllOrderCards(context.Background(), OrderCardsOptions{MaxRetries: -1})
			if err == nil || res != nil {
				t.Fatalf("got %v, %v", res, err)
			}
		})
	}
}

func TestGetAllOrderCardsDoesNotRetryAuthErrors(t *testing.T) {
	f := newFake(t)
	s := testSession()
	delete(s, "refreshToken")
	c, _ := newTestClient(t, f, s)
	f.on("GET", "/v3/customer/orders-list", reply{401, `{}`})
	_, err := c.GetAllOrderCards(context.Background(), OrderCardsOptions{})
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("err = %v", err)
	}
	if n := f.count("GET", "/v3/customer/orders-list"); n != 1 {
		t.Errorf("%d requests", n)
	}
}

func TestSelectLocationPersistsDeliverable(t *testing.T) {
	f := newFake(t)
	c, path := newTestClient(t, f, testSession())
	f.on("GET", "/v3/addresslookup/pub/place-1", reply{200, fixtureText(t, "resolved_location")})
	f.on("GET", "/customer_profile/api/v1/guest/address_book/delivery_point_info", reply{200, fixtureText(t, "delivery_point")})
	res, err := c.SelectLocation(context.Background(), "place-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Selected || !res.Deliverable || *res.CityCode != "BCN" {
		t.Fatalf("result %+v", res)
	}
	if got := f.last().URI; got != "/customer_profile/api/v1/guest/address_book/delivery_point_info?latitude=41.4&longitude=2.17&countryCode=ES" {
		t.Errorf("delivery point uri %s", got)
	}
	persisted, err := LoadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := persisted["location"].(map[string]any)
	if loc["cityCode"] != "BCN" || loc["countryCode"] != "ES" || loc["latitude"] != "41.4" || loc["accuracy"] != "12" {
		t.Errorf("persisted location %v", loc)
	}
	assertMode(t, path, 0o600)
}

func TestSelectLocationDoesNotPersistOnFailure(t *testing.T) {
	cases := map[string]struct {
		resolved, point reply
		wantErr         bool
		reason          string
	}{
		"not deliverable": {reply{200, fixtureText(t, "resolved_location")}, reply{200, `{"data":{"isDeliverable":false}}`}, false, "not_deliverable"},
		"invalid place":   {reply{200, fixtureText(t, "resolved_invalid")}, reply{200, `{}`}, false, ""},
		"point failure":   {reply{200, fixtureText(t, "resolved_location")}, reply{502, `{}`}, true, ""},
		"resolve failure": {reply{500, `{}`}, reply{200, `{}`}, true, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			c, path := newTestClient(t, f, testSession())
			before, _ := os.ReadFile(path) // #nosec G304 -- test-owned temp or testdata path
			f.on("GET", "/v3/addresslookup/pub/place-1", tc.resolved)
			f.on("GET", "/customer_profile/api/v1/guest/address_book/delivery_point_info", tc.point)
			res, err := c.SelectLocation(context.Background(), "place-1", "")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v", err)
			}
			if err == nil && (res.Selected || (tc.reason != "" && res.Reason != tc.reason)) {
				t.Errorf("result %+v", res)
			}
			after, _ := os.ReadFile(path) // #nosec G304 -- test-owned temp or testdata path
			if !bytes.Equal(before, after) {
				t.Error("session rewritten")
			}
		})
	}
}

func TestSetLocationMergesAndRemoves(t *testing.T) {
	f := newFake(t)
	c, path := newTestClient(t, f, testSession())
	loc, err := c.SetLocation(map[string]any{"cityCode": "NEW", "longitude": nil, "label": ""})
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := LoadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	pl, _ := persisted["location"].(map[string]any)
	if pl["cityCode"] != "NEW" || pl["countryCode"] != "XX" || pl["latitude"] != "10.5" {
		t.Errorf("persisted %v", pl)
	}
	if _, ok := pl["longitude"]; ok {
		t.Error("nil did not remove the field")
	}
	if _, ok := pl["label"]; ok {
		t.Error("empty string kept")
	}
	if loc["cityCode"] != "NEW" || persisted["accessToken"] != validToken {
		t.Errorf("loc %v", loc)
	}
	if len(f.requests()) != 0 {
		t.Error("SetLocation made a request")
	}
}

func TestSavedLocationsUsesAuth(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, testSession())
	f.on("GET", "/customer_profile/api/v1/address_book/me/addresses", reply{200, fixtureText(t, "saved_addresses")})
	res, err := c.SavedLocations(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	out, _ := marshalJSON(res)
	if strings.Contains(string(out), "must-not-leak") || f.last().Header.Get("Authorization") != validToken {
		t.Errorf("saved locations: %s", out)
	}
	f.on("GET", "/customer_profile/api/v1/address_book/me/addresses", reply{500, `{}`})
	if _, err := c.SavedLocations(context.Background(), ""); err == nil {
		t.Error("failed read returned saved locations")
	}
}
