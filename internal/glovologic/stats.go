package glovologic

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

var moneyPattern = regexp.MustCompile(`-?\d+(?:[.,]\d+)?`)

// ParseMoney is parseMoney (api.mjs:1471): the first number in String(v),
// with a decimal comma accepted. ok is false where upstream returns null.
func ParseMoney(v any) (amount float64, ok bool) {
	if nullish(v) {
		return 0, false
	}
	m := moneyPattern.FindString(strings.ReplaceAll(jsString(v), "\u00a0", " "))
	if m == "" {
		return 0, false
	}
	return stringToNumber(strings.Replace(m, ",", ".", 1)), true
}

// StoreStats is one top_stores entry.
type StoreStats struct {
	Store                  string  `json:"store"`
	Orders                 int     `json:"orders"`
	VisibleTotalAmount     float64 `json:"visible_total_amount"`
	ParseableVisibleTotals int     `json:"parseable_visible_totals"`
}

// OrderStats is the orderStatsFromCards result.
type OrderStats struct {
	Orders                 int          `json:"orders"`
	DetailedOrders         int          `json:"detailed_orders"`
	CardOnlyOrders         int          `json:"card_only_orders"`
	Stores                 int          `json:"stores"`
	ParseableVisibleTotals int          `json:"parseable_visible_totals"`
	VisibleTotalAmount     float64      `json:"visible_total_amount"`
	TopStores              []StoreStats `json:"top_stores"`
}

// OrderStatsFromCards is orderStatsFromCards (api.mjs:1438). Cards are
// compactOrder objects; a card of the wrong shape is an *InputError.
func OrderStatsFromCards(cards []Object) (*OrderStats, error) {
	type acc struct {
		orders, parseable int
		amount            float64
	}
	byStore := map[string]*acc{}
	var order []string
	var visible float64
	parseable, detailed := 0, 0
	for i, card := range cards {
		if err := checkOrderCard(card, idx("orders", i)); err != nil {
			return nil, err
		}
		store := "Unknown store"
		if s, ok := card["store"].(string); ok && s != "" {
			store = s
		}
		cur := byStore[store]
		if cur == nil {
			cur = &acc{}
			byStore[store] = cur
			order = append(order, store)
		}
		cur.orders++
		if amount, ok := ParseMoney(card["total"]); ok {
			cur.amount += amount
			cur.parseable++
			visible += amount
			parseable++
		}
		if card["detail_source"] == "detail" {
			detailed++
		}
	}
	coll := collator()
	sort.SliceStable(order, func(i, j int) bool {
		a, b := byStore[order[i]], byStore[order[j]]
		if a.orders != b.orders {
			return a.orders > b.orders
		}
		if a.amount != b.amount {
			return a.amount > b.amount
		}
		return coll.CompareString(order[i], order[j]) < 0
	})
	if len(order) > 20 {
		order = order[:20]
	}
	top := make([]StoreStats, 0, len(order))
	for _, s := range order {
		v := byStore[s]
		top = append(top, StoreStats{Store: s, Orders: v.orders, VisibleTotalAmount: toFixed(v.amount, 2), ParseableVisibleTotals: v.parseable})
	}
	return &OrderStats{
		Orders:                 len(cards),
		DetailedOrders:         detailed,
		CardOnlyOrders:         len(cards) - detailed,
		Stores:                 len(byStore),
		ParseableVisibleTotals: parseable,
		VisibleTotalAmount:     toFixed(visible, 2),
		TopStores:              top,
	}, nil
}

// DiscoverySummary is the discovery block of glovo_get_order_stats.
type DiscoverySummary struct {
	Pages         int    `json:"pages"`
	StoppedReason string `json:"stopped_reason"`
	Strategy      any    `json:"strategy,omitempty"`
}

// OrderStatsResult is the glovo_get_order_stats tool output.
type OrderStatsResult struct {
	OrderStats
	Discovery DiscoverySummary `json:"discovery"`
}

