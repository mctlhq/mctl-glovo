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

func (s errCards) OrderCards(context.Context, CardOptions) (json.RawMessage, error) {
	return nil, s.err
}

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
	dir := filepath.Join(t.TempDir(), "cache")
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

func cacheWalk(t *testing.T, dir string) (*CachedDiscovery, error) {
	t.Helper()
	customer := "synthetic-customer-1"
	src := &recordingCards{raw: json.RawMessage(`{"orders":[{"order_id":"1"}],"pages":[{}],"count":1,"stopped_reason":"no_next_cursor"}`)}
	return HistoryCache{Dir: dir}.CachedOrderCards(context.Background(), src, &customer, CacheOptions{})
}

func wantUnsafe(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrUnsafeCache) {
		t.Fatalf("want ErrUnsafeCache, got %v", err)
	}
}

func TestHistoryCacheCreatesPrivateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	if _, err := cacheWalk(t, dir); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, cacheFileName): 0o600} {
		st, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != want {
			t.Errorf("%s mode %o, want %o", path, st.Mode().Perm(), want)
		}
	}
}

func TestHistoryCacheRefusesOpenDirWithoutChmod(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // #nosec G302 -- deliberately loose: the cache must refuse it
		t.Fatal(err)
	}
	_, err := cacheWalk(t, dir)
	wantUnsafe(t, err)
	st, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("an existing dir must not be chmodded: mode %o", st.Mode().Perm())
	}
	if _, err := os.Lstat(filepath.Join(dir, cacheFileName)); !os.IsNotExist(err) {
		t.Fatalf("nothing may be written into a refused dir (%v)", err)
	}
}

func TestHistoryCacheRefusesSymlinkedDir(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	_, err := cacheWalk(t, link)
	wantUnsafe(t, err)
}

func TestHistoryCacheRefusesSymlinkedOrOpenFile(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "cache")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, cacheFileName)
	if err := os.Symlink(target, file); err != nil {
		t.Fatal(err)
	}
	_, err := cacheWalk(t, dir)
	wantUnsafe(t, err)

	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o644); err != nil { // #nosec G302 -- deliberately loose: the cache must refuse it
		t.Fatal(err)
	}
	_, err = cacheWalk(t, dir)
	wantUnsafe(t, err)
	st, err := os.Lstat(file)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("an existing file must not be chmodded: mode %o", st.Mode().Perm())
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

func TestHistoryCacheRefusesWrongEntryTypes(t *testing.T) {
	base := t.TempDir()
	notDir := filepath.Join(base, "file-as-dir")
	if err := os.WriteFile(notDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := cacheWalk(t, notDir)
	wantUnsafe(t, err)

	dir := filepath.Join(base, "cache")
	if err := os.MkdirAll(filepath.Join(dir, cacheFileName), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = cacheWalk(t, dir)
	wantUnsafe(t, err)
}
