package glovologic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// The golden files under testdata/golden were produced by running the
// upstream JavaScript (glovo-skill) on the synthetic fixtures in testdata.

func fixture(t *testing.T, name string) Object {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name)) // #nosec G304 -- fixed test fixture names
	if err != nil {
		t.Fatal(err)
	}
	v, err := decodeJSON(data)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return v.(Object)
}

func golden(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "golden", name)) // #nosec G304 -- fixed test fixture names
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func rawOf(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := marshalJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// plain converts a Go value into the shape encoding/json decodes golden
// JSON into, so the two compare with reflect.DeepEqual.
func plain(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func rounded(v any, digits int) any {
	switch t := v.(type) {
	case float64:
		if t != math.Trunc(t) {
			return toFixed(t, digits)
		}
		return t
	case map[string]any:
		for k, e := range t {
			t[k] = rounded(e, digits)
		}
	case []any:
		for i, e := range t {
			t[i] = rounded(e, digits)
		}
	}
	return v
}

func objects(t *testing.T, v any) []Object {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("want array, got %T", v)
	}
	out := make([]Object, len(list))
	for i, e := range list {
		out[i] = e.(Object)
	}
	return out
}

func assertEqual(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		var diffs []string
		diffPaths("$", got, want, &diffs)
		if len(diffs) > 10 {
			diffs = append(diffs[:10], "...")
		}
		t.Errorf("%s mismatch:\n%s", what, strings.Join(diffs, "\n"))
	}
}

func diffPaths(path string, got, want any, out *[]string) {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			break
		}
		keys := map[string]bool{}
		for k := range w {
			keys[k] = true
		}
		for k := range g {
			keys[k] = true
		}
		names := make([]string, 0, len(keys))
		for k := range keys {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			gv, gok := g[k]
			wv, wok := w[k]
			if gok != wok {
				*out = append(*out, fmt.Sprintf("%s.%s: present got=%v want=%v (%v / %v)", path, k, gok, wok, gv, wv))
				continue
			}
			diffPaths(path+"."+k, gv, wv, out)
		}
		return
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			break
		}
		for i := range w {
			diffPaths(fmt.Sprintf("%s[%d]", path, i), g[i], w[i], out)
		}
		return
	}
	if !reflect.DeepEqual(got, want) {
		gj, _ := json.Marshal(got)
		wj, _ := json.Marshal(want)
		*out = append(*out, fmt.Sprintf("%s: got %.300s want %.300s", path, gj, wj))
	}
}

// jsError maps an upstream Error message onto this package's Go style:
// lowercase, no trailing period.
func jsError(msg string) string {
	return strings.ToLower(strings.TrimSuffix(msg, "."))
}

func noSleep(context.Context, time.Duration) error { return nil }

type cardSource struct{ raw json.RawMessage }

func (s cardSource) OrderCards(context.Context, CardOptions) (json.RawMessage, error) {
	return s.raw, nil
}

func TestGoldenStats(t *testing.T) {
	f := fixture(t, "stats.json")
	want := golden(t, "stats.json")

	money := []any{}
	for _, v := range f["money"].([]any) {
		if amount, ok := ParseMoney(v); ok {
			money = append(money, amount)
		} else {
			money = append(money, nil)
		}
	}
	assertEqual(t, "parseMoney", plain(t, money), want["money"])

	got, err := GetOrderStats(context.Background(), cardSource{rawOf(t, f["discovery"])}, StatsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, "order stats", plain(t, got), want["handler"])
}

type historySource struct {
	t      *testing.T
	f      Object
	mu     sync.Mutex
	called []string
}

func (s *historySource) OrderCards(context.Context, CardOptions) (json.RawMessage, error) {
	return rawOf(s.t, s.f["discovery"]), nil
}

func (s *historySource) OrderDetail(_ context.Context, id string) (json.RawMessage, error) {
	s.mu.Lock()
	s.called = append(s.called, id)
	s.mu.Unlock()
	switch get(s.f, "detail_errors", id) {
	case "rate_limit":
		return nil, fmt.Errorf("429: %w", ErrRateLimited)
	case "server":
		return nil, errors.New("500")
	}
	return rawOf(s.t, get(s.f, "details", id)), nil
}