// StatsOptions are the glovo_get_order_stats inputs.
type StatsOptions struct {
	MaxPages  int           // zero means full discovery
	PageDelay time.Duration // tool default 750ms
}

// DefaultStatsOptions are the tool defaults (server.mjs:436).
func DefaultStatsOptions() StatsOptions { return StatsOptions{PageDelay: 750 * time.Millisecond} }

// GetOrderStats is the glovo_get_order_stats handler (server.mjs:436).
func GetOrderStats(ctx context.Context, src OrderCardSource, o StatsOptions) (*OrderStatsResult, error) {
	if o.MaxPages < 0 || o.PageDelay < 0 || o.PageDelay > 10*time.Second {
		return nil, errors.New("glovologic: max_pages must be >= 1 and page_delay_ms 0..10000")
	}
	d, err := fetchDiscovery(ctx, src, CardOptions{Limit: 15, MaxPages: o.MaxPages, PageDelay: o.PageDelay})
	if err != nil {
		return nil, err
	}
	stats, err := OrderStatsFromCards(d.Orders)
	if err != nil {
		return nil, err
	}
	return &OrderStatsResult{OrderStats: *stats, Discovery: DiscoverySummary{Pages: len(d.Pages), StoppedReason: d.StoppedReason, Strategy: d.Strategy}}, nil
}

// orderLine is one pastOrderLines entry (api.mjs:1124).
type orderLine struct {
	name           any // string or Null
	quantity       float64
	price          any
	customizations any // string or Null
}

func pastOrderLines(o any) []orderLine {
	lines, _ := orderLineContainer(o)
	out := make([]orderLine, len(lines))
	for i, line := range lines {
		q := coalesce(get(line, "quantity", "increments"), get(line, "quantity"), get(line, "amount"), 1)
		quantity := parseFloatJS(strings.Replace(jsString(q), ",", ".", 1))
		if !truthy(quantity) {
			quantity = 1
		}
		out[i] = orderLine{
			name:           coalesce(get(line, "name"), get(line, "productName"), get(line, "title"), Null),
			quantity:       quantity,
			price:          coalesce(get(line, "price", "formatted"), get(line, "price"), Null),
			customizations: coalesce(get(line, "customizationsDescription"), get(line, "description"), Null),
		}
	}
	return out
}

var digitsOnly = regexp.MustCompile(`^\d+$`)

// orderDate is orderDate (api.mjs:1137) in epoch milliseconds.
func orderDate(o any) (float64, bool) {
	var v any
	for _, c := range []any{get(o, "creationTime"), get(o, "createdAt"), get(o, "orderTime"), get(o, "deliveredAt"), get(o, "currentStatus", "creationTime")} {
		if !nullish(c) && c != "" {
			v = c
			break
		}
	}
	if v == nil {
		return 0, false
	}
	if kindOf(v) == kNumber || digitsOnly.MatchString(jsString(v)) {
		n := toNumber(v)
		if n < 10_000_000_000 {
			n *= 1000
		}
		return n, true
	}
	s, _ := v.(string)
	return parseDate(s)
}

// CustomizationCount is one top_customizations entry.
type CustomizationCount struct {
	Name   string `json:"name"`
	Orders int    `json:"orders"`
}

// ProductHistory is one top_products entry.
type ProductHistory struct {
	Product                     any                  `json:"product"`
	Orders                      int                  `json:"orders"`
	Quantity                    float64              `json:"quantity"`
	FirstOrderAt                *string              `json:"first_order_at"`
	LastOrderAt                 *string              `json:"last_order_at"`
	AverageIntervalDays         *float64             `json:"average_interval_days"`
	MedianIntervalDays          *float64             `json:"median_interval_days"`
	ParseableVisibleLineAmounts int                  `json:"parseable_visible_line_amounts"`
	VisibleLineAmount           float64              `json:"visible_line_amount"`
	TopCustomizations           []CustomizationCount `json:"top_customizations"`
}

