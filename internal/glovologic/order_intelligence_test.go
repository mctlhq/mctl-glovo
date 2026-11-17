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
	rateAt  string // order id whose detail answers 429
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
	if id == s.rateAt {
		return nil, fmt.Errorf("429: %w", ErrRateLimited)
	}
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
	if got.AnalyzedOrders != 50 || got.CardStatistics.Orders != 60 || got.CardStatistics.DetailedOrders != 50 || got.CardStatistics.CardOnlyOrders != 10 {
		t.Fatalf("analyzed %d, card stats %+v", got.AnalyzedOrders, got.CardStatistics)
	}
	if got.TopProducts[0].Product != "Old Favourite" || got.TopProducts[0].Orders != 47 {
		t.Fatalf("top = %+v", got.TopProducts[0])
	}
	if len(s.asked) != 50 || s.asked[0] != "1000" || s.asked[49] != "1049" {
		t.Fatalf("details must be the first 50 cards in discovery order, asked %d (%v..)", len(s.asked), s.asked[:1])
	}
	if !strings.Contains(got.Limitation, "50 most recent of 60") {
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

// ---- mctl-glovo#45 follow-up: cancelled orders, honest coverage, ties,
// uncertainty. All data below is invented.

func asked(s *synthSource) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.asked, ",")
}

func uncertaintyCount(got *HistoryAnalysis, code string) int {
	for _, u := range got.Uncertainty {
		if u.Code == code {
			return u.Count
		}
	}
	return 0
}

// A cancelled card is not an order of the dish: it is excluded from the
// counts, never costs a detail request, and is reported.
func TestOrderIntelligenceCancelledCardExcluded(t *testing.T) {
	s := build(t, 3, func(int) []any { return []any{line("Dumplings", 1, "")} })
	s.cards[0]["status"] = "CANCELLED"
	got := analyze(t, s, 10)
	if got.TopProducts[0].Orders != 2 || got.AnalyzedOrders != 2 {
		t.Fatalf("cancelled order counted: %+v / %d", got.TopProducts[0], got.AnalyzedOrders)
	}
	if asked(s) != "1001,1002" {
		t.Fatalf("cancelled card must not be fetched, asked %s", asked(s))
	}
	c := got.Coverage
	if c.DiscoveredOrders != 3 || c.EligibleOrders != 2 || c.ExcludedNonFulfilled != 1 {
		t.Fatalf("coverage = %+v", c)
	}
}

// The venue model also reads "Cancelled" in the card total.
func TestOrderIntelligenceCancelledTotalTextExcluded(t *testing.T) {
	s := build(t, 2, func(int) []any { return []any{line("Dumplings", 1, "")} })
	s.cards[1]["total"] = "Cancelled"
	got := analyze(t, s, 10)
	if got.TopProducts[0].Orders != 1 || got.Coverage.ExcludedNonFulfilled != 1 {
		t.Fatalf("got %+v / %+v", got.TopProducts[0], got.Coverage)
	}
}

// Failed and rejected orders are not fulfilled either (assumption: provider
// status values are free text; any status containing cancel, fail or reject
// is treated as not fulfilled). Delivered and unknown statuses stay in.
func TestOrderIntelligenceNonFulfilledStatuses(t *testing.T) {
	for status, excluded := range map[string]bool{
		"CANCELLED": true, "Cancelled by user": true, "FAILED": true, "payment_failed": true,
		"REJECTED": true, "DELIVERED": false, "": false, "Completed": false,
	} {
		s := build(t, 2, func(int) []any { return []any{line("Falafel", 1, "")} })
		if status != "" {
			s.cards[0]["status"] = status
		}
		got := analyze(t, s, 10)
		want := 2
		if excluded {
			want = 1
		}
		if got.TopProducts[0].Orders != want {
			t.Errorf("status %q: orders = %d, want %d", status, got.TopProducts[0].Orders, want)
		}
	}
}

