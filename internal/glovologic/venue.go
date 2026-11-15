package glovologic

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

// VenueModel is one venue ranking model (venue-model.mjs).
type VenueModel struct {
	Name         string    `json:"name"`
	Complexity   int       `json:"complexity"`
	Frequency    float64   `json:"frequency"`
	Recency      float64   `json:"recency"`
	Transition   float64   `json:"transition"`
	HalfLife     float64   `json:"halfLife"`
	Scales       []float64 `json:"scales,omitempty"`
	ScaleWeights []float64 `json:"scaleWeights,omitempty"`
}

// MultiscaleVenueModel is MULTISCALE_VENUE_MODEL, the promoted model.
var MultiscaleVenueModel = VenueModel{
	Name: "multiscale_5_20_80", Complexity: 3, Recency: 1, HalfLife: 20,
	Scales: []float64{5, 20, 80}, ScaleWeights: []float64{0.5, 0.3, 0.2},
}

// HoldoutEvidence is VENUE_HOLDOUT_EVIDENCE, the published benchmark.
type HoldoutEvidence struct {
	SourceSnapshot      string  `json:"source_snapshot"`
	UsableOrders        int     `json:"usable_orders"`
	TrainOrders         int     `json:"train_orders"`
	ValidationOrders    int     `json:"validation_orders"`
	FinalTestOrders     int     `json:"final_test_orders"`
	PrecisionAt5        float64 `json:"precision_at_5"`
	RecallAt5           float64 `json:"recall_at_5"`
	NDCGAt5             float64 `json:"ndcg_at_5"`
	RepeatRecallAt5     float64 `json:"repeat_recall_at_5"`
	NovelTargetShare    float64 `json:"novel_target_share"`
	PopularityRecallAt5 float64 `json:"popularity_recall_at_5"`
	PopularityNDCGAt5   float64 `json:"popularity_ndcg_at_5"`
}

// VenueHoldoutEvidence is VENUE_HOLDOUT_EVIDENCE.
var VenueHoldoutEvidence = HoldoutEvidence{
	SourceSnapshot: "2026-06-28", UsableOrders: 910, TrainOrders: 637, ValidationOrders: 136, FinalTestOrders: 137,
	PrecisionAt5: 0.0832, RecallAt5: 0.4161, NDCGAt5: 0.2712, RepeatRecallAt5: 0.479, NovelTargetShare: 0.1314,
	PopularityRecallAt5: 0.2044, PopularityNDCGAt5: 0.1599,
}

var spaceRun = regexp.MustCompile(`\s+`)

// NormalizeVenue is normalizeVenue (venue-model.mjs).
func NormalizeVenue(v any) string {
	s := ""
	if truthy(v) {
		s = jsString(v)
	}
	s = strings.ToLower(stripMarks(s))
	s = nonAlnumRun.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	return spaceRun.ReplaceAllString(s, " ")
}

var cancelPattern = regexp.MustCompile(`(?i)cancel`)

func isCancelled(card Object) bool {
	status, total := "", ""
	if truthy(card["status"]) {
		status = jsString(card["status"])
	}
	if truthy(card["total"]) {
		total = jsString(card["total"])
	}
	return cancelPattern.MatchString(status + " " + total)
}

// VenueOrder is one usable venue event.
type VenueOrder struct {
	Store        string  `json:"store"`
	DisplayName  any     `json:"displayName,omitempty"`
	HistoryIndex float64 `json:"-"`
	SourceIndex  int     `json:"sourceIndex"`
}