// OrderAnalysis is the orderAnalysisFromDetails result.
type OrderAnalysis struct {
	AnalyzedOrders   int              `json:"analyzed_orders"`
	DistinctProducts int              `json:"distinct_products"`
	TopProducts      []ProductHistory `json:"top_products"`
	Limitation       string           `json:"limitation"`
}

const analysisLimitation = "Product cadence and customization statistics cover only successfully enriched order details; card-only orders are excluded from item analysis."

// OrderAnalysisFromDetails is orderAnalysisFromDetails (api.mjs:1219) over
// raw order details, with maxProducts 50.
func OrderAnalysisFromDetails(details []any) (*OrderAnalysis, error) {
	type acc struct {
		name           any
		orders         int
		quantity       float64
		dates          []float64
		amount         float64
		parseable      int
		customizations map[string]int
		customOrder    []string
	}
	products := map[string]*acc{}
	var keys []string
	seen := map[string]bool{}
	for i, o := range details {
		if err := checkOrderDetail(o, idx("details", i)); err != nil {
			return nil, err
		}
		date, hasDate := orderDate(o)
		for _, line := range pastOrderLines(o) {
			key := NormalizedProductName(line.name)
			if key == "" {
				continue
			}
			cur := products[key]
			if cur == nil {
				cur = &acc{name: line.name, customizations: map[string]int{}}
				products[key] = cur
				keys = append(keys, key)
			}
			orderKey := jsString(coalesce(get(o, "id"), get(o, "orderId"), float64(i))) + "|" + key
			if !seen[orderKey] {
				cur.orders++
			}
			seen[orderKey] = true
			cur.quantity += line.quantity
			if hasDate {
				cur.dates = append(cur.dates, date)
			}
			if amount, ok := ParseMoney(line.price); ok {
				cur.amount += amount
				cur.parseable++
			}
			if c, ok := line.customizations.(string); ok && c != "" {
				if _, had := cur.customizations[c]; !had {
					cur.customOrder = append(cur.customOrder, c)
				}
				cur.customizations[c]++
			}
		}
	}
	top := make([]ProductHistory, 0, len(keys))
	for _, key := range keys {
		p := products[key]
		dates := uniqueSorted(p.dates)
		intervals := make([]float64, 0, len(dates))
		for j := 1; j < len(dates); j++ {
			intervals = append(intervals, (dates[j]-dates[j-1])/86_400_000)
		}
		sort.Float64s(intervals)
		h := ProductHistory{
			Product:                     p.name,
			Orders:                      p.orders,
			Quantity:                    toFixed(p.quantity, 3),
			ParseableVisibleLineAmounts: p.parseable,
			VisibleLineAmount:           toFixed(p.amount, 2),
			TopCustomizations:           []CustomizationCount{},
		}
		if len(dates) > 0 {
			first, err := isoString(dates[0])
			if err != nil {
				return nil, err
			}
			last, err := isoString(dates[len(dates)-1])
			if err != nil {
				return nil, err
			}
			h.FirstOrderAt, h.LastOrderAt = &first, &last
		}
		if n := len(intervals); n > 0 {
			var sum float64
			for _, v := range intervals {
				sum += v
			}
			h.AverageIntervalDays = finiteOrNil(toFixed(sum/float64(n), 1))
			median := intervals[n/2]
			if n%2 == 0 {
				median = (intervals[n/2-1] + intervals[n/2]) / 2
			}
			h.MedianIntervalDays = finiteOrNil(toFixed(median, 1))
		}
		custom := append([]string(nil), p.customOrder...)
		sort.SliceStable(custom, func(a, b int) bool { return p.customizations[custom[a]] > p.customizations[custom[b]] })
		for j, c := range custom {
			if j == 3 {
				break
			}
			h.TopCustomizations = append(h.TopCustomizations, CustomizationCount{Name: c, Orders: p.customizations[c]})
		}
		top = append(top, h)
	}
	coll := collator()
	sort.SliceStable(top, func(i, j int) bool {
		a, b := top[i], top[j]
		if a.Orders != b.Orders {
			return a.Orders > b.Orders
		}
		if a.Quantity != b.Quantity {
			return a.Quantity > b.Quantity
		}
		return coll.CompareString(jsString(a.Product), jsString(b.Product)) < 0
	})
	if len(top) > 50 {
		top = top[:50]
	}
	return &OrderAnalysis{AnalyzedOrders: len(details), DistinctProducts: len(products), TopProducts: top, Limitation: analysisLimitation}, nil
}

