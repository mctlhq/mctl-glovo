package glovologic

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GlobalStore is one compactGlobalStoreSearch store.
type GlobalStore struct {
	StoreID        string   `json:"store_id"`
	StoreAddressID any      `json:"store_address_id"`
	Name           string   `json:"name"`
	Slug           any      `json:"slug,omitempty"`
	Open           *bool    `json:"open"`
	Rating         *float64 `json:"rating"`
	Votes          *float64 `json:"votes"`
	SearchPosition float64  `json:"search_position"`
	Image          any      `json:"image,omitempty"`
	Products       []Object `json:"products"`
}

// GlobalStoreSearch is the compactGlobalStoreSearch result.
type GlobalStoreSearch struct {
	Count  int            `json:"count"`
	Stores []*GlobalStore `json:"stores"`
}

var navBase, _ = url.Parse("https://glovo.invalid")

// actionParams returns the query of the first NAVIGATION action's path.
func actionParams(actions any) url.Values {
	list, _ := actions.([]any)
	for _, a := range list {
		if get(a, "type") == "NAVIGATION" && truthy(get(a, "data", "path")) {
			path, _ := get(a, "data", "path").(string)
			ref, err := url.Parse(path)
			if err != nil {
				return url.Values{}
			}
			return navBase.ResolveReference(ref).Query()
		}
	}
	return url.Values{}
}

// param is URLSearchParams.get: Null when the key is missing.
func param(v url.Values, k string) any {
	if vs, ok := v[k]; ok && len(vs) > 0 {
		return vs[0]
	}
	return Null
}

// eventData is the first object-valued event data of an element.
func eventData(entry any) any {
	actions, _ := get(entry, "actions").([]any)
	for _, a := range actions {
		events, _ := get(a, "data", "events").([]any)
		for _, e := range events {
			d := get(e, "data")
			if kindOf(d)&(kObject|kArray) != 0 {
				return d
			}
		}
	}
	return Object{}
}

// text is suggestions.mjs text(): a nested label, which must be a string.
func text(v any, path string) (any, error) {
	t := coalesce(get(v, "text", "text"), get(v, "text"), get(v, "label"), v)
	if err := expect(t, path, kString); err != nil {
		return nil, err
	}
	return t, nil
}

func checkActions(entry any, path string) error {
	if err := expect(get(entry, "actions"), path+".actions", kArray); err != nil {
		return err
	}
	actions, _ := get(entry, "actions").([]any)
	for i, a := range actions {
		p := idx(path+".actions", i)
		if err := expect(a, p, kObject); err != nil {
			return err
		}
		if err := expectFields(a, p, map[string]kind{"type": kString, "data": kObject}); err != nil {
			return err
		}
		if err := expectFields(get(a, "data"), p+".data", map[string]kind{"path": kString, "events": kArray}); err != nil {
			return err
		}
		events, _ := get(a, "data", "events").([]any)
		for j, e := range events {
			if err := expect(e, idx(p+".data.events", j), kObject); err != nil {
				return err
			}
		}
	}
	return nil
}

var trackingFields = map[string]kind{
	"shopId": idKinds, "shopRating": kString | kNumber, "numberOfRatedOrders": kString | kNumber,
	"shopRankPosition": kString | kNumber, "shopPosition": kString | kNumber,
	"shopAvailabilityStatus": kString | kBool, "shopIsOpen": kString | kBool,
}

// boolish is suggestions.mjs bool().
func boolish(v any) *bool {
	if b, ok := v.(bool); ok {
		return &b
	}
	s := ""
	if truthy(v) {
		s = strings.ToLower(jsString(v))
	}
	t, f := true, false
	switch s {
	case "true", "open", "available":
		return &t
	case "false", "closed", "unavailable":
		return &f
	}
	return nil
}