// PrepareVenueOrders is prepareVenueOrders: drop cancelled and nameless
// cards and return them oldest first.
func PrepareVenueOrders(cards []Object, newestFirst bool) ([]VenueOrder, error) {
	var prepared []VenueOrder
	index := 0
	for i, card := range cards {
		if err := checkOrderCard(card, idx("orders", i)); err != nil {
			return nil, err
		}
		if isCancelled(card) {
			continue
		}
		name := coalesce(card["store"], card["store_name"])
		o := VenueOrder{Store: NormalizeVenue(name), DisplayName: name, HistoryIndex: toNumber(card["history_index"]), SourceIndex: index}
		index++
		if o.Store != "" {
			prepared = append(prepared, o)
		}
	}
	allFinite := true
	for _, o := range prepared {
		if math.IsNaN(o.HistoryIndex) || math.IsInf(o.HistoryIndex, 0) {
			allFinite = false
			break
		}
	}
	if allFinite {
		sort.SliceStable(prepared, func(i, j int) bool { return prepared[i].HistoryIndex > prepared[j].HistoryIndex })
		return prepared, nil
	}
	if newestFirst {
		for i, j := 0, len(prepared)-1; i < j; i, j = i+1, j-1 {
			prepared[i], prepared[j] = prepared[j], prepared[i]
		}
	}
	return prepared, nil
}

// Split is a chronological train/validation/test split.
type Split struct {
	Train, Validation, Test []VenueOrder
}

// TemporalSplit is temporalSplit with the 70/15/15 default.
func TemporalSplit(orders []VenueOrder, trainRatio, validationRatio float64) (Split, error) {
	if len(orders) < 3 {
		return Split{}, errors.New("at least 3 usable orders are required")
	}
	n := float64(len(orders))
	trainEnd := max(1, int(math.Floor(n*trainRatio)))
	validationEnd := max(trainEnd+1, int(math.Floor(n*(trainRatio+validationRatio))))
	validationEnd = min(validationEnd, len(orders))
	return Split{Train: orders[:trainEnd], Validation: orders[trainEnd:validationEnd], Test: orders[validationEnd:]}, nil
}

func modelCandidates() []VenueModel {
	models := []VenueModel{
		{Name: "popularity", Complexity: 1, Frequency: 1, HalfLife: 50},
		{Name: "transition", Complexity: 2, Transition: 1, HalfLife: 50},
		MultiscaleVenueModel,
		{Name: "multiscale_10_30_120", Complexity: 3, Recency: 1, HalfLife: 30, Scales: []float64{10, 30, 120}, ScaleWeights: []float64{0.5, 0.3, 0.2}},
	}
	for _, h := range []float64{10, 30, 100} {
		models = append(models, VenueModel{Name: fmt.Sprintf("recency_h%s", numberToString(h)), Complexity: 2, Recency: 1, HalfLife: h})
		for _, w := range [][3]float64{{0.6, 0.25, 0.15}, {0.45, 0.35, 0.2}, {0.4, 0.2, 0.4}, {0.25, 0.35, 0.4}} {
			models = append(models, VenueModel{
				Name:       fmt.Sprintf("hybrid_%s_%s_%s_h%s", numberToString(w[0]), numberToString(w[1]), numberToString(w[2]), numberToString(h)),
				Complexity: 4, Frequency: w[0], Recency: w[1], Transition: w[2], HalfLife: h,
			})
		}
	}
	return models
}

type venueStore struct {
	count       int
	occurrences []int
	displayName any
}

type venueState struct {
	step          int
	previousStore string
	stores        map[string]*venueStore
	storeOrder    []string
	transitions   map[string]map[string]int
}

func newVenueState() *venueState {
	return &venueState{stores: map[string]*venueStore{}, transitions: map[string]map[string]int{}}
}

func (s *venueState) ingest(o VenueOrder) {
	st := s.stores[o.Store]
	if st == nil {
		st = &venueStore{displayName: o.DisplayName}
		s.stores[o.Store] = st
		s.storeOrder = append(s.storeOrder, o.Store)
	}
	if s.previousStore != "" {
		next := s.transitions[s.previousStore]
		if next == nil {
			next = map[string]int{}
			s.transitions[s.previousStore] = next
		}
		next[o.Store]++
	}
	st.count++
	st.occurrences = append(st.occurrences, s.step)
	if truthy(o.DisplayName) {
		st.displayName = o.DisplayName
	}
	s.previousStore = o.Store
	s.step++
}