// The card says nothing, the detail says cancelled (currentStatus.type, as
// the provider detail shape carries it): excluded after the read and counted.
func TestOrderIntelligenceCancelledDetailOnlyExcluded(t *testing.T) {
	s := build(t, 3, func(int) []any { return []any{line("Dumplings", 1, "")} })
	s.details["1000"].(map[string]any)["currentStatus"] = map[string]any{"type": "CANCELLED", "creationTime": 1767225600000}
	got := analyze(t, s, 10)
	if got.TopProducts[0].Orders != 2 || got.AnalyzedOrders != 2 {
		t.Fatalf("got %+v / %d", got.TopProducts[0], got.AnalyzedOrders)
	}
	c := got.Coverage
	if c.DetailedOrders != 3 || c.ExcludedNonFulfilled != 1 || c.EligibleOrders != 3 {
		t.Fatalf("coverage = %+v", c)
	}
	if got.CardStatistics.DetailedOrders != 3 {
		t.Fatalf("all three details were read: %+v", got.CardStatistics)
	}
	// Excluded by detail is not "eligible but not analysed".
	if c.EligibleNotAnalyzed != 0 || !c.AnalysisComplete {
		t.Fatalf("coverage = %+v", c)
	}
}

// The detail may carry the status as orderStatus rather than currentStatus.type.
func TestOrderIntelligenceCancelledDetailOrderStatusField(t *testing.T) {
	s := build(t, 2, func(int) []any { return []any{line("Dumplings", 1, "")} })
	d := s.details["1000"].(map[string]any)
	delete(d, "currentStatus")
	d["orderStatus"] = "CANCELLED"
	got := analyze(t, s, 10)
	if got.TopProducts[0].Orders != 1 || got.Coverage.ExcludedNonFulfilled != 1 {
		t.Fatalf("got %+v / %+v", got.TopProducts[0], got.Coverage)
	}
}

// A status on the card is enough: a detail without one is not status_unknown.
func TestOrderIntelligenceStatusKnownFromCard(t *testing.T) {
	s := build(t, 2, func(int) []any { return []any{line("Dumplings", 1, "")} })
	s.cards[0]["status"] = "DELIVERED"
	delete(s.details["1000"].(map[string]any), "currentStatus")
	delete(s.details["1001"].(map[string]any), "currentStatus")
	if n := uncertaintyCount(analyze(t, s, 10), "status_unknown"); n != 1 {
		t.Fatalf("status_unknown = %d, want 1 (only the order with no status anywhere)", n)
	}
}

// Cancelled cards do not consume the detail budget: with limit 2 the two
// newest fulfilled orders are read.
func TestOrderIntelligenceCancelledDoNotConsumeDetailBudget(t *testing.T) {
	s := build(t, 5, func(int) []any { return []any{line("Tacos", 1, "")} })
	for i := 0; i < 3; i++ {
		s.cards[i]["status"] = "CANCELLED"
	}
	got := analyze(t, s, 2)
	if asked(s) != "1003,1004" || got.AnalyzedOrders != 2 {
		t.Fatalf("asked %s, analyzed %d", asked(s), got.AnalyzedOrders)
	}
}

// Only cancelled orders: an honest empty answer, no detail request, no error.
func TestOrderIntelligenceAllCancelled(t *testing.T) {
	s := build(t, 2, func(int) []any { return []any{line("Tacos", 1, "")} })
	s.cards[0]["status"], s.cards[1]["status"] = "CANCELLED", "CANCELLED"
	got := analyze(t, s, 10)
	if got.AnalyzedOrders != 0 || len(got.TopProducts) != 0 || got.MostOrdered != nil || asked(s) != "" {
		t.Fatalf("got %+v asked %s", got, asked(s))
	}
	if got.Coverage.ExcludedNonFulfilled != 2 || got.Coverage.EligibleOrders != 0 || !got.Coverage.AnalysisComplete {
		t.Fatalf("coverage = %+v", got.Coverage)
	}
}