func uniqueSorted(values []float64) []float64 {
	seen := map[float64]bool{}
	out := make([]float64, 0, len(values))
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Float64s(out)
	return out
}

// AnalysisCoverage is the coverage block of glovo_analyze_order_history.
type AnalysisCoverage struct {
	DiscoveredOrders  int    `json:"discovered_orders"`
	CursorPages       int    `json:"cursor_pages"`
	DetailLimit       int    `json:"detail_limit"`
	DetailAttempts    int    `json:"detail_attempts"`
	DetailedOrders    int    `json:"detailed_orders"`
	DetailErrors      int    `json:"detail_errors"`
	DetailRateLimited bool   `json:"detail_rate_limited"`
	StoppedReason     string `json:"stopped_reason"`
	Strategy          any    `json:"strategy,omitempty"`

	// Additive fields (mctl-glovo#45). EligibleOrders are the discovered
	// orders whose card is not cancelled or failed. ExcludedNonFulfilled
	// counts the cancelled or failed orders left out of the product counts,
	// by card or, after the read, by detail. EligibleNotAnalyzed are
	// fulfilled-or-unknown orders that did not make it into the product
	// counts (detail limit, read errors, rate limit).
	EligibleOrders       int  `json:"eligible_orders"`
	ExcludedNonFulfilled int  `json:"excluded_cancelled_or_failed"`
	EligibleNotAnalyzed  int  `json:"eligible_not_analyzed"`
	HistoryComplete      bool `json:"history_complete"`
	AnalysisComplete     bool `json:"analysis_complete"`
}

// MostOrdered answers "which dish did I order most often" from top_products,
// with ties made explicit. Products rank by distinct orders, then quantity,
// then product name; TiedOnOrders lists every product with the top order
// count, and UnresolvedTie is true when quantity could not separate them
// either, so the name decided.
type MostOrdered struct {
	Product       any      `json:"product"`
	Orders        int      `json:"orders"`
	Quantity      float64  `json:"quantity"`
	TiedOnOrders  []string `json:"tied_on_orders,omitempty"`
	UnresolvedTie bool     `json:"unresolved_tie"`
	TieBreak      string   `json:"tie_break"`
}

// Uncertainty is one reason the answer may be less certain than it looks.
type Uncertainty struct {
	Code  string `json:"code"`
	Count int    `json:"count"`
	Note  string `json:"note"`
}

// HistoryAnalysis is the glovo_analyze_order_history tool output.
type HistoryAnalysis struct {
	OrderAnalysis
	MostOrdered    *MostOrdered     `json:"most_ordered,omitempty"`
	CardStatistics OrderStats       `json:"card_statistics"`
	Coverage       AnalysisCoverage `json:"coverage"`
	Uncertainty    []Uncertainty    `json:"uncertainty"`
}

// nonFulfilledPattern matches order statuses that are not a fulfilled order.
// ASSUMPTION: the provider status vocabulary is not documented; real values
// seen are free text such as DELIVERED and CANCELLED (currentStatus.type of
// an order detail, status of a card). Anything containing cancel, fail or
// reject is treated as not fulfilled; any other status, and a missing one,
// is kept (and a missing one is reported as status_unknown).
var nonFulfilledPattern = regexp.MustCompile(`(?i)cancel|fail|reject`)