func compactGlobalProduct(element any, path string) (Object, error) {
	if err := expect(element, path, kObject); err != nil {
		return nil, err
	}
	if err := checkActions(element, path); err != nil {
		return nil, err
	}
	data := get(element, "data")
	if err := expect(data, path+".data", kObject); err != nil {
		return nil, err
	}
	if err := expectFields(data, path+".data", map[string]kind{"pricing": kObject, "thumbnail": kObject}); err != nil {
		return nil, err
	}
	if err := expect(get(data, "pricing", "finalPrice"), path+".data.pricing.finalPrice", kString|kNumber); err != nil {
		return nil, err
	}
	if err := expect(get(data, "thumbnail", "image"), path+".data.thumbnail.image", kString); err != nil {
		return nil, err
	}
	name, err := text(get(data, "name"), path+".data.name")
	if err != nil {
		return nil, err
	}
	params := actionParams(get(element, "actions"))
	productID, externalID, storeProductID := param(params, "product_id"), param(params, "product_external_id"), param(params, "store_product_id")
	if !truthy(productID) || !truthy(externalID) || !truthy(storeProductID) {
		return nil, nil
	}
	p := Object{
		"product_id": productID, "external_id": externalID, "store_product_id": storeProductID,
		"store_id": param(params, "store_id"), "store_address_id": param(params, "store_address_id"),
		"source": "global_search",
	}
	for _, k := range []string{"category_id", "collection_id", "collection_group_id", "search_id"} {
		if v := param(params, k); truthy(v) {
			p[k] = v
		}
	}
	setOrDelete(p, "name", name)
	setOrDelete(p, "price", get(data, "pricing", "finalPrice"))
	setOrDelete(p, "image", get(data, "thumbnail", "image"))
	return p, nil
}

// CompactGlobalStoreSearch is compactGlobalStoreSearch: store cards with the
// product carousels that follow them, from the raw global search response.
func CompactGlobalStoreSearch(raw []byte) (*GlobalStoreSearch, error) {
	v, err := decodeAt(raw, "search")
	if err != nil {
		return nil, err
	}
	if !nullish(v) {
		if err := expect(v, "search", kObject); err != nil {
			return nil, err
		}
	}
	elemsAny := coalesce(get(v, "data", "body", "data", "elements"), get(v, "body", "data", "elements"))
	if err := expect(elemsAny, "search.elements", kArray); err != nil {
		return nil, err
	}
	elements, _ := elemsAny.([]any)
	out := &GlobalStoreSearch{Stores: []*GlobalStore{}}
	byShop, byStore := map[string]*GlobalStore{}, map[string]*GlobalStore{}
	for i, el := range elements {
		path := idx("search.elements", i)
		if err := expect(el, path, kObject); err != nil {
			return nil, err
		}
		if err := expectFields(el, path, map[string]kind{"type": kString, "data": kObject}); err != nil {
			return nil, err
		}
		switch get(el, "type") {
		case "STORE_CARD_V2":
			if err := checkActions(el, path); err != nil {
				return nil, err
			}
			data := get(el, "data")
			tracking := eventData(el)
			if err := expectFields(tracking, path+".tracking", trackingFields); err != nil {
				return nil, err
			}
			if err := expectFields(data, path+".data", map[string]kind{"slug": kString, "imagePath": kString}); err != nil {
				return nil, err
			}
			title, err := text(get(data, "title"), path+".data.title")
			if err != nil {
				return nil, err
			}
			params := actionParams(get(el, "actions"))
			storeID := param(params, "store_id")
			shopID := or(param(params, "shop_id"), get(tracking, "shopId"))
			if !truthy(storeID) || !truthy(shopID) || !truthy(title) {
				continue
			}
			pos := numberOrNil(coalesce(get(tracking, "shopRankPosition"), get(tracking, "shopPosition")))
			position := float64(len(out.Stores) + 1)
			if pos != nil {
				position = *pos
			}
			s := &GlobalStore{
				StoreID:        storeID.(string),
				StoreAddressID: shopID,
				Name:           title.(string),
				Slug:           get(data, "slug"),
				Open:           boolish(or(param(params, "shop_availability_status"), get(tracking, "shopAvailabilityStatus"), get(tracking, "shopIsOpen"))),
				Rating:         numberOrNil(get(tracking, "shopRating")),
				Votes:          numberOrNil(get(tracking, "numberOfRatedOrders")),
				SearchPosition: position,
				Image:          get(data, "imagePath"),
				Products:       []Object{},
			}
			out.Stores = append(out.Stores, s)
			byShop[jsString(shopID)] = s
			byStore[storeID.(string)] = s
		case "CAROUSEL":
			if err := checkActions(el, path); err != nil {
				return nil, err
			}
			if err := expect(get(el, "data", "elements"), path+".data.elements", kArray); err != nil {
				return nil, err
			}
			items, _ := get(el, "data", "elements").([]any)
			var products []Object
			for j, it := range items {
				p, err := compactGlobalProduct(it, idx(path+".data.elements", j))
				if err != nil {
					return nil, err
				}
				if p != nil {
					products = append(products, p)
				}
			}
			if len(products) == 0 {
				continue
			}
			tracking := eventData(el)
			if err := expectFields(tracking, path+".tracking", trackingFields); err != nil {
				return nil, err
			}
			s := byShop[jsString(get(tracking, "shopId"))]
			if s == nil {
				s = byStore[jsString(products[0]["store_id"])]
			}
			if s == nil {
				continue
			}
			if truthy(products[0]["store_address_id"]) {
				s.StoreAddressID = products[0]["store_address_id"]
			}
			s.Products = append(s.Products, products...)
		}
	}
	out.Count = len(out.Stores)
	return out, nil
}