func TestGoldenHistory(t *testing.T) {
	f := fixture(t, "history.json")
	want := golden(t, "history.json")
	// analyze and analyze_limit_2 are the Go behaviour: cancelled orders are
	// excluded from the counts, card_statistics.detailed_orders counts the
	// details read, and the output carries additive coverage, uncertainty and
	// most_ordered fields (mctl-glovo#45). history.json is the upstream
	// output and still pins analysis_all.
	wantGo := golden(t, "history_go.json")

	var details []any
	for _, id := range []string{"101", "102", "103", "104", "105", "106", "107"} {
		details = append(details, get(f, "details", id))
	}
	all, err := OrderAnalysisFromDetails(details)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, "analysis_all", plain(t, all), want["analysis_all"])

	for name, limit := range map[string]int{"analyze": 10, "analyze_limit_2": 2} {
		src := &historySource{t: t, f: f}
		got, err := AnalyzeOrderHistory(context.Background(), src, AnalyzeOptions{DetailLimit: limit, Sleep: noSleep})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		assertEqual(t, name, plain(t, got), wantGo[name])
	}
}

type reorderSource struct {
	t      *testing.T
	f      Object
	inputs map[string]any
	calls  []any
}

func (s *reorderSource) OrderDetail(_ context.Context, id string) (json.RawMessage, error) {
	return rawOf(s.t, get(s.f, "orders", id)), nil
}

func (s *reorderSource) StoreContent(_ context.Context, storeID, addressID string) (ContentProducts, error) {
	s.calls = append(s.calls, []any{"content", storeID, addressID, true})
	return func(kind string, limit int) (json.RawMessage, error) {
		if kind != "easy_reorder" || limit != 100 {
			return nil, fmt.Errorf("unexpected content request %s/%d", kind, limit)
		}
		products, ok := s.inputs["easy_reorder"].(map[string]any)[storeID]
		if !ok {
			return json.RawMessage("[]"), nil
		}
		return json.Marshal(products)
	}, nil
}

func (s *reorderSource) SearchStoreItems(_ context.Context, storeID, addressID, query string) (json.RawMessage, error) {
	s.calls = append(s.calls, []any{"search", storeID, addressID, query})
	results, ok := s.inputs["search"].(map[string]any)[storeID+"|"+query]
	if !ok {
		return json.RawMessage("[]"), nil
	}
	return json.Marshal(results)
}