// statusText returns the status strings an order card or detail carries.
func statusText(o any) []string {
	var out []string
	for _, v := range []any{get(o, "status"), get(o, "orderStatus"), get(o, "currentStatus", "type")} {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func nonFulfilledStatus(o any) bool {
	for _, s := range statusText(o) {
		if nonFulfilledPattern.MatchString(s) {
			return true
		}
	}
	return false
}

// cardNonFulfilled is true for a cancelled or failed order card: the venue
// model's cancelled test (status or total text) plus failed and rejected.
func cardNonFulfilled(card Object) bool {
	return isCancelled(card) || nonFulfilledStatus(card)
}

// AnalyzeOptions are the glovo_analyze_order_history inputs.
type AnalyzeOptions struct {
	MaxPages    int // zero means full discovery
	DetailLimit int // 0..50
	PageDelay   time.Duration
	DetailDelay time.Duration
	Sleep       Sleep // nil means a real timer
}

// DefaultAnalyzeOptions are the tool defaults (server.mjs:452).
func DefaultAnalyzeOptions() AnalyzeOptions {
	return AnalyzeOptions{DetailLimit: 10, PageDelay: 750 * time.Millisecond, DetailDelay: time.Second}
}

// AnalyzeOrderHistory is GlovoClient.analyzeOrderHistory (api.mjs:584). A
// rate-limited detail stops enrichment and is reported; any other detail
// failure is counted in detail_errors. When no detail of the subset could
// be read at all, the call fails. A detail that was read but is malformed
// is an *InputError for the whole call.
func AnalyzeOrderHistory(ctx context.Context, src HistorySource, o AnalyzeOptions) (*HistoryAnalysis, error) {
	if o.MaxPages < 0 || o.DetailLimit < 0 || o.DetailLimit > 50 ||
		o.PageDelay < 0 || o.PageDelay > 10*time.Second || o.DetailDelay < 0 || o.DetailDelay > 10*time.Second {
		return nil, errors.New("glovologic: detail_limit must be 0..50 and delays 0..10000ms")
	}
	sleep := orSleep(o.Sleep)
	d, err := fetchDiscovery(ctx, src, CardOptions{Limit: 15, MaxPages: o.MaxPages, PageDelay: o.PageDelay})
	if err != nil {
		return nil, err
	}
	var details []any
	errCount, attempts, rateLimited, excludedByDetail := 0, 0, false, 0
	var eligible []Object
	excludedByCard := 0
	for _, card := range d.Orders {
		if cardNonFulfilled(card) {
			excludedByCard++
			continue
		}
		eligible = append(eligible, card)
	}
	subset := eligible
	if len(subset) > o.DetailLimit {
		subset = subset[:o.DetailLimit]
	}
	readIDs := map[string]bool{}
	for i, card := range subset {
		attempts++
		id := card["order_id"]
		if nullish(id) {
			return nil, &InputError{Path: idx("discovery.orders", i) + ".order_id", Msg: "missing order id"}
		}
		raw, ferr := src.OrderDetail(ctx, jsString(id))
		if ferr != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(ferr, ErrRateLimited) {
				rateLimited = true
				break
			}
			errCount++
		} else {
			detail, derr := decodeAt(raw, fmt.Sprintf("order_detail(%s)", jsString(id)))
			if derr != nil {
				return nil, derr
			}
			readIDs[jsString(id)] = true
			if nonFulfilledStatus(detail) {
				excludedByDetail++
			} else {
				details = append(details, detail)
			}
		}
		if o.DetailDelay > 0 {
			if err := sleep(ctx, o.DetailDelay); err != nil {
				return nil, err
			}
		}
	}
	// Partial failures are tolerated and counted, as upstream; a subset in
	// which no detail at all could be read is an outage, not an empty
	// history.
	if attempts > 0 && len(readIDs) == 0 && (errCount > 0 || rateLimited) {
		return nil, fmt.Errorf("glovologic: no order detail could be read (%d failed, rate limited: %t)", errCount, rateLimited)
	}
	analysis, err := OrderAnalysisFromDetails(details)
	if err != nil {
		return nil, err
	}
	// The cards whose detail was read are marked, which is what detailed_orders
	// counts. d is this call's own decoded copy, not the source's data.
	for _, card := range d.Orders {
		if id := card["order_id"]; !nullish(id) && readIDs[jsString(id)] {
			card["detail_source"] = "detail"
		}
	}
	stats, err := OrderStatsFromCards(d.Orders)
	if err != nil {
		return nil, err
	}
	cov := AnalysisCoverage{
		DiscoveredOrders:     len(d.Orders),
		CursorPages:          len(d.Pages),
		DetailLimit:          o.DetailLimit,
		DetailAttempts:       attempts,
		DetailedOrders:       len(readIDs),
		DetailErrors:         errCount,
		DetailRateLimited:    rateLimited,
		StoppedReason:        d.StoppedReason,
		Strategy:             d.Strategy,
		EligibleOrders:       len(eligible),
		ExcludedNonFulfilled: excludedByCard + excludedByDetail,
		HistoryComplete:      historyComplete(d.StoppedReason),
	}
	cov.EligibleNotAnalyzed = len(eligible) - excludedByDetail - len(details)
	if cov.EligibleNotAnalyzed < 0 {
		cov.EligibleNotAnalyzed = 0
	}
	cov.AnalysisComplete = cov.EligibleNotAnalyzed == 0
	analysis.Limitation = coverageLimitation(cov, len(details))
	return &HistoryAnalysis{
		OrderAnalysis:  *analysis,
		MostOrdered:    mostOrdered(analysis.TopProducts),
		CardStatistics: *stats,
		Coverage:       cov,
		Uncertainty:    collectUncertainty(details, d.Orders, analysis, cov),
	}, nil
}

// historyComplete is true only for the provider's own end-of-list signals.
// Every other stop reason (page cap, loop guards, known_order, unknown, and
// any reason added later) is a history that may have more orders behind it.
func historyComplete(stopped string) bool {
	return stopped == "no_next_cursor" || stopped == "empty_page"
}

// coverageLimitation states what the product counts rest on: orders analysed
// against orders available, and whether the history itself was cut short.
func coverageLimitation(c AnalysisCoverage, analyzed int) string {
	var b strings.Builder
	if c.EligibleNotAnalyzed == 0 {
		fmt.Fprintf(&b, "Product statistics cover all %d eligible discovered orders.", analyzed)
	} else {
		fmt.Fprintf(&b, "Product statistics cover the %d most recent of %d eligible discovered orders (the first in provider order, assumed newest first); %d eligible orders were not analysed (detail limit, read errors or rate limit).",
			analyzed, c.EligibleOrders, c.EligibleNotAnalyzed)
	}
	if c.ExcludedNonFulfilled > 0 {
		fmt.Fprintf(&b, " Cancelled or failed orders excluded from the counts: %d.", c.ExcludedNonFulfilled)
	}
	if !c.HistoryComplete {
		fmt.Fprintf(&b, " The order history itself may be incomplete (discovery stopped: %s), so older orders may never have been discovered.", c.StoppedReason)
	}
	if c.DetailRateLimited {
		b.WriteString(" Detail reads were rate-limited and stopped early.")
	}
	b.WriteString(" Card-only orders are excluded from item analysis; dishes are matched by normalised name, not by product id.")
	return b.String()
}

// mostOrdered reads the winner off the ranked top_products, making ties explicit.
func mostOrdered(top []ProductHistory) *MostOrdered {
	if len(top) == 0 {
		return nil
	}
	w := top[0]
	m := &MostOrdered{Product: w.Product, Orders: w.Orders, Quantity: w.Quantity, TieBreak: "product_name"}
	var tied []string
	for _, p := range top {
		if p.Orders != w.Orders {
			continue
		}
		tied = append(tied, jsString(p.Product))
		if p.Quantity == w.Quantity && jsString(p.Product) != jsString(w.Product) {
			m.UnresolvedTie = true
		}
	}
	if len(tied) > 1 {
		m.TiedOnOrders = tied
	}
	return m
}

// collectUncertainty lists, in a fixed order, what may make the answer
// weaker than it looks. Only reasons that occur are listed.
func collectUncertainty(details []any, cards []Object, analysis *OrderAnalysis, c AnalysisCoverage) []Uncertainty {
	counts := map[string]int{}
	if !c.HistoryComplete {
		counts["history_truncated"] = 1
	}
	if n := c.EligibleOrders - c.DetailAttempts; n > 0 {
		counts["details_not_attempted"] = n
	}
	if c.DetailRateLimited {
		counts["detail_rate_limited"] = 1
	}
	counts["detail_errors"] = c.DetailErrors
	if n := analysis.DistinctProducts - len(analysis.TopProducts); n > 0 {
		counts["products_beyond_top_cap"] = n
	}
	cardStatus := map[string]bool{}
	for _, card := range cards {
		if id := card["order_id"]; !nullish(id) && len(statusText(card)) > 0 {
			cardStatus[jsString(id)] = true
		}
	}
	names := map[string]map[string]bool{}
	stores := map[string]map[string]bool{}
	seenIDs := map[string]bool{}
	for _, o := range details {
		id := coalesce(get(o, "id"), get(o, "orderId"))
		if nullish(id) {
			counts["details_without_id"]++
		} else {
			if seenIDs[jsString(id)] {
				counts["duplicate_detail_ids"]++
			}
			seenIDs[jsString(id)] = true
		}
		if len(statusText(o)) == 0 && (nullish(id) || !cardStatus[jsString(id)]) {
			counts["status_unknown"]++
		}
		store := NormalizeVenue(coalesce(get(o, "storeName"), get(o, "store", "name"), get(o, "providerName"), Null))
		for _, line := range pastOrderLines(o) {
			key := NormalizedProductName(line.name)
			if key == "" {
				counts["lines_without_name"]++
				continue
			}
			if names[key] == nil {
				names[key], stores[key] = map[string]bool{}, map[string]bool{}
			}
			names[key][jsString(line.name)] = true
			if store != "" {
				stores[key][store] = true
			}
		}
	}
	for key := range names {
		if len(names[key]) > 1 {
			counts["product_name_variants_merged"]++
		}
		if len(stores[key]) > 1 {
			counts["same_name_across_stores"]++
		}
	}
	notes := []struct{ code, note string }{
		{"history_truncated", "Order discovery stopped before the provider signalled the end of the list; older orders may exist."},
		{"details_not_attempted", "Eligible orders whose detail was not requested (detail limit or an earlier rate limit)."},
		{"detail_rate_limited", "The provider rate-limited the detail reads; enrichment stopped early."},
		{"detail_errors", "Order details that could not be read and are missing from the counts."},
		{"status_unknown", "Orders with no status on card or detail; a cancelled order among them cannot be told apart."},
		{"product_name_variants_merged", "Products whose lines carry more than one spelling, merged by name normalisation (renamed items are not tracked by id)."},
		{"same_name_across_stores", "Products whose normalised name occurs at more than one store; counts mix those stores."},
		{"lines_without_name", "Order lines without a product name, left out of the product counts."},
		{"details_without_id", "Order details without an id; each counts as its own order."},
		{"duplicate_detail_ids", "Order details sharing an id; the order counts once but quantities are added again."},
		{"products_beyond_top_cap", "Distinct products not listed in top_products (capped at 50)."},
	}
	out := []Uncertainty{}
	for _, n := range notes {
		if counts[n.code] > 0 {
			out = append(out, Uncertainty{Code: n.code, Count: counts[n.code], Note: n.note})
		}
	}
	return out
}
