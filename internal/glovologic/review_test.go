package glovologic

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for the PR #10 review findings.

var errOutage = errors.New("glovo: 503 service unavailable")

// flakySource is the golden suggestion source with selected reads failing.
type flakySource struct {
	*suggestionSource
	failViews   bool
	failSearch  bool
	failContent bool
	viewCalls   atomic.Int32
}

func (s *flakySource) ProductView(ctx context.Context, r ProductRequest) (json.RawMessage, error) {
	s.viewCalls.Add(1)
	if s.failViews {
		return nil, errOutage
	}
	return s.suggestionSource.ProductView(ctx, r)
}

func (s *flakySource) SearchStoreItems(ctx context.Context, storeID, addressID, query string) (json.RawMessage, error) {
	if s.failSearch {
		return nil, errOutage
	}
	return s.suggestionSource.SearchStoreItems(ctx, storeID, addressID, query)
}

func (s *flakySource) StoreContent(ctx context.Context, storeID, addressID string) (ContentProducts, error) {
	if s.failContent {
		return nil, errOutage
	}
	return s.suggestionSource.StoreContent(ctx, storeID, addressID)
}

func suggestionFixture(t *testing.T) (*suggestionSource, SessionState) {
	t.Helper()
	f := fixture(t, "suggestions.json")
	inputs := golden(t, "suggestions.json")["inputs"].(map[string]any)
	loc := f["location"]
	return &suggestionSource{t: t, f: f, inputs: inputs}, SessionState{SignedIn: true, HasLocation: true, Location: Location{
		Latitude: get(loc, "latitude"), Longitude: get(loc, "longitude"), CityCode: jsString(get(loc, "cityCode")),
	}}
}

func TestSuggestionsFailWhenEveryProductViewFails(t *testing.T) {
	base, sess := suggestionFixture(t)
	src := &flakySource{suggestionSource: base, failViews: true}
	max := 3
	got, err := GetSuggestions(context.Background(), src, sess, SuggestionInput{Mode: "explore", Query: "pizza", MaxChoices: &max}, SuggestOptions{Sleep: noSleep})
	if err == nil {
		t.Fatalf("an outage must be an error, got %d choices", len(got.Choices))
	}
	if !strings.Contains(err.Error(), "no live product could be resolved") {
		t.Fatalf("unexpected error %v", err)
	}
	if src.viewCalls.Load() == 0 {
		t.Fatal("the test must exercise product views")
	}
}

func TestSuggestionsFailWhenEveryStoreSearchFails(t *testing.T) {
	base, sess := suggestionFixture(t)
	src := &flakySource{suggestionSource: base, failSearch: true}
	max := 3
	_, err := GetSuggestions(context.Background(), src, sess, SuggestionInput{Mode: "explore", Query: "funghi", MaxChoices: &max}, SuggestOptions{Sleep: noSleep})
	if err == nil || !strings.Contains(err.Error(), "no live product could be resolved") {
		t.Fatalf("failed store searches with no choices must be an error, got %v", err)
	}
}

func TestSuggestionsCountOptionalContentFailures(t *testing.T) {
	base, sess := suggestionFixture(t)
	src := &flakySource{suggestionSource: base, failContent: true}
	max := 3
	got, err := GetSuggestions(context.Background(), src, sess, SuggestionInput{Mode: "repeat", Query: "pizza", ItemMode: "repeat", MaxChoices: &max}, SuggestOptions{Sleep: noSleep})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Choices) == 0 {
		t.Fatal("carousels are optional; the public catalog must still give choices")
	}
	if got.Coverage.StoreContentFailures == 0 {
		t.Fatal("failed carousel reads must be counted")
	}
}

// brokenCarousel serves store content whose compaction fails.
type brokenCarousel struct{ *suggestionSource }

func (s *brokenCarousel) StoreContent(context.Context, string, string) (ContentProducts, error) {
	return func(string, int) (json.RawMessage, error) { return nil, errOutage }, nil
}

func TestSuggestionsCountFailedCarouselReads(t *testing.T) {
	base, sess := suggestionFixture(t)
	max := 3
	got, err := GetSuggestions(context.Background(), &brokenCarousel{base}, sess, SuggestionInput{Mode: "repeat", Query: "pizza", ItemMode: "repeat", MaxChoices: &max}, SuggestOptions{Sleep: noSleep})
	if err != nil {
		t.Fatal(err)
	}
	if got.Coverage.StoreContentFailures == 0 {
		t.Fatal("a failed carousel read must be counted")
	}
}

