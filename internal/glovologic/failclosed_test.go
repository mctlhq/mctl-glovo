package glovologic

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func decodeOrFail(t *testing.T, s string) any {
	t.Helper()
	v, err := decodeJSON([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func wantInputError(t *testing.T, err error, path string) {
	t.Helper()
	var in *InputError
	if !errors.As(err, &in) {
		t.Fatalf("want *InputError at %s, got %v", path, err)
	}
	if in.Path != path {
		t.Fatalf("InputError path = %q, want %q (%v)", in.Path, path, err)
	}
}

func TestOrderStatsRejectsWrongTypedStore(t *testing.T) {
	cards := objects(t, decodeOrFail(t, `[{"order_id":"1","store":"A","total":"EUR 1"},{"order_id":"2","store":5}]`))
	_, err := OrderStatsFromCards(cards)
	wantInputError(t, err, "orders[1].store")
}

func TestOrderStatsAcceptsNullAndAbsentStore(t *testing.T) {
	cards := objects(t, decodeOrFail(t, `[{"order_id":"1","store":null},{"order_id":"2"}]`))
	got, err := OrderStatsFromCards(cards)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stores != 1 || got.TopStores[0].Store != "Unknown store" || got.TopStores[0].Orders != 2 {
		t.Fatalf("null and absent stores must count as Unknown store: %+v", got)
	}
}

func TestOrderAnalysisRejectsWrongTypedPrice(t *testing.T) {
	details := decodeOrFail(t, `[{"id":1,"boughtProducts":[{"name":"Milk","price":{"formatted":true}}]}]`).([]any)
	_, err := OrderAnalysisFromDetails(details)
	wantInputError(t, err, "details[0].boughtProducts[0].price.formatted")
}

func TestOrderAnalysisRejectsWrongTypedIncrements(t *testing.T) {
	details := decodeOrFail(t, `[{"id":1,"boughtProducts":[{"name":"Milk","quantity":{"increments":[2]}}]}]`).([]any)
	_, err := OrderAnalysisFromDetails(details)
	wantInputError(t, err, "details[0].boughtProducts[0].quantity.increments")
}

func TestDecodeDiscoveryFailsClosed(t *testing.T) {
	for name, raw := range map[string]string{
		"invalid json":   `{"orders":[`,
		"trailing data":  `{"orders":[],"pages":[]} {}`,
		"not an object":  `[]`,
		"orders object":  `{"orders":{},"pages":[],"stopped_reason":"no_next_cursor"}`,
		"missing orders": `{"pages":[],"stopped_reason":"no_next_cursor"}`,
		"card store num": `{"orders":[{"order_id":"1","store":1}],"pages":[],"stopped_reason":"no_next_cursor"}`,
	} {
		if d, err := DecodeDiscovery([]byte(raw)); err == nil {
			t.Errorf("%s: want an error, got %+v", name, d)
		}
	}
}

type errCards struct{ err error }

func (s errCards) OrderCards(context.Context, CardOptions) (json.RawMessage, error) { return nil, s.err }

func TestGetOrderStatsPropagatesSourceError(t *testing.T) {
	boom := errors.New("page 2 failed")
	if _, err := GetOrderStats(context.Background(), errCards{boom}, StatsOptions{}); !errors.Is(err, boom) {
		t.Fatalf("want the source error, got %v", err)
	}
}

type badDetailSource struct{}

func (badDetailSource) OrderCards(context.Context, CardOptions) (json.RawMessage, error) {
	return json.RawMessage(`{"orders":[{"order_id":"1","store":"A"}],"pages":[{}],"stopped_reason":"no_next_cursor"}`), nil
}

func (badDetailSource) OrderDetail(context.Context, string) (json.RawMessage, error) {
	return json.RawMessage(`{"id":1,"boughtProducts":"milk"}`), nil
}

func TestAnalyzeOrderHistoryFailsOnMalformedDetail(t *testing.T) {
	_, err := AnalyzeOrderHistory(context.Background(), badDetailSource{}, AnalyzeOptions{DetailLimit: 1, Sleep: noSleep})
	var in *InputError
	if !errors.As(err, &in) {
		t.Fatalf("a malformed detail must fail the call, got %v", err)
	}
}

func TestPlanReorderRejectsWrongTypedOrder(t *testing.T) {
	f := fixture(t, "reorder.json")
	f["orders"].(Object)["201"].(Object)["store"] = "not an object"
	src := &reorderSource{t: t, f: f, inputs: map[string]any{"easy_reorder": map[string]any{}, "search": map[string]any{}}}
	_, err := PlanReorder(context.Background(), src, "201", DefaultPlanOptions())
	var in *InputError
	if !errors.As(err, &in) {
		t.Fatalf("want *InputError, got %v", err)
	}
	if len(src.calls) != 0 {
		t.Fatalf("no store call may follow a malformed order, got %v", src.calls)
	}
}

func TestPlanReorderValidatesOptions(t *testing.T) {
	f := fixture(t, "reorder.json")
	src := &reorderSource{t: t, f: f}
	for _, o := range []PlanOptions{{MaxSearches: -1, CandidatesPerLine: 3}, {MaxSearches: 21, CandidatesPerLine: 3}, {MaxSearches: 5, CandidatesPerLine: 0}, {MaxSearches: 5, CandidatesPerLine: 6}} {
		if _, err := PlanReorder(context.Background(), src, "201", o); err == nil {
			t.Errorf("options %+v must be rejected", o)
		}
	}
}

func TestHistoryCacheTreatsWrongTypedOrdersAsCorrupt(t *testing.T) {
	dir := t.TempDir()
	customer := "synthetic-customer-1"
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := HistoryCache{Dir: dir, Now: func() time.Time { return now }}
	src := &recordingCards{raw: json.RawMessage(`{"orders":[{"order_id":"1","store":"A"}],"pages":[{}],"count":1,"stopped_reason":"no_next_cursor","strategy":"order-id-cursor"}`)}
	if _, err := h.CachedOrderCards(context.Background(), src, &customer, CacheOptions{}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, cacheFileName)
	data, err := os.ReadFile(file) // #nosec G304 -- file is under t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	c["orders"] = []any{map[string]any{"order_id": "1", "store": 7}}
	data, _ = json.Marshal(c)
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := h.CachedOrderCards(context.Background(), src, &customer, CacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Cache.Status != "corrupt_rebuilt" {
		t.Fatalf("status = %q, want corrupt_rebuilt", got.Cache.Status)
	}
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("cache file mode %o, want 600", fi.Mode().Perm())
	}
}

func TestHistoryCacheDisabledWithoutCustomer(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "never")
	src := &recordingCards{raw: json.RawMessage(`{"orders":[],"pages":[],"count":0,"stopped_reason":"no_next_cursor"}`)}
	got, err := HistoryCache{Dir: dir}.CachedOrderCards(context.Background(), src, nil, CacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Cache.Status != "disabled" {
		t.Fatalf("status = %q, want disabled", got.Cache.Status)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("a disabled cache must not create %s (%v)", dir, err)
	}
}

func TestHistoryCacheTightensDirMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(dir, 0o755); err != nil { // #nosec G301 -- deliberately loose, the cache must tighten it
		t.Fatal(err)
	}
	customer := "synthetic-customer-1"
	src := &recordingCards{raw: json.RawMessage(`{"orders":[{"order_id":"1"}],"pages":[{}],"count":1,"stopped_reason":"no_next_cursor"}`)}
	if _, err := (HistoryCache{Dir: dir}).CachedOrderCards(context.Background(), src, &customer, CacheOptions{}); err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %o, want 700", di.Mode().Perm())
	}
}

func TestMatchGooglePlaceRejectsWrongTypedPlace(t *testing.T) {
	places := decodeOrFail(t, `[{"id":"a","displayName":"Pizza","location":{"latitude":1,"longitude":2}}]`).([]any)
	if _, err := MatchGooglePlace("Pizza", Location{Latitude: 1.0, Longitude: 2.0}, places); err == nil {
		t.Fatal("a string displayName must be rejected")
	}
}

func TestCompactGlobalStoreSearchFailsClosed(t *testing.T) {
	for _, raw := range []string{`{"elements":`, `"text"`, `{"body":{"data":{"elements":{}}}}`} {
		if _, err := CompactGlobalStoreSearch([]byte(raw)); err == nil {
			t.Errorf("%s: want an error", raw)
		}
	}
}
