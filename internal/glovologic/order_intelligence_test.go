package glovologic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// Synthetic fixtures for "Which dish did I order most often?" (mctl-glovo#45).
// Every name, id and amount below is invented. The tests pin how the existing
// glovo_analyze_order_history logic answers that question; they do not add a
// tool.

// synthSource serves a fixed set of order cards and details for one principal.
type synthSource struct {
	t       *testing.T
	cards   []map[string]any
	details map[string]any
	stopped string
	pages   int
	mu      sync.Mutex
	asked   []string
}

func (s *synthSource) OrderCards(context.Context, CardOptions) (json.RawMessage, error) {
	pages := s.pages
	if pages == 0 {
		pages = 1
	}
	stopped := s.stopped
	if stopped == "" {
		stopped = "no_next_cursor"
	}
	b, err := json.Marshal(map[string]any{
		"orders": s.cards, "pages": make([]any, pages), "count": len(s.cards),
		"stopped_reason": stopped, "strategy": "order-id-cursor",
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return b, nil
}

func (s *synthSource) OrderDetail(_ context.Context, id string) (json.RawMessage, error) {
	s.mu.Lock()
	s.asked = append(s.asked, id)
	s.mu.Unlock()
	d, ok := s.details[id]
	if !ok {
		return nil, fmt.Errorf("synthetic: no detail for %s", id)
	}
	b, err := json.Marshal(d)
	if err != nil {
		s.t.Fatal(err)
	}
	return b, nil
}

func line(name string, qty int, custom string) map[string]any {
	l := map[string]any{"name": name, "quantity": fmt.Sprint(qty), "price": "5,00 EUR"}
	if custom != "" {
		l["customizationsDescription"] = custom
	}
	return l
}

// build creates n orders (newest first), each with the lines given by lines(i).
func build(t *testing.T, n int, lines func(i int) []any) *synthSource {
	t.Helper()
	s := &synthSource{t: t, details: map[string]any{}}
	for i := 0; i < n; i++ {
		id := fmt.Sprint(1000 + i)
		s.cards = append(s.cards, map[string]any{"order_id": id, "store": "Synthetic Kitchen", "total": "10,00 EUR"})
		s.details[id] = map[string]any{
			"id": id, "currentStatus": map[string]any{"type": "DELIVERED", "creationTime": 1767225600000 - int64(i)*86_400_000},
			"boughtProducts": lines(i),
		}
	}
	return s
}

func analyze(t *testing.T, s *synthSource, limit int) *HistoryAnalysis {
	t.Helper()
	got, err := AnalyzeOrderHistory(context.Background(), s, AnalyzeOptions{DetailLimit: limit, Sleep: noSleep})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// Most ordered dish across repeated orders, with spelling, case, accent and
// punctuation variants of the same dish merged by name normalisation.
func TestOrderIntelligenceMostOrderedDish(t *testing.T) {
	s := build(t, 6, func(i int) []any {
		switch i {
		case 0:
			return []any{line("Margherita Pizza", 2, ""), line("Pad Thai", 1, "")}
		case 1:
			return []any{line("MARGHERITA  pizza", 1, ""), line("Pad Thai", 1, "")}
		case 2:
			return []any{line("Marghérita Pizza!", 1, ""), line("Green Curry", 1, "")}
		case 3:
			return []any{line("Pad Thai", 1, "")}
		default:
			return []any{line("Miso Soup", 1, "")}
		}
	})
	got := analyze(t, s, 10)
	top := got.TopProducts
	if len(top) != 4 {
		t.Fatalf("want 4 distinct dishes, got %d: %+v", len(top), top)
	}
	// Pad Thai and Margherita both appear in 3 orders; Margherita has quantity 4.
	if top[0].Product != "Margherita Pizza" || top[0].Orders != 3 || top[0].Quantity != 4 {
		t.Fatalf("top dish = %+v", top[0])
	}
	if top[1].Product != "Pad Thai" || top[1].Orders != 3 || top[1].Quantity != 3 {
		t.Fatalf("second dish = %+v", top[1])
	}
	if got.DistinctProducts != 4 || got.AnalyzedOrders != 6 {
		t.Fatalf("analysis = %+v", got.OrderAnalysis)
	}
}

// Option variants of one dish are one product; the variants surface in
// top_customizations, ranked by how many lines used them.
func TestOrderIntelligenceOptionVariants(t *testing.T) {
	s := build(t, 4, func(i int) []any {
		switch i {
		case 0, 1:
			return []any{line("Burger", 1, "No onion")}
		case 2:
			return []any{line("Burger", 1, "Extra cheese")}
		default:
			return []any{line("Burger", 1, "")}
		}
	})
	got := analyze(t, s, 10)
	if len(got.TopProducts) != 1 || got.TopProducts[0].Orders != 4 {
		t.Fatalf("variants must merge into one dish: %+v", got.TopProducts)
	}
	c := got.TopProducts[0].TopCustomizations
	if len(c) != 2 || c[0] != (CustomizationCount{Name: "No onion", Orders: 2}) || c[1] != (CustomizationCount{Name: "Extra cheese", Orders: 1}) {
		t.Fatalf("customizations = %+v", c)
	}
}

// The same dish twice in one order is one order with summed quantity.
func TestOrderIntelligenceRepeatedLineInOneOrder(t *testing.T) {
	s := build(t, 1, func(int) []any { return []any{line("Ramen", 1, "Mild"), line("Ramen", 2, "Spicy")} })
	got := analyze(t, s, 10)
	p := got.TopProducts[0]
	if p.Orders != 1 || p.Quantity != 3 {
		t.Fatalf("want 1 order, quantity 3: %+v", p)
	}
}

// Details without any id still count as separate orders (index fallback);
// a detail that repeats an id counts once as an order, although its quantity
// is added again (OrderAnalysisFromDetails has no detail-level dedup).
func TestOrderIntelligenceMissingAndDuplicateIDs(t *testing.T) {
	noID := func(name string) any {
		return map[string]any{"boughtProducts": []any{line(name, 1, "")}}
	}
	got, err := OrderAnalysisFromDetails([]any{noID("Falafel"), noID("Falafel"), noID("Falafel")})
	if err != nil {
		t.Fatal(err)
	}
	if got.TopProducts[0].Orders != 3 {
		t.Fatalf("id-less details must stay separate orders: %+v", got.TopProducts[0])
	}
	dup := map[string]any{"id": "7", "boughtProducts": []any{line("Falafel", 1, "")}}
	got, err = OrderAnalysisFromDetails([]any{dup, dup})
	if err != nil {
		t.Fatal(err)
	}
	if p := got.TopProducts[0]; p.Orders != 1 || p.Quantity != 2 {
		t.Fatalf("duplicate detail: want orders 1 (deduped) and quantity 2 (not deduped), got %+v", p)
	}
}

// A card without an order id cannot be enriched: the call fails closed.
func TestOrderIntelligenceCardWithoutIDFailsClosed(t *testing.T) {
	s := build(t, 2, func(int) []any { return []any{line("Soup", 1, "")} })
	delete(s.cards[0], "order_id")
	_, err := AnalyzeOrderHistory(context.Background(), s, AnalyzeOptions{DetailLimit: 5, Sleep: noSleep})
	if err == nil {
		t.Fatal("a card without order_id must not be silently skipped")
	}
}

// Cancelled orders. KNOWN GAP (mctl-glovo#45): unlike the venue model
// (venue.go isCancelled), glovo_analyze_order_history and glovo_get_order_stats
// apply no cancelled filter, so a cancelled order's lines are counted as
// orders of the dish. This test pins that behaviour; when the filter is added
// it must be inverted deliberately.
func TestOrderIntelligenceCancelledOrdersAreCountedKnownGap(t *testing.T) {
	s := build(t, 3, func(int) []any { return []any{line("Dumplings", 1, "")} })
	s.cards[0]["status"] = "CANCELLED"
	s.cards[0]["total"] = "Cancelled"
	s.details["1000"].(map[string]any)["currentStatus"] = map[string]any{"type": "CANCELLED", "creationTime": 1767225600000}
	got := analyze(t, s, 10)
	if got.TopProducts[0].Orders != 3 {
		t.Fatalf("current behaviour counts the cancelled order, got %+v", got.TopProducts[0])
	}
	if !isCancelled(s.cards[0]) {
		t.Fatal("fixture card must be recognisable as cancelled by the venue-model predicate")
	}
}

// Partial coverage: 60 orders discovered, the dish analysis reads only the
// newest detail_limit (<=50) of them. The answer must say so in coverage
// fields; the card statistics still cover the whole discovered history.
func TestOrderIntelligencePartialCoverage(t *testing.T) {
	s := build(t, 60, func(i int) []any {
		if i < 3 {
			return []any{line("Newest Dish", 1, "")}
		}
		return []any{line("Old Favourite", 1, "")}
	})
	s.stopped, s.pages = "max_pages", 4
	got := analyze(t, s, 50)
	c := got.Coverage
	if c.DiscoveredOrders != 60 || c.DetailedOrders != 50 || c.DetailAttempts != 50 || c.DetailLimit != 50 || c.CursorPages != 4 || c.StoppedReason != "max_pages" {
		t.Fatalf("coverage = %+v", c)
	}
	if got.AnalyzedOrders != 50 || got.CardStatistics.Orders != 60 || got.CardStatistics.CardOnlyOrders != 60 {
		// card_statistics is computed from the discovery cards only, so its
		// detailed_orders stays 0 even where 50 details were read (quirk).
		t.Fatalf("analyzed %d, card stats %+v", got.AnalyzedOrders, got.CardStatistics)
	}
	if got.TopProducts[0].Product != "Old Favourite" || got.TopProducts[0].Orders != 47 {
		t.Fatalf("top = %+v", got.TopProducts[0])
	}
	if len(s.asked) != 50 || s.asked[0] != "1000" || s.asked[49] != "1049" {
		t.Fatalf("details must be the first 50 cards in discovery order, asked %d (%v..)", len(s.asked), s.asked[:1])
	}
	// The limitation text is static and says card-only orders are excluded.
	if !strings.Contains(got.Limitation, "card-only orders are excluded") {
		t.Fatalf("limitation = %q", got.Limitation)
	}
	// The JSON the tool returns carries the coverage block and limitation.
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"coverage"`, `"discovered_orders":60`, `"detailed_orders":50`, `"detail_limit":50`, `"stopped_reason":"max_pages"`, `"limitation"`, `"card_statistics"`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("tool output lacks %s", k)
		}
	}
}

// The default detail limit is 10: over a long history the "most often" answer
// is built from the newest 10 orders only, and coverage shows it.
func TestOrderIntelligenceDefaultLimitIsTen(t *testing.T) {
	if d := DefaultAnalyzeOptions().DetailLimit; d != 10 {
		t.Fatalf("default detail_limit = %d", d)
	}
	s := build(t, 30, func(int) []any { return []any{line("Tacos", 1, "")} })
	got, err := AnalyzeOrderHistory(context.Background(), s, func() AnalyzeOptions {
		o := DefaultAnalyzeOptions()
		o.Sleep, o.DetailDelay, o.PageDelay = noSleep, 0, 0
		return o
	}())
	if err != nil {
		t.Fatal(err)
	}
	if got.Coverage.DiscoveredOrders != 30 || got.Coverage.DetailedOrders != 10 || got.TopProducts[0].Orders != 10 {
		t.Fatalf("coverage %+v, top %+v", got.Coverage, got.TopProducts[0])
	}
	// More than 50 is refused rather than clamped.
	if _, err := AnalyzeOrderHistory(context.Background(), s, AnalyzeOptions{DetailLimit: 51, Sleep: noSleep}); err == nil {
		t.Fatal("detail_limit 51 must be refused")
	}
}

// No history: an empty but successful read is an honest zero, with coverage.
func TestOrderIntelligenceNoHistory(t *testing.T) {
	s := &synthSource{t: t, details: map[string]any{}, cards: []map[string]any{}}
	got := analyze(t, s, 10)
	if got.AnalyzedOrders != 0 || len(got.TopProducts) != 0 || got.Coverage.DiscoveredOrders != 0 || got.Coverage.StoppedReason != "no_next_cursor" {
		t.Fatalf("got %+v", got)
	}
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"top_products":[]`) || !strings.Contains(string(b), `"coverage"`) {
		t.Fatalf("empty history output = %s", b)
	}
}

// Detail enrichment that fails for some orders is counted, not hidden.
func TestOrderIntelligencePartialDetailFailureIsReported(t *testing.T) {
	s := build(t, 4, func(int) []any { return []any{line("Gyros", 1, "")} })
	delete(s.details, "1001")
	got := analyze(t, s, 10)
	if got.Coverage.DetailErrors != 1 || got.Coverage.DetailedOrders != 3 || got.Coverage.DetailAttempts != 4 || got.TopProducts[0].Orders != 3 {
		t.Fatalf("coverage %+v top %+v", got.Coverage, got.TopProducts[0])
	}
}

// Two principals: separate sources, interleaved and concurrent, never mix.
func TestOrderIntelligenceTwoPrincipalsDoNotLeak(t *testing.T) {
	a := build(t, 5, func(int) []any { return []any{line("Alpha Dish", 1, "")} })
	b := build(t, 3, func(int) []any { return []any{line("Beta Dish", 1, "")} })
	var wg sync.WaitGroup
	res := make([]*HistoryAnalysis, 2)
	for i, s := range []*synthSource{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := AnalyzeOrderHistory(context.Background(), s, AnalyzeOptions{DetailLimit: 10, Sleep: noSleep})
			if err != nil {
				t.Error(err)
				return
			}
			res[i] = r
		}()
	}
	wg.Wait()
	if res[0] == nil || res[1] == nil {
		t.FailNow()
	}
	ja, _ := json.Marshal(res[0])
	jb, _ := json.Marshal(res[1])
	if strings.Contains(string(ja), "Beta") || strings.Contains(string(jb), "Alpha") {
		t.Fatalf("cross-principal leak:\n%s\n%s", ja, jb)
	}
	if res[0].TopProducts[0].Orders != 5 || res[1].TopProducts[0].Orders != 3 {
		t.Fatalf("counts mixed: %d / %d", res[0].TopProducts[0].Orders, res[1].TopProducts[0].Orders)
	}
}