func queryRelevance(name any, query string) float64 {
	haystack := NormalizeVenue(name)
	tokens := strings.Fields(NormalizeVenue(query))
	if len(tokens) == 0 {
		return 0
	}
	hits := 0
	for _, t := range tokens {
		if strings.Contains(haystack, t) {
			hits++
		}
	}
	return float64(hits) / float64(len(tokens))
}

type candidateStore struct {
	*GlobalStore
	familiar bool
	history  *Venue
	quality  *float64
}

func attachHistory(stores []*GlobalStore, h *VenueHistory) []*candidateStore {
	counts := map[string]int{}
	for _, s := range stores {
		counts[NormalizeVenue(s.Name)]++
	}
	out := make([]*candidateStore, len(stores))
	for i, s := range stores {
		key := NormalizeVenue(s.Name)
		var hist *Venue
		if counts[key] == 1 {
			hist = h.Venues[key]
		}
		out[i] = &candidateStore{GlobalStore: s, familiar: hist != nil, history: hist, quality: shrunkQuality(nullableFloat(s.Rating), nullableFloat(s.Votes))}
	}
	return out
}

func orZero(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func sortFamiliar(list []*candidateStore) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		var ha, hb float64
		if a.history != nil {
			ha = a.history.Score
		}
		if b.history != nil {
			hb = b.history.Score
		}
		if ha != hb {
			return hb < ha
		}
		if qa, qb := orZero(a.quality), orZero(b.quality); qa != qb {
			return qb < qa
		}
		return a.SearchPosition < b.SearchPosition
	})
}

func sortExplore(list []*candidateStore) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if qa, qb := orZero(a.quality), orZero(b.quality); qa != qb {
			return qb < qa
		}
		return a.SearchPosition < b.SearchPosition
	})
}

type intent struct {
	mode, query, venueQuery, itemMode, qualityPreference, historyRefresh string
	knownLikedOnly, includeGoogle, includeGoogleReviews                  bool
	noveltyTolerance                                                     float64
	maxChoices                                                           int
}

func capStores(list []*candidateStore, n int) []*candidateStore {
	if len(list) > n {
		return list[:n]
	}
	return list
}

func selectStores(stores []*candidateStore, in intent) []*candidateStore {
	var familiar, novel []*candidateStore
	for _, s := range stores {
		if s.familiar {
			familiar = append(familiar, s)
		} else {
			novel = append(novel, s)
		}
	}
	sortFamiliar(familiar)
	sortExplore(novel)
	limit := in.maxChoices * 3
	if in.knownLikedOnly || in.mode == "repeat" {
		out := append([]*candidateStore{}, familiar...)
		if !in.knownLikedOnly {
			out = append(out, novel...)
		}
		return capStores(out, limit)
	}
	if in.mode == "explore" {
		return capStores(append(append([]*candidateStore{}, novel...), familiar...), limit)
	}
	novelTarget := max(1, min(in.maxChoices-1, int(math.Floor(float64(in.maxChoices)*in.noveltyTolerance+0.5))))
	selected := append(append([]*candidateStore{}, familiar[:min(len(familiar), max(0, in.maxChoices-novelTarget))]...), novel[:min(len(novel), novelTarget)]...)
	ids := map[string]bool{}
	for _, s := range selected {
		ids[s.StoreID+":"+jsString(s.StoreAddressID)] = true
	}
	for _, s := range stores {
		if !ids[s.StoreID+":"+jsString(s.StoreAddressID)] {
			selected = append(selected, s)
		}
	}
	return capStores(selected, limit)
}

func chooseProduct(products []Object, query string, exclude map[string]bool) Object {
	var best Object
	bestRel := 0.0
	for _, p := range products {
		if !truthy(p["product_id"]) || !truthy(p["external_id"]) || !truthy(p["store_product_id"]) {
			continue
		}
		if exclude[NormalizeVenue(p["name"])] {
			continue
		}
		rel := queryRelevance(p["name"], query)
		if rel > bestRel {
			best, bestRel = p, rel
		}
	}
	return best
}