func recencyValue(occurrences []int, step int, m VenueModel) float64 {
	if m.Scales == nil {
		age := max(0, step-occurrences[len(occurrences)-1])
		return math.Pow(2, -float64(age)/m.HalfLife)
	}
	total := 0.0
	for _, occ := range occurrences {
		age := float64(max(0, step-occ))
		sum := 0.0
		for i, scale := range m.Scales {
			sum += m.ScaleWeights[i] * math.Pow(2, -age/scale)
		}
		total += sum
	}
	return total
}

func summedDecay(occurrences []int, step int, halfLife float64) float64 {
	sum := 0.0
	for _, occ := range occurrences {
		sum += math.Pow(2, -float64(max(0, step-occ))/halfLife)
	}
	return sum
}

type rankedRow struct {
	key                            string
	displayName                    any
	count                          int
	frequency, recency, transition float64
	lastOrderAge                   int
	score                          float64
}

func (s *venueState) rankedRows(m VenueModel) []rankedRow {
	if len(s.stores) == 0 {
		return nil
	}
	maxCount := 0
	for _, st := range s.stores {
		maxCount = max(maxCount, st.count)
	}
	transitions := s.transitions[s.previousStore]
	maxTransition := 1
	for _, n := range transitions {
		maxTransition = max(maxTransition, n)
	}
	rows := make([]rankedRow, 0, len(s.stores))
	maxRecency := 1e-9
	for _, key := range s.storeOrder {
		st := s.stores[key]
		r := rankedRow{
			key:          key,
			displayName:  st.displayName,
			count:        st.count,
			frequency:    float64(st.count) / float64(maxCount),
			recency:      recencyValue(st.occurrences, s.step, m),
			transition:   float64(transitions[key]) / float64(maxTransition),
			lastOrderAge: s.step - st.occurrences[len(st.occurrences)-1] - 1,
		}
		maxRecency = math.Max(maxRecency, r.recency)
		rows = append(rows, r)
	}
	for i := range rows {
		r := &rows[i]
		r.score = m.Frequency*r.frequency + m.Recency*(r.recency/maxRecency) + m.Transition*r.transition
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if a.count != b.count {
			return a.count > b.count
		}
		return a.key < b.key // keys are normalized [a-z0-9 ]: byte order equals localeCompare
	})
	return rows
}

// Venue is one ranked venue from the order history.
type Venue struct {
	Name         any     `json:"name"`
	OrderCount   int     `json:"order_count"`
	LastOrderAge int     `json:"last_order_age"`
	RawScore     float64 `json:"raw_score"`
	Score        float64 `json:"score"`
	Short        float64 `json:"short"`
	Medium       float64 `json:"medium"`
	Long         float64 `json:"long"`
}

// VenueHistory is the rankVenueHistory result. Keys lists Venues in rank order.
type VenueHistory struct {
	Cards  int
	Keys   []string
	Venues map[string]*Venue
}

// RankVenueHistory is rankVenueHistory with the promoted multi-scale model.
func RankVenueHistory(newestFirstCards []Object) (*VenueHistory, error) {
	return RankVenueHistoryWith(newestFirstCards, MultiscaleVenueModel)
}

// RankVenueHistoryWith is rankVenueHistory with an explicit model.
func RankVenueHistoryWith(newestFirstCards []Object, m VenueModel) (*VenueHistory, error) {
	orders, err := PrepareVenueOrders(newestFirstCards, true)
	if err != nil {
		return nil, err
	}
	s := newVenueState()
	for _, o := range orders {
		s.ingest(o)
	}
	h := &VenueHistory{Cards: len(orders), Venues: map[string]*Venue{}}
	for _, r := range s.rankedRows(m) {
		occ := s.stores[r.key].occurrences
		h.Keys = append(h.Keys, r.key)
		h.Venues[r.key] = &Venue{
			Name: r.displayName, OrderCount: r.count, LastOrderAge: r.lastOrderAge, RawScore: r.recency, Score: r.score,
			Short: summedDecay(occ, s.step, 5), Medium: summedDecay(occ, s.step, 20), Long: summedDecay(occ, s.step, 80),
		}
	}
	return h, nil
}

