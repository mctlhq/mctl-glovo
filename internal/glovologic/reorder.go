package glovologic

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// MatchThreshold is the score at which a candidate resolves a past line.
const MatchThreshold = 0.9

var (
	nonAlnumRun  = regexp.MustCompile(`[^a-z0-9]+`)
	digitUnitGap = regexp.MustCompile(`(\d)\s+([a-z])\b`)
)

// stripMarks is NFD with U+0300..U+036F removed.
func stripMarks(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 0x300 && r <= 0x36f {
			return -1
		}
		return r
	}, norm.NFD.String(s))
}

// NormalizedProductName is normalizedProductName (api.mjs:1148).
func NormalizedProductName(v any) string {
	s := ""
	if truthy(v) {
		s = jsString(v)
	}
	s = strings.ToLower(stripMarks(s))
	s = nonAlnumRun.ReplaceAllString(s, " ")
	s = digitUnitGap.ReplaceAllString(s, "$1$2")
	return strings.TrimFunc(s, unicode.IsSpace)
}

// ProductMatchScore is productMatchScore (api.mjs:1158).
func ProductMatchScore(left, right any) float64 {
	a, b := NormalizedProductName(left), NormalizedProductName(right)
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	if (strings.Contains(a, b) || strings.Contains(b, a)) && min(len(a), len(b)) >= 5 {
		return 0.9
	}
	return toFixed(jaccard(strings.Fields(a), strings.Fields(b)), 3)
}

// jaccard is |A∩B| / |A∪B| over token sets.
func jaccard(a, b []string) float64 {
	as, bs := map[string]bool{}, map[string]bool{}
	var union []string
	for _, t := range a {
		if !as[t] {
			as[t] = true
			union = append(union, t)
		}
	}
	overlap := 0
	for t := range as {
		if containsToken(b, t) {
			overlap++
		}
	}
	for _, t := range b {
		if !bs[t] {
			bs[t] = true
			if !as[t] {
				union = append(union, t)
			}
		}
	}
	return float64(overlap) / float64(len(union))
}

func containsToken(tokens []string, t string) bool {
	for _, x := range tokens {
		if x == t {
			return true
		}
	}
	return false
}

func candidateLimit(limit int) int {
	if limit == 0 {
		limit = 3
	}
	return max(1, min(limit, 5))
}

// rankProductCandidates is rankProductCandidates (api.mjs:1170). It never
// mutates the input products.
func rankProductCandidates(name any, products []Object, limit int) []Object {
	seen := map[string]bool{}
	ranked := make([]Object, 0, len(products))
	for _, p := range products {
		score := ProductMatchScore(name, p["name"])
		key := jsString(p["product_id"]) + "|" + jsString(p["external_id"]) + "|" + jsString(p["store_product_id"])
		if score == 0 || seen[key] {
			continue
		}
		seen[key] = true
		c := copyObject(p)
		c["match_score"] = score
		ranked = append(ranked, c)
	}
	coll := collator()
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := ranked[i]["match_score"].(float64), ranked[j]["match_score"].(float64)
		if a != b {
			return a > b
		}
		return coll.CompareString(jsString(ranked[i]["name"]), jsString(ranked[j]["name"])) < 0
	})
	if n := candidateLimit(limit); len(ranked) > n {
		ranked = ranked[:n]
	}
	return ranked
}

// PlanItem is one reorder plan line.
type PlanItem struct {
	Index                     int      `json:"index"`
	PreviousName              any      `json:"previous_name"`
	PreviousQuantity          float64  `json:"previous_quantity"`
	PreviousCustomizations    any      `json:"previous_customizations"`
	RequiresOptionReselection bool     `json:"requires_option_reselection"`
	ResolvedCurrentProduct    bool     `json:"resolved_current_product"`
	RequiresLiveProductCheck  bool     `json:"requires_live_product_check"`
	Candidates                []Object `json:"candidates"`
}