func withStore(products []Object, s *candidateStore) []Object {
	out := make([]Object, len(products))
	for i, p := range products {
		c := copyObject(p)
		c["store_id"] = s.StoreID
		c["store_address_id"] = nullable(s.StoreAddressID)
		out[i] = c
	}
	return out
}

func isInputError(err error) bool {
	var ie *InputError
	return errors.As(err, &ie)
}

func optString(v any) string {
	if nullish(v) {
		return ""
	}
	return jsString(v)
}

// resolveFailures counts the reads resolveProduct could not make.
type resolveFailures struct {
	content int // personalized carousels; optional, as upstream
	lookup  int // store searches and product views
}

// resolveProduct is resolveProduct: a live, addable product for the store,
// or nil. A failed read skips the store, as upstream, but is counted in f
// so the caller can tell an outage from "nothing matched"; malformed data
// is an *InputError.
func resolveProduct(ctx context.Context, src SuggestionSource, s *candidateStore, in intent, sleep Sleep, f *resolveFailures) (Object, error) {
	var easy, top []Object
	if s.familiar || in.itemMode != "any" {
		content, err := src.StoreContent(ctx, s.StoreID, jsString(s.StoreAddressID))
		if err != nil && !isInputError(err) {
			f.content++
		}
		if err == nil {
			for _, k := range []struct {
				kind string
				dst  *[]Object
			}{{"easy_reorder", &easy}, {"top_sellers", &top}} {
				raw, cerr := content(k.kind, 40)
				if cerr != nil {
					if isInputError(cerr) {
						return nil, cerr
					}
					f.content++
					break
				}
				ps, derr := decodeProducts(raw, "store_content."+k.kind)
				if derr != nil {
					return nil, derr
				}
				*k.dst = withStore(ps, s)
			}
		} else if isInputError(err) {
			return nil, err
		}
	}
	easyNames := map[string]bool{}
	for _, p := range easy {
		easyNames[NormalizeVenue(p["name"])] = true
	}
	exclude := map[string]bool{}
	if in.itemMode == "different" {
		exclude = easyNames
	}
	pools := [][]Object{s.Products, top, easy}
	switch in.itemMode {
	case "repeat":
		pools = [][]Object{easy, s.Products, top}
	case "different":
		pools = [][]Object{top, s.Products, easy}
	}
	var candidate Object
	for _, pool := range pools {
		if candidate = chooseProduct(pool, in.query, exclude); candidate != nil {
			break
		}
	}
	if candidate == nil {
		results, err := withRetry(ctx, sleep, 2, func() ([]Object, error) {
			return searchStore(ctx, src, s.StoreID, nullable(s.StoreAddressID), in.query, 12)
		})
		if err != nil {
			if isInputError(err) {
				return nil, err
			}
			f.lookup++
			return nil, nil
		}
		candidate = chooseProduct(results, in.query, exclude)
	}
	if candidate == nil {
		return nil, nil
	}
	req := ProductRequest{
		StoreID: s.StoreID, StoreAddressID: jsString(s.StoreAddressID),
		ProductID: optString(candidate["product_id"]), ExternalID: optString(candidate["external_id"]),
		CategoryID: optString(candidate["category_id"]), CollectionID: optString(candidate["collection_id"]),
		CollectionGroupID: optString(candidate["collection_group_id"]), SearchID: optString(candidate["search_id"]),
	}
	detail, err := withRetry(ctx, sleep, 2, func() (Object, error) {
		raw, err := src.ProductView(ctx, req)
		if err != nil {
			return nil, err
		}
		v, err := decodeAt(raw, "product_view")
		if err != nil {
			return nil, err
		}
		o, ok := v.(map[string]any)
		if !ok {
			return nil, &InputError{Path: "product_view", Msg: "want object, got " + typeName(v)}
		}
		return o, checkProductView(o, "product_view")
	})
	if err != nil {
		if isInputError(err) {
			return nil, err
		}
		f.lookup++
		return nil, nil
	}
	if detail["add_enabled"] != true {
		return nil, nil
	}
	out := copyObject(detail)
	out["store_id"] = s.StoreID
	out["store_address_id"] = nullable(s.StoreAddressID)
	setOrDelete(out, "category_id", candidate["category_id"])
	out["source"] = or(candidate["source"], "store_search")
	required := 0
	groups, _ := detail["option_groups"].([]any)
	for _, g := range groups {
		if truthy(get(g, "required")) {
			required++
		}
	}
	out["required_option_groups"] = required
	return out, nil
}