// The limitation text states what was analysed against what is available and
// whether the history itself was cut short.
func TestOrderIntelligenceLimitationIsDynamic(t *testing.T) {
	complete := analyze(t, build(t, 3, func(int) []any { return []any{line("Tacos", 1, "")} }), 10)
	if !complete.Coverage.HistoryComplete || !complete.Coverage.AnalysisComplete || complete.Coverage.EligibleNotAnalyzed != 0 {
		t.Fatalf("complete coverage = %+v", complete.Coverage)
	}
	if !strings.Contains(complete.Limitation, "all 3") || strings.Contains(complete.Limitation, "most recent") {
		t.Fatalf("limitation = %q", complete.Limitation)
	}

	s := build(t, 12, func(int) []any { return []any{line("Tacos", 1, "")} })
	s.stopped = "max_pages"
	cut := analyze(t, s, 10)
	c := cut.Coverage
	if c.HistoryComplete || c.AnalysisComplete || c.EligibleNotAnalyzed != 2 || c.EligibleOrders != 12 {
		t.Fatalf("cut coverage = %+v", c)
	}
	for _, want := range []string{"10 most recent of 12", "may be incomplete"} {
		if !strings.Contains(cut.Limitation, want) {
			t.Errorf("limitation lacks %q: %q", want, cut.Limitation)
		}
	}
	if uncertaintyCount(cut, "history_truncated") != 1 || uncertaintyCount(cut, "details_not_attempted") != 2 {
		t.Fatalf("uncertainty = %+v", cut.Uncertainty)
	}

	// A history that ended by the provider's own end-of-list signals is complete.
	for _, reason := range []string{"no_next_cursor", "empty_page"} {
		s := build(t, 2, func(int) []any { return []any{line("Tacos", 1, "")} })
		s.stopped = reason
		if !analyze(t, s, 10).Coverage.HistoryComplete {
			t.Errorf("%s must count as a complete history", reason)
		}
	}
	// Every other stop reason (loop guards, page cap, unknown) is not.
	for _, reason := range []string{"max_pages", "repeated_cursor", "repeated_next_cursor", "unknown", "known_order", "something_new"} {
		s := build(t, 2, func(int) []any { return []any{line("Tacos", 1, "")} })
		s.stopped = reason
		if analyze(t, s, 10).Coverage.HistoryComplete {
			t.Errorf("%s must not count as a complete history", reason)
		}
	}
}

// card_statistics.detailed_orders counts the orders whose detail was read.
func TestOrderIntelligenceCardStatisticsDetailedOrders(t *testing.T) {
	s := build(t, 4, func(int) []any { return []any{line("Tacos", 1, "")} })
	got := analyze(t, s, 3)
	cs := got.CardStatistics
	if cs.Orders != 4 || cs.DetailedOrders != 3 || cs.CardOnlyOrders != 1 {
		t.Fatalf("card statistics = %+v", cs)
	}
	// A failed detail is not "detailed".
	s2 := build(t, 3, func(int) []any { return []any{line("Tacos", 1, "")} })
	delete(s2.details, "1001")
	if cs := analyze(t, s2, 10).CardStatistics; cs.DetailedOrders != 2 {
		t.Fatalf("card statistics = %+v", cs)
	}
}

// Ties: equal order counts are reported; the ranking among them is
// deterministic (quantity, then name) and an unresolved tie is flagged.
func TestOrderIntelligenceTies(t *testing.T) {
	// Equal orders, equal quantity: resolved by name only, flagged.
	s := build(t, 2, func(i int) []any {
		if i == 0 {
			return []any{line("Zucchini Fritters", 1, ""), line("Apple Pie", 1, "")}
		}
		return []any{line("Apple Pie", 1, ""), line("Zucchini Fritters", 1, "")}
	})
	got := analyze(t, s, 10)
	m := got.MostOrdered
	if m == nil || m.Product != "Apple Pie" || m.Orders != 2 || !m.UnresolvedTie || m.TieBreak != "product_name" {
		t.Fatalf("most_ordered = %+v", m)
	}
	if len(m.TiedOnOrders) != 2 || m.TiedOnOrders[0] != "Apple Pie" || m.TiedOnOrders[1] != "Zucchini Fritters" {
		t.Fatalf("tied = %v", m.TiedOnOrders)
	}
	if got.TopProducts[0].Product != "Apple Pie" || got.TopProducts[1].Product != "Zucchini Fritters" {
		t.Fatalf("name tie-break order: %+v", got.TopProducts)
	}

	// Equal orders, different quantity: quantity wins, the tie on orders is
	// still shown but it is not an unresolved tie.
	s = build(t, 2, func(i int) []any {
		if i == 0 {
			return []any{line("Apple Pie", 1, ""), line("Zucchini Fritters", 3, "")}
		}
		return []any{line("Apple Pie", 1, ""), line("Zucchini Fritters", 1, "")}
	})
	m = analyze(t, s, 10).MostOrdered
	if m.Product != "Zucchini Fritters" || m.Quantity != 4 || m.UnresolvedTie || len(m.TiedOnOrders) != 2 {
		t.Fatalf("quantity tie-break: %+v", m)
	}

	// A clear winner has no tie fields.
	s = build(t, 2, func(i int) []any {
		if i == 0 {
			return []any{line("Apple Pie", 1, ""), line("Zucchini Fritters", 1, "")}
		}
		return []any{line("Apple Pie", 1, "")}
	})
	m = analyze(t, s, 10).MostOrdered
	if m.Product != "Apple Pie" || m.UnresolvedTie || len(m.TiedOnOrders) != 0 {
		t.Fatalf("clear winner: %+v", m)
	}
}