func TestGoldenReorder(t *testing.T) {
	f := fixture(t, "reorder.json")
	want := golden(t, "reorder.json")
	inputs := want["inputs"].(map[string]any)

	for _, c := range f["cases"].([]any) {
		name := jsString(get(c, "name"))
		maxSearches := int(toNumber(get(c, "max_searches")))
		src := &reorderSource{t: t, f: f, inputs: inputs}
		got, err := PlanReorder(context.Background(), src, jsString(get(c, "order_id")), PlanOptions{
			MaxSearches:       maxSearches,
			CandidatesPerLine: int(toNumber(get(c, "candidates_per_line"))),
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.MutatesBasket {
			t.Errorf("%s: mutates_basket must be false", name)
		}
		searches := 0
		for _, call := range src.calls {
			if call.([]any)[0] == "search" {
				searches++
			}
		}
		if searches > maxSearches {
			t.Errorf("%s: %d searches exceed max_searches %d", name, searches, maxSearches)
		}
		assertEqual(t, name+" plan", plain(t, got), get(want, "plans", name))
		assertEqual(t, name+" calls", plain(t, append([]any{}, src.calls...)), get(want, "calls", name))
	}

	direct, err := BuildReorderPlan(get(f, "orders", "201"), map[int][]Object{
		0: {{"product_id": "1", "external_id": "e", "store_product_id": "s", "name": "Whole Milk 1L", "available": true, "match_score": 1.0}},
		1: {{"product_id": "2", "external_id": "e", "store_product_id": "s", "name": "Pan", "available": false, "match_score": 0.95}},
		2: {{"product_id": "3", "external_id": "", "store_product_id": "s", "name": "Huevos", "match_score": 0.9}},
		3: {{"product_id": "4", "external_id": "e", "store_product_id": "s", "name": "Tomate", "match_score": 0.899}},
	}, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, "direct plan", plain(t, direct), want["direct"])

	var scores []float64
	for _, pair := range f["match_pairs"].([]any) {
		p := pair.([]any)
		scores = append(scores, ProductMatchScore(p[0], p[1]))
	}
	assertEqual(t, "match scores", plain(t, scores), want["match_scores"])
}

func TestGoldenVenue(t *testing.T) {
	f := fixture(t, "venue.json")
	want := golden(t, "venue.json")
	cards := objects(t, f["cards"])

	prepared, err := PrepareVenueOrders(cards, true)
	if err != nil {
		t.Fatal(err)
	}
	var rows []any
	for _, o := range prepared {
		rows = append(rows, []any{o.Store, nullable(o.DisplayName), o.SourceIndex})
	}
	assertEqual(t, "prepared", plain(t, rows), want["prepared"])

	indexed, err := PrepareVenueOrders(objects(t, f["indexed_cards"]), true)
	if err != nil {
		t.Fatal(err)
	}
	rows = nil
	for _, o := range indexed {
		rows = append(rows, []any{o.Store, o.SourceIndex})
	}
	assertEqual(t, "prepared_indexed", plain(t, rows), want["prepared_indexed"])

	split, err := TemporalSplit(prepared, 0.7, 0.15)
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, "split", plain(t, []int{len(split.Train), len(split.Validation), len(split.Test)}), want["split"])

	for _, name := range []string{"rank", "rank_small"} {
		source := "cards"
		if name == "rank_small" {
			source = "rank_small"
		}
		h, err := RankVenueHistory(objects(t, f[source]))
		if err != nil {
			t.Fatal(err)
		}
		venues := []any{}
		for _, k := range h.Keys {
			venues = append(venues, []any{k, h.Venues[k]})
		}
		got := rounded(plain(t, map[string]any{"cards": h.Cards, "venues": venues}), 9)
		assertEqual(t, name, got, want[name])
	}

	for name, run := range map[string]func() (*VenueEvaluation, error){
		"evaluation":              func() (*VenueEvaluation, error) { return RunVenueEvaluation(cards, true) },
		"evaluation_oldest_first": func() (*VenueEvaluation, error) { return RunVenueEvaluation(cards, false) },
		"evaluation_indexed":      func() (*VenueEvaluation, error) { return RunVenueEvaluation(objects(t, f["indexed_cards"]), true) },
	} {
		got, err := run()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		assertEqual(t, name, plain(t, got), want[name])
	}

	_, err = RunVenueEvaluation(objects(t, f["too_few"]), true)
	if err == nil || err.Error() != jsError(want["too_few_error"].(string)) {
		t.Errorf("too_few error = %v, want %q", err, want["too_few_error"])
	}

	var normalized []string
	for _, s := range []string{"  Café  Délice!! ", "ÀÉÎÕÜ ñ", "", "a\tb\nc"} {
		normalized = append(normalized, NormalizeVenue(s))
	}
	assertEqual(t, "normalized", plain(t, normalized), want["normalized"])
}

type suggestionSource struct {
	t      *testing.T
	f      Object
	inputs map[string]any
}

func (s *suggestionSource) OrderCards(context.Context, CardOptions) (json.RawMessage, error) {
	return rawOf(s.t, s.f["discovery"]), nil
}

func (s *suggestionSource) StoreContent(_ context.Context, storeID, _ string) (ContentProducts, error) {
	if _, ok := s.f["store_content"].(Object)[storeID]; !ok {
		return nil, errors.New("no personalized carousel")
	}
	return func(kind string, limit int) (json.RawMessage, error) {
		products, ok := s.inputs["content"].(map[string]any)[fmt.Sprintf("%s|%s|%d", storeID, kind, limit)]
		if !ok {
			return nil, fmt.Errorf("unexpected content request %s/%d", kind, limit)
		}
		return json.Marshal(products)
	}, nil
}

func (s *suggestionSource) SearchStoreItems(_ context.Context, storeID, _, _ string) (json.RawMessage, error) {
	results, ok := s.inputs["search"].(map[string]any)[storeID]
	if !ok {
		return json.RawMessage("[]"), nil
	}
	return json.Marshal(results)
}

func (s *suggestionSource) SearchStores(context.Context, string) (json.RawMessage, error) {
	return rawOf(s.t, s.f["global_search"]), nil
}

func (s *suggestionSource) ProductView(_ context.Context, r ProductRequest) (json.RawMessage, error) {
	view, ok := s.inputs["views"].(map[string]any)[r.ProductID]
	if !ok {
		return nil, errors.New("missing product")
	}
	return json.Marshal(view)
}

// fakeGoogle mirrors the deterministic provider in the golden generator.
func fakeGoogle(_ context.Context, r GoogleRequest) (Object, error) {
	name := jsString(r.VenueName)
	switch name {
	case "Third Pizza":
		return nil, errors.New("boom")
	case "Fourth Pizza":
		return Object{"match_status": "no_match"}, nil
	}
	status := "not_requested"
	if r.IncludeReviews {
		status = "returned"
	}
	return Object{
		"match_status": "matched", "source": "Google Maps", "place_id": fmt.Sprintf("place-%d", len(name)),
		"city": r.CityCode, "lat": r.Location.Latitude, "reviews_status": status,
		"ranking_effect": "display_only_not_backtested",
	}, nil
}

func suggestionInput(v any) SuggestionInput {
	in := SuggestionInput{
		Mode:                 optString(get(v, "mode")),
		Query:                optString(get(v, "query")),
		VenueQuery:           optString(get(v, "venueQuery")),
		ItemMode:             optString(get(v, "itemMode")),
		KnownLikedOnly:       get(v, "knownLikedOnly") == true,
		QualityPreference:    optString(get(v, "qualityPreference")),
		IncludeGoogle:        get(v, "includeGoogle") == true,
		IncludeGoogleReviews: get(v, "includeGoogleReviews") == true,
		HistoryRefresh:       optString(get(v, "historyRefresh")),
	}
	if n := get(v, "noveltyTolerance"); n != nil {
		x := toNumber(n)
		in.NoveltyTolerance = &x
	}
	if n := get(v, "maxChoices"); n != nil {
		x := int(toNumber(n))
		in.MaxChoices = &x
	}
	return in
}

func TestGoldenSuggestions(t *testing.T) {
	f := fixture(t, "suggestions.json")
	want := golden(t, "suggestions.json")
	inputs := want["inputs"].(map[string]any)

	global, err := CompactGlobalStoreSearch(rawOf(t, f["global_search"]))
	if err != nil {
		t.Fatal(err)
	}
	assertEqual(t, "global compact", plain(t, global), want["global_compact"])

	loc := f["location"]
	sess := SessionState{SignedIn: true, HasLocation: true, Location: Location{
		Latitude: get(loc, "latitude"), Longitude: get(loc, "longitude"), CityCode: jsString(get(loc, "cityCode")),
	}}
	for _, c := range f["cases"].([]any) {
		name := jsString(get(c, "name"))
		o := SuggestOptions{Sleep: noSleep}
		if get(c, "google") == "fake" {
			o.GoogleProvider = fakeGoogle
		}
		got, err := GetSuggestions(context.Background(), &suggestionSource{t: t, f: f, inputs: inputs}, sess, suggestionInput(get(c, "input")), o)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.MutatesBasket {
			t.Errorf("%s: mutates_basket must be false", name)
		}
		if got.Coverage.ProductLookupFailures != 0 {
			t.Errorf("%s: %d product lookups failed on a healthy source", name, got.Coverage.ProductLookupFailures)
		}
		// The failure counters are a Go addition; upstream has no field.
		plainGot := plain(t, got).(map[string]any)
		cov := plainGot["coverage"].(map[string]any)
		delete(cov, "store_content_failures")
		delete(cov, "product_lookup_failures")
		assertEqual(t, name, plainGot, get(want, "cases", name))
	}

	_, err = GetSuggestions(context.Background(), &suggestionSource{t: t, f: f, inputs: inputs}, sess, SuggestionInput{Query: "   "}, SuggestOptions{Sleep: noSleep})
	if err == nil || err.Error() != jsError(want["query_error"].(string)) {
		t.Errorf("blank query error = %v, want %q", err, want["query_error"])
	}
}

type recordingCards struct {
	raw   json.RawMessage
	stops []any
}

func (s *recordingCards) OrderCards(_ context.Context, o CardOptions) (json.RawMessage, error) {
	if o.StopOrderIDs == nil {
		s.stops = append(s.stops, nil)
	} else {
		ids := make([]string, 0, len(o.StopOrderIDs))
		for id := range o.StopOrderIDs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		s.stops = append(s.stops, ids)
	}
	return s.raw, nil
}

func TestGoldenHistoryCache(t *testing.T) {
	f := fixture(t, "history_cache.json")
	want := golden(t, "history_cache.json")["steps"].([]any)
	dir := filepath.Join(t.TempDir(), "cache")
	file := filepath.Join(dir, cacheFileName)
	customer := jsString(f["customer_id"])
	src := &recordingCards{}

	steps := f["steps"].([]any)
	if len(steps) != len(want) {
		t.Fatalf("%d steps, golden has %d", len(steps), len(want))
	}
	for i, step := range steps {
		switch get(step, "op") {
		case "write_raw":
			if err := os.WriteFile(file, []byte(jsString(get(step, "content"))), 0o600); err != nil {
				t.Fatal(err)
			}
			continue
		case "mutate_hash":
			var c map[string]any
			data, err := os.ReadFile(file) // #nosec G304 -- file is under t.TempDir()
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &c); err != nil {
				t.Fatal(err)
			}
			c["account_hash"] = "wrong"
			data, _ = json.Marshal(c)
			if err := os.WriteFile(file, data, 0o600); err != nil {
				t.Fatal(err)
			}
			continue
		}
		now, err := time.Parse(time.RFC3339Nano, jsString(get(step, "now")))
		if err != nil {
			t.Fatal(err)
		}
		src.raw = rawOf(t, get(step, "response"))
		before := len(src.stops)
		h := HistoryCache{Dir: dir, Now: func() time.Time { return now }}
		refresh := ""
		if r := get(step, "refresh"); r != nil {
			refresh = jsString(r)
		}
		res, err := h.CachedOrderCards(context.Background(), src, &customer, CacheOptions{Refresh: refresh})
		var result any
		if err != nil {
			result = map[string]any{"error": err.Error()}
		} else {
			result = plain(t, res)
		}
		var onDisk any = "unparseable"
		if data, rerr := os.ReadFile(file); rerr == nil { // #nosec G304 -- file is under t.TempDir()
			if json.Unmarshal(data, &onDisk) != nil {
				onDisk = "unparseable"
			}
		}
		fi, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		di, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		w := want[i].(map[string]any)
		if e, ok := w["result"].(map[string]any)["error"].(string); ok {
			w["result"] = map[string]any{"error": jsError(e)}
		}
		got := map[string]any{
			"result": result, "stop_order_ids": plain(t, src.stops[before:]), "file": onDisk,
			"file_mode": fmt.Sprintf("%o", fi.Mode().Perm()), "dir_mode": fmt.Sprintf("%o", di.Mode().Perm()),
		}
		assertEqual(t, fmt.Sprintf("step %d", i), got, w)
	}
}