// HistoryComponents are the multi-scale recency components.
type HistoryComponents struct {
	HalfLife5  *float64  `json:"half_life_5"`
	HalfLife20 *float64  `json:"half_life_20"`
	HalfLife80 *float64  `json:"half_life_80"`
	Weights    []float64 `json:"weights"`
}

// HistoryEvidence is personalized_evidence.
type HistoryEvidence struct {
	Familiar          bool               `json:"familiar"`
	VenueOrders       int                `json:"venue_orders"`
	LastOrderEventAge *int               `json:"last_order_event_age,omitempty"`
	RecencyScore      *float64           `json:"recency_score"`
	Components        *HistoryComponents `json:"components,omitempty"`
}

func r3(f float64) *float64 { return finiteOrNil(toFixed(f, 3)) }

func historyEvidence(h *Venue) HistoryEvidence {
	if h == nil {
		zero := 0.0
		return HistoryEvidence{RecencyScore: &zero}
	}
	age := h.LastOrderAge
	return HistoryEvidence{
		Familiar: true, VenueOrders: h.OrderCount, LastOrderEventAge: &age, RecencyScore: r3(h.Score),
		Components: &HistoryComponents{HalfLife5: r3(h.Short), HalfLife20: r3(h.Medium), HalfLife80: r3(h.Long), Weights: MultiscaleVenueModel.ScaleWeights},
	}
}

func labelFor(s *candidateStore, index int) string {
	if !s.familiar {
		if s.quality == nil {
			return "new_live_option"
		}
		return "new_high_rated_option"
	}
	if index == 0 {
		return "reliable_repeat"
	}
	return "familiar_alternative"
}

func reasonFor(s *candidateStore, product Object, in intent) []string {
	var reasons []string
	if s.familiar {
		reasons = append(reasons, "Previously ordered from this venue "+strconv.Itoa(s.history.OrderCount)+" time(s); ranked by the validated multi-scale recency model.")
	} else {
		reasons = append(reasons, "Not found in the card-level venue history; included as a separate live-catalog exploration option.")
	}
	price := "a live catalog price"
	if truthy(product["price"]) {
		price = jsString(product["price"])
	}
	reasons = append(reasons, "Glovo currently exposes "+jsString(product["name"])+" at "+price+".")
	if n, _ := product["required_option_groups"].(int); n != 0 {
		reasons = append(reasons, strconv.Itoa(n)+" required option group(s) must be selected before a basket add.")
	}
	if in.itemMode == "different" {
		reasons = append(reasons, "The resolver excluded current Easy Reorder names where that authenticated carousel was available.")
	}
	return reasons
}

// ChoiceStore is the store block of one choice.
type ChoiceStore struct {
	StoreID                string   `json:"store_id"`
	StoreAddressID         any      `json:"store_address_id"`
	Name                   string   `json:"name"`
	Slug                   any      `json:"slug,omitempty"`
	Open                   *bool    `json:"open"`
	GlovoRating            *float64 `json:"glovo_rating"`
	GlovoRatingCount       *float64 `json:"glovo_rating_count"`
	GlovoCountAwareQuality *float64 `json:"glovo_count_aware_quality"`
	SearchPosition         float64  `json:"search_position"`
}

// Choice is one suggestion.
type Choice struct {
	Label                string          `json:"label"`
	Store                ChoiceStore     `json:"store"`
	Product              Object          `json:"product"`
	PersonalizedEvidence HistoryEvidence `json:"personalized_evidence"`
	Reasons              []string        `json:"reasons"`
	GoogleMaps           Object          `json:"google_maps"`
}

// SuggestionIntent echoes the normalized intent.
type SuggestionIntent struct {
	Mode                   string  `json:"mode"`
	Query                  string  `json:"query"`
	VenueQuery             *string `json:"venue_query"`
	ItemMode               string  `json:"item_mode"`
	KnownLikedConstraint   bool    `json:"known_liked_constraint"`
	QualityPreference      string  `json:"quality_preference"`
	NoveltyTolerance       float64 `json:"novelty_tolerance"`
	MaxChoices             int     `json:"max_choices"`
	GoogleReviewsRequested bool    `json:"google_reviews_requested"`
	HistoryRefresh         string  `json:"history_refresh"`
}