// The first-listed name wins for the display name; the order of equal
// products in the output does not depend on discovery order.
func TestOrderIntelligenceTieBreakIgnoresDiscoveryOrder(t *testing.T) {
	mk := func(first, second string) *HistoryAnalysis {
		s := build(t, 1, func(int) []any { return []any{line(first, 1, ""), line(second, 1, "")} })
		return analyze(t, s, 10)
	}
	a, b := mk("Beta Bowl", "Alpha Bowl"), mk("Alpha Bowl", "Beta Bowl")
	if a.TopProducts[0].Product != "Alpha Bowl" || b.TopProducts[0].Product != "Alpha Bowl" {
		t.Fatalf("a=%v b=%v", a.TopProducts[0].Product, b.TopProducts[0].Product)
	}
}

// A rate limit stops enrichment, is reported, and later orders are not
// attempted; what was read before it is still analysed.
func TestOrderIntelligenceRateLimitedDetail(t *testing.T) {
	s := build(t, 5, func(int) []any { return []any{line("Tacos", 1, "")} })
	s.rateAt = "1002"
	got := analyze(t, s, 10)
	c := got.Coverage
	if !c.DetailRateLimited || c.DetailAttempts != 3 || c.DetailedOrders != 2 || c.DetailErrors != 0 || got.AnalyzedOrders != 2 {
		t.Fatalf("coverage = %+v analyzed %d", c, got.AnalyzedOrders)
	}
	if asked(s) != "1000,1001,1002" {
		t.Fatalf("asked %s", asked(s))
	}
	if uncertaintyCount(got, "detail_rate_limited") != 1 || uncertaintyCount(got, "details_not_attempted") != 2 || c.AnalysisComplete {
		t.Fatalf("uncertainty = %+v complete=%t", got.Uncertainty, c.AnalysisComplete)
	}
	if !strings.Contains(got.Limitation, "rate-limited") {
		t.Fatalf("limitation = %q", got.Limitation)
	}
	// Nothing readable at all: an outage, not an empty history.
	s = build(t, 3, func(int) []any { return []any{line("Tacos", 1, "")} })
	s.rateAt = "1000"
	if _, err := AnalyzeOrderHistory(context.Background(), s, AnalyzeOptions{DetailLimit: 10, Sleep: noSleep}); err == nil {
		t.Fatal("a rate limit on the first detail must fail the call")
	}
}

// top_products is capped at 50; the distinct count and an uncertainty entry
// say how many were cut.
func TestOrderIntelligenceTopProductsCap(t *testing.T) {
	s := build(t, 60, func(i int) []any { return []any{line(fmt.Sprintf("Dish %02d", i), 1, "")} })
	got := analyze(t, s, 50)
	if len(got.TopProducts) != 50 || got.DistinctProducts != 50 {
		t.Fatalf("50 orders, 50 dishes: %d / %d", len(got.TopProducts), got.DistinctProducts)
	}
	// 50 detail orders with two dishes each: 100 distinct dishes, 50 shown.
	s = build(t, 50, func(i int) []any {
		return []any{line(fmt.Sprintf("Dish A%02d", i), 1, ""), line(fmt.Sprintf("Dish B%02d", i), 1, "")}
	})
	got = analyze(t, s, 50)
	if len(got.TopProducts) != 50 || got.DistinctProducts != 100 {
		t.Fatalf("cap: %d / %d", len(got.TopProducts), got.DistinctProducts)
	}
	if uncertaintyCount(got, "products_beyond_top_cap") != 50 {
		t.Fatalf("uncertainty = %+v", got.Uncertainty)
	}
}