// ReorderPlan is the glovo_plan_reorder tool output. It is read-only:
// MutatesBasket is always false.
type ReorderPlan struct {
	OrderID               any        `json:"order_id,omitempty"`
	Store                 any        `json:"store,omitempty"`
	StoreID               any        `json:"store_id"`
	StoreAddressID        any        `json:"store_address_id"`
	NativeReorderAllowed  *bool      `json:"native_reorder_allowed,omitempty"`
	ItemsCount            int        `json:"items_count"`
	ResolvedItems         int        `json:"resolved_items"`
	UnresolvedItems       int        `json:"unresolved_items"`
	SearchesUsed          int        `json:"searches_used"`
	SearchLimit           int        `json:"search_limit"`
	CanPrepareAfterReview bool       `json:"can_prepare_after_review"`
	MutatesBasket         bool       `json:"mutates_basket"`
	NextStep              string     `json:"next_step"`
	Items                 []PlanItem `json:"items"`
}

const planNextStep = "Inspect each selected candidate with glovo_get_product, reselect required options, obtain explicit approval, then call glovo_add_to_basket. This plan never places an order."

// BuildReorderPlan is buildReorderPlan (api.mjs:1184). order is a raw order
// detail; candidatesByLine maps a line index to its ranked candidates.
func BuildReorderPlan(order any, candidatesByLine map[int][]Object, maxSearches, searched int) (*ReorderPlan, error) {
	if err := checkOrderDetail(order, "order"); err != nil {
		return nil, err
	}
	for i, cands := range candidatesByLine {
		for j, c := range cands {
			if err := checkProduct(c, idx(idx("candidates", i), j)); err != nil {
				return nil, err
			}
		}
	}
	lines := pastOrderLines(order)
	items := make([]PlanItem, len(lines))
	resolvedCount := 0
	for i, line := range lines {
		cands := candidatesByLine[i]
		if cands == nil {
			cands = []Object{}
		}
		resolved := false
		if len(cands) > 0 {
			best := cands[0]
			score := toNumber(best["match_score"])
			resolved = score >= MatchThreshold && truthy(best["product_id"]) && truthy(best["external_id"]) &&
				truthy(best["store_product_id"]) && best["available"] != false
		}
		if resolved {
			resolvedCount++
		}
		items[i] = PlanItem{
			Index:                     i,
			PreviousName:              line.name,
			PreviousQuantity:          line.quantity,
			PreviousCustomizations:    line.customizations,
			RequiresOptionReselection: truthy(line.customizations),
			ResolvedCurrentProduct:    resolved,
			RequiresLiveProductCheck:  resolved,
			Candidates:                cands,
		}
	}
	plan := &ReorderPlan{
		StoreID:               coalesce(get(order, "storeId"), get(order, "store", "id"), Null),
		StoreAddressID:        coalesce(get(order, "storeAddressId"), get(order, "store", "addressId"), Null),
		ItemsCount:            len(items),
		ResolvedItems:         resolvedCount,
		UnresolvedItems:       len(items) - resolvedCount,
		SearchesUsed:          searched,
		SearchLimit:           maxSearches,
		CanPrepareAfterReview: len(items) > 0 && resolvedCount == len(items),
		MutatesBasket:         false,
		NextStep:              planNextStep,
		Items:                 items,
	}
	if !nullish(order) {
		// compactReorderPreview (api.mjs:1268) fields; a null order has none.
		plan.OrderID = coalesce(get(order, "id"), get(order, "orderId"), get(order, "orderCode"), get(order, "code"))
		plan.Store = coalesce(get(order, "storeName"), get(order, "store", "name"), get(order, "providerName"), Null)
		allowed := truthy(get(order, "reorderData", "allowed")) || truthy(get(order, "reorderData", "isAllowed")) || truthy(get(order, "isRemake"))
		plan.NativeReorderAllowed = &allowed
	}
	return plan, nil
}