// VenueMetrics are evaluateVenueModel metrics.
type VenueMetrics struct {
	Events             int      `json:"events"`
	PrecisionAt5       float64  `json:"precision_at_5"`
	RecallAt5          float64  `json:"recall_at_5"`
	NDCGAt5            float64  `json:"ndcg_at_5"`
	NDCGAt5SE          *float64 `json:"ndcg_at_5_se"`
	RepeatRecallAt5    float64  `json:"repeat_recall_at_5"`
	NovelTargetShare   float64  `json:"novel_target_share"`
	CatalogCoverageAt5 float64  `json:"catalog_coverage_at_5"`
}

func standardError(values []float64) (float64, bool) {
	if len(values) < 2 {
		return 0, false
	}
	n := float64(len(values))
	mean := 0.0
	for _, v := range values {
		mean += v
	}
	mean /= n
	variance := 0.0
	for _, v := range values {
		variance += (v - mean) * (v - mean)
	}
	variance /= n - 1
	return math.Sqrt(variance / n), true
}

// EvaluateVenueModel is evaluateVenueModel: rolling-origin top-k scoring.
func EvaluateVenueModel(train, evaluation []VenueOrder, m VenueModel, topK int) VenueMetrics {
	s := newVenueState()
	for _, o := range train {
		s.ingest(o)
	}
	var observations []float64
	recommended := map[string]bool{}
	hits, repeatEvents, repeatHits, novelEvents := 0, 0, 0, 0
	for _, o := range evaluation {
		_, known := s.stores[o.Store]
		rows := s.rankedRows(m)
		if len(rows) > topK {
			rows = rows[:topK]
		}
		rank := -1
		for i, r := range rows {
			recommended[r.key] = true
			if r.key == o.Store && rank < 0 {
				rank = i
			}
		}
		hit := rank >= 0
		if hit {
			hits++
		}
		if !known {
			novelEvents++
		} else {
			repeatEvents++
			if hit {
				repeatHits++
			}
		}
		if hit {
			observations = append(observations, 1/math.Log2(float64(rank+2)))
		} else {
			observations = append(observations, 0)
		}
		s.ingest(o)
	}
	count := float64(max(1, len(evaluation)))
	sum := 0.0
	for _, v := range observations {
		sum += v
	}
	r4 := func(v float64) float64 { return toFixed(v, 4) }
	met := VenueMetrics{
		Events:             len(evaluation),
		PrecisionAt5:       r4(float64(hits) / count / float64(topK)),
		RecallAt5:          r4(float64(hits) / count),
		NDCGAt5:            r4(sum / count),
		RepeatRecallAt5:    r4(float64(repeatHits) / float64(max(1, repeatEvents))),
		NovelTargetShare:   r4(float64(novelEvents) / count),
		CatalogCoverageAt5: r4(float64(len(recommended)) / float64(max(1, len(s.stores)))),
	}
	if se, ok := standardError(observations); ok {
		v := r4(se)
		met.NDCGAt5SE = &v
	}
	return met
}

type validatedModel struct {
	model   VenueModel
	metrics VenueMetrics
}

func selectWithinOneStandardError(rows []validatedModel) validatedModel {
	byNDCG := append([]validatedModel(nil), rows...)
	sort.SliceStable(byNDCG, func(i, j int) bool { return byNDCG[i].metrics.NDCGAt5 > byNDCG[j].metrics.NDCGAt5 })
	best := byNDCG[0]
	se := 0.0
	if best.metrics.NDCGAt5SE != nil {
		se = *best.metrics.NDCGAt5SE
	}
	threshold := best.metrics.NDCGAt5 - se
	var within []validatedModel
	for i := range rows {
		if rows[i].metrics.NDCGAt5 >= threshold {
			within = append(within, rows[i])
		}
	}
	coll := collator()
	sort.SliceStable(within, func(i, j int) bool {
		a, b := within[i], within[j]
		if a.model.Complexity != b.model.Complexity {
			return a.model.Complexity < b.model.Complexity
		}
		if a.metrics.NDCGAt5 != b.metrics.NDCGAt5 {
			return a.metrics.NDCGAt5 > b.metrics.NDCGAt5
		}
		return coll.CompareString(a.model.Name, b.model.Name) < 0
	})
	return within[0]
}