// The same dish name at two stores is one product (name-only key). That is
// reported, because the counts may mix two different kitchens.
func TestOrderIntelligenceCrossStoreMerge(t *testing.T) {
	s := build(t, 3, func(int) []any { return []any{line("Margherita Pizza", 1, "")} })
	s.details["1000"].(map[string]any)["storeName"] = "Synthetic Kitchen"
	s.details["1001"].(map[string]any)["storeName"] = "Other Kitchen"
	s.details["1002"].(map[string]any)["storeName"] = "Other Kitchen"
	got := analyze(t, s, 10)
	if len(got.TopProducts) != 1 || got.TopProducts[0].Orders != 3 {
		t.Fatalf("name-only key: %+v", got.TopProducts)
	}
	if uncertaintyCount(got, "same_name_across_stores") != 1 {
		t.Fatalf("uncertainty = %+v", got.Uncertainty)
	}
	// One store only: nothing to flag.
	s = build(t, 2, func(int) []any { return []any{line("Margherita Pizza", 1, "")} })
	for _, id := range []string{"1000", "1001"} {
		s.details[id].(map[string]any)["storeName"] = "Synthetic Kitchen"
	}
	if n := uncertaintyCount(analyze(t, s, 10), "same_name_across_stores"); n != 0 {
		t.Fatalf("single store flagged %d", n)
	}
}

// Renamed or respelled items are merged by name normalisation; the merge is
// reported so the reader knows the count rests on that heuristic.
func TestOrderIntelligenceNameVariantsReported(t *testing.T) {
	s := build(t, 3, func(i int) []any {
		return []any{line([]string{"Margherita Pizza", "MARGHERITA  pizza", "Marghérita Pizza!"}[i], 1, "")}
	})
	got := analyze(t, s, 10)
	if got.TopProducts[0].Orders != 3 || uncertaintyCount(got, "product_name_variants_merged") != 1 {
		t.Fatalf("got %+v / %+v", got.TopProducts[0], got.Uncertainty)
	}
	s = build(t, 2, func(int) []any { return []any{line("Margherita Pizza", 1, "")} })
	if n := uncertaintyCount(analyze(t, s, 10), "product_name_variants_merged"); n != 0 {
		t.Fatalf("identical names flagged %d", n)
	}
}

// Missing ids, duplicated detail ids, nameless lines and unknown status are
// all reported rather than silently absorbed.
func TestOrderIntelligenceDataQualityUncertainty(t *testing.T) {
	s := build(t, 4, func(int) []any { return []any{line("Tacos", 1, ""), line("", 1, "")} })
	delete(s.details["1000"].(map[string]any), "id")
	s.details["1002"].(map[string]any)["id"] = "1001"
	delete(s.details["1003"].(map[string]any), "currentStatus")
	got := analyze(t, s, 10)
	for code, want := range map[string]int{
		"details_without_id": 1, "duplicate_detail_ids": 1, "lines_without_name": 4, "status_unknown": 1,
	} {
		if n := uncertaintyCount(got, code); n != want {
			t.Errorf("%s = %d, want %d (%+v)", code, n, want, got.Uncertainty)
		}
	}
	// A clean history reports none of them.
	clean := analyze(t, build(t, 3, func(int) []any { return []any{line("Tacos", 1, "")} }), 10)
	if len(clean.Uncertainty) != 0 {
		t.Fatalf("clean history flagged: %+v", clean.Uncertainty)
	}
	b, _ := json.Marshal(clean)
	if !strings.Contains(string(b), `"uncertainty":[]`) {
		t.Fatalf("uncertainty must be an empty array: %s", b)
	}
}

// Partial detail failures are an uncertainty too, with their count.
func TestOrderIntelligenceDetailErrorsAreUncertainty(t *testing.T) {
	s := build(t, 4, func(int) []any { return []any{line("Gyros", 1, "")} })
	delete(s.details, "1001")
	got := analyze(t, s, 10)
	if uncertaintyCount(got, "detail_errors") != 1 || got.Coverage.AnalysisComplete {
		t.Fatalf("uncertainty = %+v coverage = %+v", got.Uncertainty, got.Coverage)
	}
}

// Additive: every key the previous output had is still present.
func TestOrderIntelligenceOutputKeepsExistingKeys(t *testing.T) {
	got := analyze(t, build(t, 2, func(int) []any { return []any{line("Tacos", 1, "")} }), 10)
	b, _ := json.Marshal(got)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"analyzed_orders", "distinct_products", "top_products", "limitation", "card_statistics", "coverage"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing top-level key %s", k)
		}
	}
	cov := m["coverage"].(map[string]any)
	for _, k := range []string{"discovered_orders", "cursor_pages", "detail_limit", "detail_attempts", "detailed_orders", "detail_errors", "detail_rate_limited", "stopped_reason", "strategy"} {
		if _, ok := cov[k]; !ok {
			t.Errorf("missing coverage key %s", k)
		}
	}
}