func TestSuggestionsNoMatchIsNotAnError(t *testing.T) {
	base, sess := suggestionFixture(t)
	got, err := GetSuggestions(context.Background(), base, sess, SuggestionInput{Query: "pizza", VenueQuery: "Nowhere"}, SuggestOptions{Sleep: noSleep})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Choices) != 0 || got.Coverage.ProductLookupFailures != 0 {
		t.Fatalf("no candidate store is an honest empty result: %+v", got.Coverage)
	}
}

// blockingSearch fails the history walk at once and blocks SearchStores
// until its context is cancelled.
type blockingSearch struct {
	*suggestionSource
	cancelled chan struct{}
}

func (s *blockingSearch) OrderCards(context.Context, CardOptions) (json.RawMessage, error) {
	return nil, errOutage
}

func (s *blockingSearch) SearchStores(ctx context.Context, _ string) (json.RawMessage, error) {
	select {
	case <-ctx.Done():
		close(s.cancelled)
		return nil, ctx.Err()
	case <-time.After(10 * time.Second):
		return nil, errors.New("search was not cancelled")
	}
}

func TestSuggestionsCancelSearchWhenHistoryFails(t *testing.T) {
	base, sess := suggestionFixture(t)
	src := &blockingSearch{suggestionSource: base, cancelled: make(chan struct{})}
	start := time.Now()
	_, err := GetSuggestions(context.Background(), src, sess, SuggestionInput{Query: "pizza"}, SuggestOptions{Sleep: noSleep})
	if !errors.Is(err, errOutage) {
		t.Fatalf("want the history error, got %v", err)
	}
	select {
	case <-src.cancelled:
	default:
		t.Fatal("the search context was not cancelled")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("GetSuggestions waited for the search instead of cancelling it")
	}
}

func TestWithRetryReportsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sleep := func(context.Context, time.Duration) error {
		cancel()
		return ctx.Err()
	}
	_, err := withRetry(ctx, sleep, 3, func() (int, error) { return 0, errOutage })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if !errors.Is(err, errOutage) {
		t.Fatalf("the original failure must be kept, got %v", err)
	}
}

type failingDetails struct{ historySource }

func (s *failingDetails) OrderDetail(context.Context, string) (json.RawMessage, error) {
	return nil, errOutage
}

func TestAnalyzeOrderHistoryFailsWhenNoDetailIsRead(t *testing.T) {
	src := &failingDetails{historySource{t: t, f: fixture(t, "history.json")}}
	_, err := AnalyzeOrderHistory(context.Background(), src, AnalyzeOptions{DetailLimit: 5, Sleep: noSleep})
	if err == nil || !strings.Contains(err.Error(), "no order detail could be read") {
		t.Fatalf("want an error when every detail fails, got %v", err)
	}
}

func TestAnalyzeOrderHistoryZeroLimitIsNotAnError(t *testing.T) {
	src := &failingDetails{historySource{t: t, f: fixture(t, "history.json")}}
	got, err := AnalyzeOrderHistory(context.Background(), src, AnalyzeOptions{DetailLimit: 0, Sleep: noSleep})
	if err != nil {
		t.Fatal(err)
	}
	if got.Coverage.DetailAttempts != 0 {
		t.Fatalf("detail_limit 0 must not read details: %+v", got.Coverage)
	}
}

func TestReorderPlanReportsTheSearchedStore(t *testing.T) {
	order := decodeOrFail(t, `{"id":9,"store":{"storeId":"77","storeAddressId":"88","name":"Shop"},"boughtProducts":[{"name":"Milk"}]}`)
	src := &reorderSource{t: t, f: Object{"orders": Object{"9": order}}, inputs: map[string]any{"easy_reorder": map[string]any{}, "search": map[string]any{}}}
	plan, err := PlanReorder(context.Background(), src, "9", DefaultPlanOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(src.calls) == 0 || src.calls[0].([]any)[1] != "77" || src.calls[0].([]any)[2] != "88" {
		t.Fatalf("store 77/88 must be read, calls %v", src.calls)
	}
	if plan.StoreID != "77" || plan.StoreAddressID != "88" {
		t.Fatalf("plan reports store %v/%v, want the searched 77/88", plan.StoreID, plan.StoreAddressID)
	}
}