// VenueEvaluation is the runVenueEvaluation result.
type VenueEvaluation struct {
	Dataset struct {
		CapturedOrders             int    `json:"captured_orders"`
		UsableVenueOrders          int    `json:"usable_venue_orders"`
		ExcludedCancelledOrInvalid int    `json:"excluded_cancelled_or_invalid"`
		UniqueVenues               int    `json:"unique_venues"`
		ProductModel               string `json:"product_model"`
	} `json:"dataset"`
	Split struct {
		Strategy                   string `json:"strategy"`
		TrainOrders                int    `json:"train_orders"`
		ValidationOrders           int    `json:"validation_orders"`
		FinalTestOrders            int    `json:"final_test_orders"`
		RollingUpdatesAfterScoring bool   `json:"rolling_updates_after_scoring"`
	} `json:"split"`
	Selection struct {
		Model                 VenueModel   `json:"model"`
		Validation            VenueMetrics `json:"validation"`
		Rule                  string       `json:"rule"`
		SelectionUsedTestData bool         `json:"selection_used_test_data"`
	} `json:"selection"`
	FinalTest struct {
		Popularity VenueMetrics `json:"popularity"`
		Selected   VenueMetrics `json:"selected"`
	} `json:"final_test"`
}

// RunVenueEvaluation is runVenueEvaluation: select a model on validation
// data only, then score it once on the untouched final test split.
func RunVenueEvaluation(cards []Object, newestFirst bool) (*VenueEvaluation, error) {
	orders, err := PrepareVenueOrders(cards, newestFirst)
	if err != nil {
		return nil, err
	}
	split, err := TemporalSplit(orders, 0.7, 0.15)
	if err != nil {
		return nil, err
	}
	candidates := modelCandidates()
	validation := make([]validatedModel, len(candidates))
	for i, m := range candidates {
		validation[i] = validatedModel{model: m, metrics: EvaluateVenueModel(split.Train, split.Validation, m, 5)}
	}
	selected := selectWithinOneStandardError(validation)
	trainPlusValidation := append(append([]VenueOrder{}, split.Train...), split.Validation...)
	unique := map[string]bool{}
	for _, o := range orders {
		unique[o.Store] = true
	}
	ev := &VenueEvaluation{}
	ev.Dataset.CapturedOrders = len(cards)
	ev.Dataset.UsableVenueOrders = len(orders)
	ev.Dataset.ExcludedCancelledOrInvalid = len(cards) - len(orders)
	ev.Dataset.UniqueVenues = len(unique)
	ev.Dataset.ProductModel = "not_evaluated_from_order_cards"
	ev.Split.Strategy = "70/15/15 chronological holdout"
	ev.Split.TrainOrders = len(split.Train)
	ev.Split.ValidationOrders = len(split.Validation)
	ev.Split.FinalTestOrders = len(split.Test)
	ev.Split.RollingUpdatesAfterScoring = true
	ev.Selection.Model = selected.model
	ev.Selection.Validation = selected.metrics
	ev.Selection.Rule = "simplest model within one standard error of best validation NDCG@5"
	ev.Selection.SelectionUsedTestData = false
	ev.FinalTest.Popularity = EvaluateVenueModel(trainPlusValidation, split.Test, candidates[0], 5)
	ev.FinalTest.Selected = EvaluateVenueModel(trainPlusValidation, split.Test, selected.model, 5)
	return ev, nil
}