// SuggestionModel describes the ranking model and its evidence.
type SuggestionModel struct {
	Name          string          `json:"name"`
	Level         string          `json:"level"`
	Evidence      string          `json:"evidence"`
	EvidenceScope string          `json:"evidence_scope"`
	Holdout       HoldoutEvidence `json:"holdout"`
	ProductModel  string          `json:"product_model"`
}

// SuggestionCoverage reports what the suggestions are based on.
type SuggestionCoverage struct {
	OrderCardsDiscovered                 int           `json:"order_cards_discovered"`
	OrderCursorPages                     int           `json:"order_cursor_pages"`
	OrderCursorPagesFetchedThisCall      int           `json:"order_cursor_pages_fetched_this_call"`
	OrderCursorPagesAtLastFullRefresh    any           `json:"order_cursor_pages_at_last_full_refresh"`
	OrderCursorStop                      string        `json:"order_cursor_stop"`
	HistoryCache                         CacheMetadata `json:"history_cache"`
	VenueModelOrders                     int           `json:"venue_model_orders"`
	LiveStoreCandidates                  int           `json:"live_store_candidates"`
	ResolvedLiveProducts                 int           `json:"resolved_live_products"`
	ProductDetailHistoryOrdersForRanking int           `json:"product_detail_history_orders_used_for_ranking"`
	BenchmarkDetailedOrders              int           `json:"benchmark_detailed_orders"`
	ProductRankingSource                 string        `json:"product_ranking_source"`
	// Not in upstream: reads that failed while resolving products. Store
	// content is optional; lookups are store searches and product views.
	StoreContentFailures  int `json:"store_content_failures"`
	ProductLookupFailures int `json:"product_lookup_failures"`
}

// GoogleQuality summarizes optional Google Maps evidence.
type GoogleQuality struct {
	Requested            bool   `json:"requested"`
	Available            bool   `json:"available"`
	Enriched             int    `json:"enriched"`
	AmbiguousOrUnmatched int    `json:"ambiguous_or_unmatched"`
	Failures             int    `json:"failures"`
	RankingEffect        string `json:"ranking_effect"`
	ReviewsRequested     bool   `json:"reviews_requested"`
	ReviewVenuesEnriched int    `json:"review_venues_enriched"`
	Reason               string `json:"reason,omitempty"`
}

// Suggestions is the glovo_get_suggestions tool output. It is read-only:
// MutatesBasket is always false.
type Suggestions struct {
	Intent        SuggestionIntent   `json:"intent"`
	Model         SuggestionModel    `json:"model"`
	Coverage      SuggestionCoverage `json:"coverage"`
	Choices       []Choice           `json:"choices"`
	GoogleQuality GoogleQuality      `json:"google_quality"`
	Limitations   []string           `json:"limitations"`
	MutatesBasket bool               `json:"mutates_basket"`
}

// SuggestionInput is the glovo_get_suggestions input; empty strings and nil
// pointers are absent arguments.
type SuggestionInput struct {
	Mode                 string // repeat, explore, balanced (default)
	Query                string
	VenueQuery           string
	ItemMode             string // repeat, different, any (default)
	KnownLikedOnly       bool
	QualityPreference    string // personal, glovo, google, balanced (default)
	NoveltyTolerance     *float64
	MaxChoices           *int
	IncludeGoogle        bool
	IncludeGoogleReviews bool
	HistoryRefresh       string // incremental (default), full
}

// SessionState is what getSuggestions reads from the signed-in session.
type SessionState struct {
	SignedIn    bool
	HasLocation bool
	CustomerID  *string // nil disables the history cache
	Location    Location
}

// SuggestOptions configure getSuggestions. With neither GoogleProvider nor
// GoogleAPIKey, Google evidence is reported as not configured.
type SuggestOptions struct {
	Cache          HistoryCache
	GoogleAPIKey   string
	GoogleProvider GoogleProvider
	Sleep          Sleep // nil means a real timer
}

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