func TestGoldenGoogle(t *testing.T) {
	f := fixture(t, "google.json")
	want := golden(t, "google.json")
	loc := Location{Latitude: get(f, "location", "latitude"), Longitude: get(f, "location", "longitude")}

	var matches []any
	for _, c := range f["match_cases"].([]any) {
		places, _ := get(c, "places").([]any)
		m, err := MatchGooglePlace(get(c, "venue"), loc, places)
		if err != nil {
			t.Fatal(err)
		}
		matches = append(matches, rounded(plain(t, m), 6))
	}
	assertEqual(t, "match", matches, want["match"])

	for _, c := range f["provider_cases"].([]any) {
		name := jsString(get(c, "name"))
		var requests []any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body any
			if data, _ := io.ReadAll(r.Body); len(data) > 0 {
				if err := json.Unmarshal(data, &body); err != nil {
					t.Errorf("request body: %v", err)
				}
			}
			var contentType any
			if ct := r.Header.Get("Content-Type"); ct != "" {
				contentType = ct
			}
			requests = append(requests, map[string]any{
				"path": r.RequestURI, "method": r.Method, "api_key": r.Header.Get("X-Goog-Api-Key"),
				"field_mask": r.Header.Get("X-Goog-FieldMask"), "content_type": contentType, "body": body,
			})
			reply := get(c, "details")
			if strings.Contains(r.URL.Path, "searchText") {
				reply = get(c, "search")
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(int(toNumber(get(reply, "status"))))
			_, _ = w.Write(rawOf(t, get(reply, "body")))
		}))
		provider := NewGooglePlacesProvider("synthetic-key", srv.Client(), srv.URL)
		out, err := provider(context.Background(), GoogleRequest{
			VenueName: get(c, "venue"), Location: loc, CityCode: jsString(get(c, "city")),
			IncludeReviews: get(c, "include_reviews") == true, ReviewLimit: int(toNumber(get(c, "review_limit"))),
		})
		srv.Close()
		var output any
		if err != nil {
			output = map[string]any{"error": err.Error()}
		} else {
			output = plain(t, out)
		}
		w := get(want, "providers", name).(map[string]any)
		if e, ok := w["output"].(map[string]any)["error"].(string); ok {
			w["output"] = map[string]any{"error": jsError(e)}
		}
		assertEqual(t, name, map[string]any{"output": output, "requests": plain(t, requests)}, w)
	}

	if (NewGooglePlacesProvider("", nil, "") == nil) != want["no_key_is_null"].(bool) {
		t.Error("an empty key must yield no provider")
	}
}
