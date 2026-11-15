package glovoapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The files in testdata/golden were produced by running the upstream compact*
// functions of glovo-skill (src/glovo/api.mjs) on the synthetic fixtures in
// testdata with JSON.stringify. Each case asserts that the Go port encodes to
// the same JSON: same keys present, same keys absent, same nulls and values.

func fixture(t *testing.T, name string) Value {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".json")) // #nosec G304 -- test-owned temp or testdata path
	if err != nil {
		t.Fatal(err)
	}
	v, err := decodeJSON(b)
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return v
}

func canonical(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	return v
}

func assertGolden(t *testing.T, name string, got any) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", "golden", name+".json")) // #nosec G304 -- test-owned temp or testdata path
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, err := marshalJSON(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if g, w := canonical(t, gotJSON), canonical(t, want); !reflect.DeepEqual(g, w) {
		pretty, _ := json.MarshalIndent(g, "", "  ")
		t.Errorf("%s mismatch\n got: %s\nwant: %s", name, pretty, want)
	}
}

var testCurrentLocation = Location{"countryCode": "ES", "cityCode": "BCN", "latitude": "41.4", "longitude": "2.17", "accuracy": "0", "languageCode": "en"}

func TestCompactMatchesUpstreamGolden(t *testing.T) {
	cases := map[string]func(t *testing.T) any{
		"store_wall":  func(t *testing.T) any { return CompactStoreWall(fixture(t, "store_wall")) },
		"store":       func(t *testing.T) any { return CompactStoreEntry(fixture(t, "store")) },
		"menu":        func(t *testing.T) any { return CompactMenu(fixture(t, "menu"), 80) },
		"menu_limit2": func(t *testing.T) any { return CompactMenu(fixture(t, "menu"), 2) },
		"store_content_all": func(t *testing.T) any {
			return CompactStoreContent(fixture(t, "store_content"), StoreContentCompactOptions{Kind: "all", Limit: 40, StoreID: 1001, StoreAddressID: 2001})
		},
		"store_content_easy": func(t *testing.T) any {
			return CompactStoreContent(fixture(t, "store_content"), StoreContentCompactOptions{Kind: "easy_reorder", Limit: 40})
		},
		"store_content_limit1": func(t *testing.T) any {
			return CompactStoreContent(fixture(t, "store_content"), StoreContentCompactOptions{Kind: "all", Limit: 1})
		},
		"store_content_empty": func(t *testing.T) any {
			return CompactStoreContent(map[string]any{"data": map[string]any{"body": []any{}}}, StoreContentCompactOptions{Kind: "top_sellers", Limit: 40})
		},
		"search": func(t *testing.T) any {
			return CompactSearch(fixture(t, "search"), SearchOptions{StoreID: "1001", StoreAddressID: 2001, Limit: 24})
		},
		"search_limit1": func(t *testing.T) any {
			return CompactSearch(fixture(t, "search"), SearchOptions{StoreID: "1001", StoreAddressID: "2001", Limit: 1})
		},
		"product_view":  func(t *testing.T) any { return CompactProductView(fixture(t, "product_view")) },
		"option_groups": func(t *testing.T) any { return OptionGroupsFromProduct(fixture(t, "product_view")) },
		"order_options": func(t *testing.T) any {
			return CompactStoreOrderOptions(fixture(t, "store_fees"), fixture(t, "store_restrictions"), fixture(t, "store_info"), fixture(t, "store_similar"))
		},
		"basket":      func(t *testing.T) any { return CompactBasket(fixture(t, "basket")) },
		"basket_null": func(t *testing.T) any { return CompactBasket(nil) },
		"order_cards": func(t *testing.T) any {
			var out []*CompactOrder
			for _, o := range list(get(fixture(t, "orders_page1"), "orders")) {
				out = append(out, CompactOrderCard(o))
			}
			return out
		},
		"order_detail":         func(t *testing.T) any { return CompactOrderDetail(fixture(t, "order_detail")) },
		"reorder_preview":      func(t *testing.T) any { return CompactReorderPreview(fixture(t, "order_detail")) },
		"reorder_preview_null": func(t *testing.T) any { return CompactReorderPreview(nil) },
		"reorder_preview_noids": func(t *testing.T) any {
			return CompactReorderPreview(map[string]any{"orderCode": "x", "boughtProducts": []any{}})
		},
		"saved_locations_match": func(t *testing.T) any {
			return CompactSavedLocations(fixture(t, "saved_addresses"), testCurrentLocation, "example street 00000")
		},
		"saved_locations_plain": func(t *testing.T) any {
			return CompactSavedLocations(fixture(t, "saved_addresses"), Location{"countryCode": "PT"}, "")
		},
		"location_search":        func(t *testing.T) any { return CompactLocationSearch(fixture(t, "address_search"), 5) },
		"location_search_limit2": func(t *testing.T) any { return CompactLocationSearch(fixture(t, "address_search"), 2) },
		"resolved_location":      func(t *testing.T) any { return CompactResolvedLocation(fixture(t, "resolved_location")) },
		"resolved_invalid":       func(t *testing.T) any { return CompactResolvedLocation(fixture(t, "resolved_invalid")) },
		"delivery_point":         func(t *testing.T) any { return CompactDeliveryPointInfo(fixture(t, "delivery_point")) },
	}
	goldens, err := filepath.Glob(filepath.Join("testdata", "golden", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(goldens) != len(cases) {
		t.Fatalf("%d golden files but %d cases", len(goldens), len(cases))
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) { assertGolden(t, name, fn(t)) })
	}
}

func TestCompactItemKeepsRawPriceInfo(t *testing.T) {
	res := CompactSearch(fixture(t, "search"), SearchOptions{})
	raw := res.Results[0].PriceInfo
	if !strings.Contains(string(raw), `"discountedPrice"`) {
		t.Fatalf("raw priceInfo lost: %s", raw)
	}
	if res.Results[1].PriceInfo != nil {
		t.Fatalf("product without priceInfo has raw %s", res.Results[1].PriceInfo)
	}
	out, err := marshalJSON(res.Results[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "discountedPrice") {
		t.Fatalf("raw priceInfo leaked into tool output: %s", out)
	}
	pv := CompactProductView(fixture(t, "product_view"))
	if !strings.Contains(string(pv.PriceInfo), `"displayText"`) {
		t.Fatalf("product view raw priceInfo lost: %s", pv.PriceInfo)
	}
}

func TestCompactSavedLocationsDoesNotLeakTokens(t *testing.T) {
	out, err := marshalJSON(CompactSavedLocations(fixture(t, "saved_addresses"), testCurrentLocation, "home"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "must-not-leak") {
		t.Fatalf("token leaked: %s", out)
	}
	res := CompactLocationSearch(fixture(t, "address_search"), 9)
	out, _ = marshalJSON(res)
	if strings.Contains(string(out), "hidden") || res.Count != 4 {
		t.Fatalf("location search: %s", out)
	}
}

func TestStoreClassFromCategoryID(t *testing.T) {
	for in, want := range map[Value]string{"1": "restaurant", 4: "grocery", "3": "retail", 2: "", nil: ""} {
		if got := StoreClassFromCategoryID(in); got != want {
			t.Errorf("StoreClassFromCategoryID(%v) = %q, want %q", in, got, want)
		}
	}
}