// PlanOptions are the glovo_plan_reorder inputs.
type PlanOptions struct {
	MaxSearches       int // 0..20, fallback store searches
	CandidatesPerLine int // 1..5
}

// DefaultPlanOptions are the tool defaults (server.mjs:419).
func DefaultPlanOptions() PlanOptions { return PlanOptions{MaxSearches: 5, CandidatesPerLine: 3} }

// PlanReorder is GlovoClient.planReorder (api.mjs:620): rank Easy Reorder
// products against each past line and fall back to at most MaxSearches
// store searches for lines without a match of at least MatchThreshold. It
// reads only; it never changes the basket.
func PlanReorder(ctx context.Context, src ReorderSource, orderID string, o PlanOptions) (*ReorderPlan, error) {
	if o.MaxSearches < 0 || o.MaxSearches > 20 || o.CandidatesPerLine < 1 || o.CandidatesPerLine > 5 {
		return nil, errors.New("glovologic: max_searches must be 0..20 and candidates_per_line 1..5")
	}
	raw, err := src.OrderDetail(ctx, orderID)
	if err != nil {
		return nil, err
	}
	order, err := decodeAt(raw, "order")
	if err != nil {
		return nil, err
	}
	if err := checkOrderDetail(order, "order"); err != nil {
		return nil, err
	}
	storeID := coalesce(get(order, "storeId"), get(order, "store", "id"), get(order, "store", "storeId"))
	storeAddressID := coalesce(get(order, "storeAddressId"), get(order, "store", "addressId"), get(order, "store", "storeAddressId"))
	if !truthy(storeID) || !truthy(storeAddressID) {
		return BuildReorderPlan(order, nil, o.MaxSearches, 0)
	}
	sid, said := jsString(storeID), jsString(storeAddressID)
	content, err := src.StoreContent(ctx, sid, said)
	if err != nil {
		return nil, err
	}
	poolRaw, err := content("easy_reorder", 100)
	if err != nil {
		return nil, err
	}
	pool, err := decodeProducts(poolRaw, "easy_reorder")
	if err != nil {
		return nil, err
	}
	candidates := map[int][]Object{}
	searched := 0
	for i, line := range pastOrderLines(order) {
		ranked := rankProductCandidates(line.name, pool, o.CandidatesPerLine)
		if (len(ranked) == 0 || ranked[0]["match_score"].(float64) < MatchThreshold) && searched < o.MaxSearches && truthy(line.name) {
			searched++
			current, err := searchStore(ctx, src, sid, storeAddressID, jsString(line.name), 24)
			if err != nil {
				return nil, err
			}
			for _, p := range current {
				p["source"] = "store_search"
			}
			ranked = rankProductCandidates(line.name, append(append([]Object{}, pool...), current...), o.CandidatesPerLine)
		}
		candidates[i] = ranked
	}
	return BuildReorderPlan(order, candidates, o.MaxSearches, searched)
}

// searchStore is compactSearch(searchStoreItems(...), {storeId, storeAddressId,
// limit}).results: storeID is written as a string, storeAddressID as given.
func searchStore(ctx context.Context, src StoreSearchSource, storeID string, storeAddressID any, query string, limit int) ([]Object, error) {
	raw, err := src.SearchStoreItems(ctx, storeID, jsString(storeAddressID), query)
	if err != nil {
		return nil, err
	}
	return compactSearchResults(raw, storeID, storeAddressID, limit)
}

func compactSearchResults(raw []byte, storeID any, storeAddressID any, limit int) ([]Object, error) {
	items, err := decodeProducts(raw, "store_search")
	if err != nil {
		return nil, err
	}
	if len(items) > limit {
		items = items[:limit]
	}
	out := make([]Object, len(items))
	for i, p := range items {
		c := copyObject(p)
		setOrDelete(c, "store_id", storeID)
		setOrDelete(c, "store_address_id", storeAddressID)
		out[i] = c
	}
	return out, nil
}