func normalizeIntent(in SuggestionInput) (intent, error) {
	it := intent{
		mode: in.Mode, query: strings.TrimSpace(in.Query), venueQuery: strings.TrimSpace(in.VenueQuery),
		itemMode: in.ItemMode, knownLikedOnly: in.KnownLikedOnly, qualityPreference: in.QualityPreference,
		noveltyTolerance: 0.4, maxChoices: 5, includeGoogle: in.IncludeGoogle, includeGoogleReviews: in.IncludeGoogleReviews,
		historyRefresh: "incremental",
	}
	if it.mode == "" {
		it.mode = "balanced"
	}
	if it.itemMode == "" {
		it.itemMode = "any"
	}
	if it.qualityPreference == "" {
		it.qualityPreference = "balanced"
	}
	switch {
	case !oneOf(it.mode, "repeat", "explore", "balanced"):
		return it, errors.New("glovologic: mode must be repeat, explore or balanced")
	case !oneOf(it.itemMode, "repeat", "different", "any"):
		return it, errors.New("glovologic: item_mode must be repeat, different or any")
	case !oneOf(it.qualityPreference, "personal", "glovo", "google", "balanced"):
		return it, errors.New("glovologic: quality_preference must be personal, glovo, google or balanced")
	case !oneOf(in.HistoryRefresh, "", "incremental", "full"):
		return it, errors.New("glovologic: history_refresh must be incremental or full")
	}
	if in.HistoryRefresh == "full" {
		it.historyRefresh = "full"
	}
	if in.NoveltyTolerance != nil {
		if *in.NoveltyTolerance < 0 || *in.NoveltyTolerance > 1 || math.IsNaN(*in.NoveltyTolerance) {
			return it, errors.New("glovologic: novelty_tolerance must be 0..1")
		}
		it.noveltyTolerance = *in.NoveltyTolerance
	}
	if in.MaxChoices != nil {
		if *in.MaxChoices < 3 || *in.MaxChoices > 5 {
			return it, errors.New("glovologic: max_choices must be 3..5")
		}
		it.maxChoices = *in.MaxChoices
	}
	if it.query == "" {
		return it, errors.New("suggestion query is required")
	}
	return it, nil
}

var suggestionLimitations = []string{
	"Order cards support venue prediction across full cursor discovery; product history covers only a bounded 15-order detailed subset.",
	"A past order proves familiarity, not satisfaction. known_liked_only is honored only as an explicit user constraint.",
	"Exploration ordering is a transparent live-catalog heuristic, not the backtested repeat model.",
	"Current product availability and required options are re-fetched; the suggestion tool never changes the basket.",
}

// GetSuggestions is getSuggestions (suggestions.mjs): 3-5 read-only live
// choices ranked by the venue history model. It never changes the basket.
func GetSuggestions(ctx context.Context, src SuggestionSource, sess SessionState, input SuggestionInput, o SuggestOptions) (*Suggestions, error) {
	in, err := normalizeIntent(input)
	if err != nil {
		return nil, err
	}
	if !sess.SignedIn {
		return nil, errors.New("suggestions require an authenticated Glovo session for order history")
	}
	if !sess.HasLocation {
		return nil, errors.New("select or confirm a Glovo delivery location before requesting suggestions")
	}
	sleep := orSleep(o.Sleep)

	var (
		wg        sync.WaitGroup
		liveRaw   []byte
		searchErr error
	)
	// The live search and the history walk run concurrently on src, as
	// upstream; a failed history walk cancels the search instead of
	// waiting out its retries.
	searchCtx, cancelSearch := context.WithCancel(ctx)
	defer cancelSearch()
	wg.Add(1)
	go func() {
		defer wg.Done()
		liveRaw, searchErr = withRetry(searchCtx, sleep, 2, func() ([]byte, error) { return src.SearchStores(searchCtx, in.query) })
	}()
	discovery, cacheErr := o.Cache.CachedOrderCards(ctx, src, sess.CustomerID, CacheOptions{Refresh: in.historyRefresh, PageDelay: 750 * time.Millisecond, MaxRetries: 4})
	if cacheErr != nil {
		cancelSearch()
	}
	wg.Wait()
	if cacheErr != nil {
		return nil, cacheErr
	}
	if searchErr != nil {
		return nil, searchErr
	}
	history, err := RankVenueHistory(discovery.Orders)
	if err != nil {
		return nil, err
	}
	live, err := CompactGlobalStoreSearch(liveRaw)
	if err != nil {
		return nil, err
	}
	needle := NormalizeVenue(in.venueQuery)
	var open []*GlobalStore
	for _, s := range live.Stores {
		if s.Open != nil && !*s.Open {
			continue
		}
		if needle != "" && !strings.Contains(NormalizeVenue(s.Name), needle) {
			continue
		}
		open = append(open, s)
	}
	candidates := attachHistory(open, history)
	choices := []Choice{}
	var failures resolveFailures
	for _, s := range selectStores(candidates, in) {
		if len(choices) >= in.maxChoices {
			break
		}
		product, err := resolveProduct(ctx, src, s, in, sleep, &failures)
		if err != nil {
			return nil, err
		}
		if product == nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		choices = append(choices, Choice{
			Label: labelFor(s, len(choices)),
			Store: ChoiceStore{
				StoreID: s.StoreID, StoreAddressID: nullable(s.StoreAddressID), Name: s.Name, Slug: s.Slug, Open: s.Open,
				GlovoRating: normalizedRating(nullableFloat(s.Rating)), GlovoRatingCount: s.Votes,
				GlovoCountAwareQuality: roundedOrNil(s.quality, 3), SearchPosition: s.SearchPosition,
			},
			Product:              product,
			PersonalizedEvidence: historyEvidence(s.history),
			Reasons:              reasonFor(s, product, in),
		})
	}

	if len(choices) == 0 && failures.lookup > 0 {
		return nil, fmt.Errorf("glovologic: no live product could be resolved: %d store searches or product views failed", failures.lookup)
	}

	useGoogle := in.includeGoogle || in.includeGoogleReviews || in.qualityPreference == "google"
	provider := o.GoogleProvider
	if provider == nil {
		provider = NewGooglePlacesProvider(o.GoogleAPIKey, nil, "")
	}
	gq := GoogleQuality{
		Requested: useGoogle, Available: provider != nil,
		RankingEffect:    "none; Google evidence is not part of the backtested venue model",
		ReviewsRequested: in.includeGoogleReviews,
	}
	if useGoogle && provider != nil {
		for i := range choices {
			if i >= 5 {
				break
			}
			g, err := provider(ctx, GoogleRequest{
				VenueName: choices[i].Store.Name, Location: sess.Location, CityCode: sess.Location.CityCode,
				IncludeReviews: in.includeGoogleReviews && i < 3, ReviewLimit: 3,
			})
			status, okStatus := get(g, "match_status").(string)
			switch {
			case err != nil || (!nullish(get(g, "match_status")) && !okStatus):
				gq.Failures++
			case status == "matched":
				choices[i].GoogleMaps = g
				gq.Enriched++
				if get(g, "reviews_status") == "returned" {
					gq.ReviewVenuesEnriched++
				}
			default:
				gq.AmbiguousOrUnmatched++
			}
		}
	}
	if provider == nil {
		gq.Reason = "GOOGLE_MAPS_API_KEY is not configured; Glovo and personalized evidence remain available."
	}

	var venueQuery *string
	if in.venueQuery != "" {
		venueQuery = &in.venueQuery
	}
	return &Suggestions{
		Intent: SuggestionIntent{
			Mode: in.mode, Query: in.query, VenueQuery: venueQuery, ItemMode: in.itemMode,
			KnownLikedConstraint: in.knownLikedOnly, QualityPreference: in.qualityPreference,
			NoveltyTolerance: in.noveltyTolerance, MaxChoices: in.maxChoices,
			GoogleReviewsRequested: in.includeGoogleReviews, HistoryRefresh: in.historyRefresh,
		},
		Model: SuggestionModel{
			Name: "venue-multi-scale-recency-5-20-80", Level: "venue",
			Evidence:      "chronological rolling-origin backtest with untouched final holdout",
			EvidenceScope: "aggregate benchmark from one private account; not a universal quality guarantee",
			Holdout:       VenueHoldoutEvidence,
			ProductModel:  "not_promoted; the benchmark had only 15 detailed orders",
		},
		Coverage: SuggestionCoverage{
			OrderCardsDiscovered:              len(discovery.Orders),
			OrderCursorPages:                  len(discovery.Pages),
			OrderCursorPagesFetchedThisCall:   len(discovery.Pages),
			OrderCursorPagesAtLastFullRefresh: coalesce(discovery.Cache.FullCursorPagesAtLastFullRefresh, len(discovery.Pages)),
			OrderCursorStop:                   discovery.StoppedReason,
			HistoryCache:                      discovery.Cache,
			VenueModelOrders:                  history.Cards,
			LiveStoreCandidates:               len(candidates),
			ResolvedLiveProducts:              len(choices),
			BenchmarkDetailedOrders:           15,
			ProductRankingSource:              "live Glovo catalog plus Easy Reorder/Top Sellers when available; no learned product model",
			StoreContentFailures:              failures.content,
			ProductLookupFailures:             failures.lookup,
		},
		Choices:       choices,
		GoogleQuality: gq,
		Limitations:   suggestionLimitations,
		MutatesBasket: false,
	}, nil
}
