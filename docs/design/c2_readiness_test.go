// Package design checks the C2 readiness planning artifact. The matrix is
// documentation, not an authorization engine: nothing here is used at runtime
// and no test contacts GitHub or any provider. Evidence checks are offline
// syntax and state checks; no evidence URL is ever fetched.
package design

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// refTime is the fixed reference time for every test. Tests never read the clock.
var refTime = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

type freshness struct {
	ObservedAt *string `json:"observed_at"`
	State      string  `json:"state"`
}

type canary struct {
	DecisionID  string    `json:"decision_id"`
	Status      string    `json:"status"`
	EvidenceSHA string    `json:"evidence_sha"`
	EvidenceURL string    `json:"evidence_url"`
	Freshness   freshness `json:"freshness"`
	Note        string    `json:"note"`
}

type gate struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Kind            string    `json:"kind"`
	Phase           string    `json:"phase"`
	Status          string    `json:"status"`
	Role            string    `json:"role"`
	DecisionID      string    `json:"decision_id"`
	EvidenceID      string    `json:"evidence_id"`
	EvidenceSHA     string    `json:"evidence_sha"`
	EvidenceURL     string    `json:"evidence_url"`
	Rollback        string    `json:"rollback"`
	Freshness       freshness `json:"freshness"`
	AllowedSurfaces []string  `json:"allowed_surfaces"`
	WriteDependency bool      `json:"write_dependency"`
	DependsOn       []string  `json:"depends_on"`
	Requires        []string  `json:"requires"`
	Refs            []string  `json:"refs"`
	Prerequisites   []string  `json:"prerequisites"`
	Canary          *canary   `json:"canary_authorization"`
	Note            string    `json:"note"`
}

type topic struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Role        string    `json:"role"`
	Refs        []string  `json:"refs"`
	SourcePaths []string  `json:"source_paths"`
	EvidenceSHA string    `json:"evidence_sha"`
	EvidenceURL string    `json:"evidence_url"`
	Freshness   freshness `json:"freshness"`
	Rollback    string    `json:"rollback"`
	Earliest    string    `json:"earliest_gate"`
	CarriedTo   []string  `json:"carried_to"`
	Note        string    `json:"note"`
}

type stageEntry struct {
	Name   string   `json:"name"`
	Kind   string   `json:"kind"`
	MapsTo []string `json:"maps_to"`
	Note   string   `json:"note"`
}

type extDep struct {
	Ref    string `json:"ref"`
	Target string `json:"target"`
}

type epic struct {
	Epic       string   `json:"epic"`
	Issue      string   `json:"issue"`
	Target     string   `json:"target"`
	RealizedBy string   `json:"realized_by"`
	CarriedBy  string   `json:"carried_by"`
	DependsOn  []string `json:"depends_on"`
	External   []extDep `json:"external_depends_on"`
}

type planSource struct {
	Repo       string              `json:"repo"`
	PR         int                 `json:"pr"`
	PRState    string              `json:"pr_state"`
	Merged     bool                `json:"merged"`
	HeadSHA    string              `json:"head_sha"`
	Path       string              `json:"path"`
	URL        string              `json:"url"`
	Note       string              `json:"note"`
	Epics      []epic              `json:"epics"`
	Safeguards map[string][]string `json:"additional_offline_safeguards"`
	ScopeNote  string              `json:"scope_note"`
}

type decisionRef struct {
	ID       string `json:"id"`
	Required bool   `json:"required"`
	Stage    string `json:"stage"`
	Note     string `json:"note"`
}

type writeApproval struct {
	LedgerRefs            []string `json:"ledger_refs"`
	SecondLedger          bool     `json:"second_ledger"`
	CanaryRef             string   `json:"canary_ref"`
	CanarySeparateOwnerGo bool     `json:"canary_separate_owner_go"`
	ApprovalStatus        string   `json:"approval_status"`
	StatusNote            string   `json:"approval_status_note"`
}

type matrix struct {
	WriteApproval writeApproval `json:"write_approval"`
	SchemaVersion int           `json:"schema_version"`
	Kind          string        `json:"kind"`
	Issue         string        `json:"issue"`
	Issue40Done   bool          `json:"issue_40_fully_satisfied"`
	Baseline      struct {
		Repo        string   `json:"repo"`
		Branch      string   `json:"branch"`
		SHA         string   `json:"sha"`
		EvidenceURL string   `json:"evidence_url"`
		CodeNote    string   `json:"code_facts_note"`
		SourcePaths []string `json:"source_paths"`
		OpenDrafts  []struct {
			Number int    `json:"number"`
			Head   string `json:"head"`
			State  string `json:"state"`
			Merged bool   `json:"merged"`
			Source string `json:"source"`
		} `json:"open_draft_prs"`
		DraftsNote      string   `json:"open_draft_prs_note"`
		RuntimeUnknowns []string `json:"runtime_unknowns"`
	} `json:"baseline"`
	Freshness struct {
		OwnerApproved bool    `json:"owner_approved"`
		EffectiveTTL  *string `json:"effective_ttl"`
		Note          string  `json:"note"`
		Proposals     []struct {
			ID   string `json:"id"`
			Rule string `json:"rule"`
		} `json:"proposals"`
	} `json:"freshness"`
	Forbidden     []string `json:"forbidden_surfaces"`
	StageSequence struct {
		Source string       `json:"source"`
		Stages []stageEntry `json:"stages"`
	} `json:"stage_sequence"`
	PlanSource planSource `json:"epic_plan_source"`
	Authority  struct {
		Section94 struct {
			SourceOfTruth string `json:"source_of_truth"`
			URL           string `json:"url"`
			URLNote       string `json:"url_note"`
			Override      bool   `json:"override"`
			Unresolved    []int  `json:"unresolved_items"`
			Note          string `json:"note"`
		} `json:"section_9_4"`
		Issue11 struct {
			Owner  string `json:"owner"`
			Status string `json:"status"`
			Note   string `json:"note"`
		} `json:"issue_11_text_correction"`
	} `json:"authority"`
	DecisionRefs []decisionRef `json:"decision_refs"`
	Gates        []gate        `json:"gates"`
	Topics       []topic       `json:"topics"`
}

const (
	jsonPath = "c2-readiness.json"
	mdPath   = "c2-readiness.md"
)

func loadMatrix(t *testing.T) matrix {
	t.Helper()
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read %s: %v", jsonPath, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m matrix
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decode %s: %v", jsonPath, err)
	}
	return m
}

func (m matrix) gate(id string) *gate {
	for i := range m.Gates {
		if m.Gates[i].ID == id {
			return &m.Gates[i]
		}
	}
	return nil
}

func (m matrix) topic(id string) *topic {
	for i := range m.Topics {
		if m.Topics[i].ID == id {
			return &m.Topics[i]
		}
	}
	return nil
}

var sha40 = regexp.MustCompile(`^[0-9a-f]{40}$`)

// validHTTPS is an offline syntax check only: https scheme and a non-empty
// host. It never fetches the URL.
func validHTTPS(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Hostname() != ""
}

// verdict reports whether a gate may be treated as PASS at time now. A gate is
// PASS only when it claims PASS and its evidence is present, observable, in a
// FRESH state and not aged. maxAge is a synthetic proposal supplied by the
// caller; the real matrix has no approved TTL, so with a nil maxAge nothing
// can pass.
func verdict(g gate, now time.Time, maxAge *time.Duration) (string, string) {
	if g.Status != "PASS" {
		if g.Status == "FAIL" || g.Status == "BLOCKED" {
			return g.Status, "claimed " + g.Status
		}
		return "UNKNOWN", "status is not PASS"
	}
	if !sha40.MatchString(g.EvidenceSHA) {
		return "UNKNOWN", "missing or non-40-hex evidence sha"
	}
	if !validHTTPS(g.EvidenceURL) {
		return "UNKNOWN", "missing or invalid evidence url"
	}
	if g.Freshness.State != "FRESH" {
		return "UNKNOWN", "freshness state is not FRESH"
	}
	if g.Freshness.ObservedAt == nil {
		return "UNKNOWN", "missing observed_at"
	}
	obs, err := time.Parse(time.RFC3339, *g.Freshness.ObservedAt)
	if err != nil {
		return "UNKNOWN", "unparseable observed_at"
	}
	if maxAge == nil {
		return "UNKNOWN", "no approved evidence ttl"
	}
	age := now.Sub(obs)
	if age < 0 {
		return "UNKNOWN", "evidence observed in the future"
	}
	if age > *maxAge {
		return "UNKNOWN", "evidence stale"
	}
	return "PASS", "ok"
}

// topicVerdict applies the same evidence rules as verdict to a topic. Gate
// evidence and topic evidence differ in scope: a gate verdict also needs every
// dependency to pass (see authorizationErrors), while a topic is a leaf whose
// own evidence is all that is checked.
func topicVerdict(tp topic, now time.Time, maxAge *time.Duration) (string, string) {
	return verdict(gate{
		ID: tp.ID, Status: tp.Status, EvidenceSHA: tp.EvidenceSHA,
		EvidenceURL: tp.EvidenceURL, Freshness: tp.Freshness,
	}, now, maxAge)
}

var gateIDPattern = regexp.MustCompile(`^G[0-9]+$`)

// authCheck walks the dependencies of one gate. TEST-ONLY MODEL: it exists to
// exercise the fail-closed rules on synthetic fixtures and is not operational
// authority. Nothing at runtime reads it.
type authCheck struct {
	m        matrix
	now      time.Time
	maxAge   *time.Duration
	visiting map[string]bool
	passed   map[string]bool
	errs     []string
}

func (c *authCheck) fail(format string, a ...any) {
	c.errs = append(c.errs, fmt.Sprintf(format, a...))
}

// gate requires the gate's own PASS plus PASS for every prerequisite, every
// depends_on and requires reference and, when present, its canary. Gate
// references recurse. A missing gate, a non-PASS verdict or a cycle fails.
func (c *authCheck) gate(id string) bool {
	if c.passed[id] {
		return true
	}
	if c.visiting[id] {
		c.fail("dependency cycle through gate %s", id)
		return false
	}
	g := c.m.gate(id)
	if g == nil {
		c.fail("gate %s missing", id)
		return false
	}
	c.visiting[id] = true
	defer delete(c.visiting, id)
	ok := true
	if v, why := verdict(*g, c.now, c.maxAge); v != "PASS" {
		c.fail("gate %s is %s: %s", id, v, why)
		ok = false
	}
	var refs []string
	if parent, has := fixedChain[id]; has {
		refs = append(refs, parent)
	}
	refs = append(refs, fixedRequired[id]...)
	for _, set := range [][]string{g.Prerequisites, g.DependsOn, g.Requires} {
		refs = append(refs, set...)
	}
	seen := map[string]bool{}
	for _, ref := range refs {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		if !c.ref(id, ref) {
			ok = false
		}
	}
	switch {
	case g.Canary != nil:
		if g.Canary.DecisionID == "" {
			c.fail("gate %s canary decision id is empty", id)
			ok = false
		}
		cg := gate{ID: id + " canary", Status: g.Canary.Status, EvidenceSHA: g.Canary.EvidenceSHA,
			EvidenceURL: g.Canary.EvidenceURL, Freshness: g.Canary.Freshness}
		if v, why := verdict(cg, c.now, c.maxAge); v != "PASS" {
			c.fail("gate %s canary is %s: %s", id, v, why)
			ok = false
		}
	case id == "G4":
		c.fail("gate G4 canary missing")
		ok = false
	}
	if ok {
		c.passed[id] = true
	}
	return ok
}

// ref resolves one reference. A gate id recurses. Anything else is resolved
// explicitly to topics: topics whose id equals the reference, otherwise topics
// listing it among their refs. All resolved topics must PASS on their own
// evidence. A reference that resolves to nothing is unobserved and refused.
func (c *authCheck) ref(owner, ref string) bool {
	if gateIDPattern.MatchString(ref) {
		return c.gate(ref)
	}
	// The union of topics whose id equals the reference and topics listing it
	// among their refs. A PASS topic must not shadow a non-PASS one.
	var found []topic
	for i := range c.m.Topics {
		tp := c.m.Topics[i]
		if tp.ID == ref || (ref != "" && ref != "UNKNOWN" && contains(tp.Refs, ref)) {
			found = append(found, tp)
		}
	}
	if len(found) == 0 {
		c.fail("%s: reference %q is unresolved (unobserved)", owner, ref)
		return false
	}
	ok := true
	for i := range found {
		if v, why := topicVerdict(found[i], c.now, c.maxAge); v != "PASS" {
			c.fail("%s: topic %s (for %q) is %s: %s", owner, found[i].ID, ref, v, why)
			ok = false
		}
	}
	return ok
}

// authorizationErrors lists every reason the stage is not authorized. A stage
// is authorized only when its own gate and its whole dependency closure PASS.
// A PASS at another stage never grants it.
func authorizationErrors(m matrix, id string, now time.Time, maxAge *time.Duration) []string {
	c := &authCheck{m: m, now: now, maxAge: maxAge, visiting: map[string]bool{}, passed: map[string]bool{}}
	c.errs = append(c.errs, modelIntegrityErrors(m)...)
	c.gate(id)
	return c.errs
}

// modelIntegrityErrors refuses ambiguous input: empty or duplicate gate and
// topic ids make every verdict unreliable, so they fail every stage.
func modelIntegrityErrors(m matrix) []string {
	var errs []string
	seenG, seenT := map[string]bool{}, map[string]bool{}
	for i := range m.Gates {
		id := m.Gates[i].ID
		switch {
		case id == "":
			errs = append(errs, "empty gate id")
		case seenG[id]:
			errs = append(errs, "duplicate gate "+id)
		}
		seenG[id] = true
	}
	for i := range m.Topics {
		id := m.Topics[i].ID
		switch {
		case id == "":
			errs = append(errs, "empty topic id")
		case seenT[id]:
			errs = append(errs, "duplicate topic "+id)
		}
		seenT[id] = true
	}
	return errs
}

// fixedChain and fixedRequired are the structure of the model itself. They are
// enforced even when the input drops the corresponding depends_on or requires
// entries, so deleting a link cannot weaken a stage.
var fixedChain = map[string]string{"G1": "G0", "G2": "G1", "G3": "G2", "G4": "G3"}

var fixedRequired = map[string][]string{
	"G3": {"#44", "#45"},
	"G4": {"#47", "#44", "#55", "#57", "mctlhq/mctl-api#1006", "mctlhq/mctl-api#1011", "mctlhq/mctl-api#1012"},
}

func authorized(m matrix, id string, now time.Time, maxAge *time.Duration) bool {
	return len(authorizationErrors(m, id, now, maxAge)) == 0
}

func strp(s string) *string { return &s }

func goodGate(id, decision string) gate {
	return gate{
		ID: id, Status: "PASS", DecisionID: decision,
		EvidenceSHA: strings.Repeat("a", 40),
		EvidenceURL: "https://example.invalid/evidence/" + id,
		Freshness:   freshness{ObservedAt: strp("2026-11-18T11:00:00Z"), State: "FRESH"},
	}
}

func dur(h int) *time.Duration { d := time.Duration(h) * time.Hour; return &d }

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestVerdictPositiveControl(t *testing.T) {
	g := goodGate("G1", "D1")
	if v, why := verdict(g, refTime, dur(24)); v != "PASS" {
		t.Fatalf("good fixture = %s (%s), want PASS", v, why)
	}
	// Exactly at the age limit is still fresh; one second past it is stale.
	g.Freshness.ObservedAt = strp("2026-11-17T12:00:00Z")
	if v, why := verdict(g, refTime, dur(24)); v != "PASS" {
		t.Fatalf("boundary fixture = %s (%s), want PASS", v, why)
	}
}

func TestMissingEvidenceRefused(t *testing.T) {
	cases := map[string]func(*gate){
		"empty sha":          func(g *gate) { g.EvidenceSHA = "" },
		"UNKNOWN sha":        func(g *gate) { g.EvidenceSHA = "UNKNOWN" },
		"short sha":          func(g *gate) { g.EvidenceSHA = "abc123" },
		"empty url":          func(g *gate) { g.EvidenceURL = "" },
		"UNKNOWN url":        func(g *gate) { g.EvidenceURL = "UNKNOWN" },
		"http url":           func(g *gate) { g.EvidenceURL = "http://example.invalid/x" },
		"https without host": func(g *gate) { g.EvidenceURL = "https://" },
		"https empty host":   func(g *gate) { g.EvidenceURL = "https:///path" },
		"no observed_at":     func(g *gate) { g.Freshness.ObservedAt = nil },
		"bad observed_at":    func(g *gate) { g.Freshness.ObservedAt = strp("yesterday") },
		"status UNKNOWN":     func(g *gate) { g.Status = "UNKNOWN" },
		"status empty":       func(g *gate) { g.Status = "" },
		"status ambiguous":   func(g *gate) { g.Status = "MAYBE" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			g := goodGate("G1", "D1")
			mutate(&g)
			if v, _ := verdict(g, refTime, dur(24)); v == "PASS" {
				t.Fatalf("%s produced PASS", name)
			}
		})
	}
	t.Run("no approved ttl", func(t *testing.T) {
		if v, _ := verdict(goodGate("G1", "D1"), refTime, nil); v == "PASS" {
			t.Fatal("PASS without an approved ttl")
		}
	})
}

func TestUnknownFreshnessStateRefused(t *testing.T) {
	for _, st := range []string{"UNKNOWN", "", "UNOBSERVABLE", "AMBIGUOUS", "STALE"} {
		t.Run("state "+st, func(t *testing.T) {
			g := goodGate("G1", "D1") // PASS status and otherwise valid evidence
			g.Freshness.State = st
			if v, _ := verdict(g, refTime, dur(24)); v == "PASS" {
				t.Fatalf("freshness state %q with valid evidence produced PASS", st)
			}
		})
	}
}

func TestStaleEvidenceRefused(t *testing.T) {
	cases := map[string]string{
		"one second past limit": "2026-11-17T11:59:59Z",
		"long ago":              "2025-01-01T00:00:00Z",
		"future":                "2026-11-18T12:00:01Z",
	}
	for name, obs := range cases {
		t.Run(name, func(t *testing.T) {
			g := goodGate("G1", "D1")
			g.Freshness.ObservedAt = strp(obs)
			if v, _ := verdict(g, refTime, dur(24)); v == "PASS" {
				t.Fatalf("%s produced PASS", name)
			}
		})
	}
}

func TestStageAuthorizationsDistinctFixture(t *testing.T) {
	m := matrix{Gates: []gate{
		goodGate("G0", "D0"), goodGate("G1", "D1"), goodGate("G2", "D2"),
	}}
	g3 := goodGate("G3", "D3")
	g3.Status = "UNKNOWN"
	m.Gates = append(m.Gates, g3)
	for _, id := range []string{"G0", "G1", "G2"} {
		if !authorized(m, id, refTime, dur(24)) {
			t.Fatalf("%s should be authorized by its own PASS", id)
		}
	}
	if authorized(m, "G3", refTime, dur(24)) {
		t.Fatal("G3 authorized by the PASS of earlier stages")
	}
	if authorized(m, "G4", refTime, dur(24)) {
		t.Fatal("absent G4 authorized")
	}
}

func TestStageAuthorizationsDistinct(t *testing.T) {
	m := loadMatrix(t)
	want := []string{"G0", "G1", "G2", "G3", "G4"}
	if len(m.Gates) != len(want) {
		t.Fatalf("got %d gates, want %d", len(m.Gates), len(want))
	}
	seen := map[string]string{}
	add := func(owner, id string) {
		if id == "" {
			t.Errorf("%s has an empty decision id", owner)
			return
		}
		if prev, dup := seen[id]; dup {
			t.Errorf("decision id %q shared by %s and %s", id, prev, owner)
		}
		seen[id] = owner
	}
	for i, id := range want {
		g := &m.Gates[i]
		if g.ID != id {
			t.Fatalf("gate %d is %s, want %s", i, g.ID, id)
		}
		switch g.Kind {
		case "decision":
			if g.EvidenceID != "" {
				t.Errorf("decision gate %s carries an evidence id", g.ID)
			}
			add(g.ID, g.DecisionID)
		case "result":
			// A result is evidence, not a decision: it has an evidence id and
			// no decision id.
			if g.DecisionID != "" {
				t.Errorf("result gate %s carries a decision id %q", g.ID, g.DecisionID)
			}
			add(g.ID+" evidence", g.EvidenceID)
		default:
			t.Errorf("gate %s has kind %q", g.ID, g.Kind)
		}
	}
	g4 := m.gate("G4")
	if g4.Canary == nil {
		t.Fatal("G4 lacks a separate canary authorization")
	}
	add("G4 canary", g4.Canary.DecisionID)
	if g4.Canary.DecisionID == g4.DecisionID {
		t.Error("G4 canary shares the G4 decision id")
	}
	// G0 is the owner pre-GO decision; G1 is its later results, not a second decision.
	if m.gate("G0").Kind != "decision" || m.gate("G1").Kind != "result" {
		t.Error("G0 must be a decision and G1 its results")
	}
	for _, id := range []string{"G2", "G3", "G4"} {
		if m.gate(id).Kind != "decision" {
			t.Errorf("%s must be a decision", id)
		}
	}
	// Chain: each stage depends on its predecessor.
	for i := 1; i < len(want); i++ {
		if !contains(m.gate(want[i]).DependsOn, want[i-1]) {
			t.Errorf("%s must depend on %s", want[i], want[i-1])
		}
	}
}

func TestStageSequenceRecorded(t *testing.T) {
	m := loadMatrix(t)
	if m.StageSequence.Source != "operator-recorded requirement" {
		t.Errorf("stage sequence source = %q", m.StageSequence.Source)
	}
	want := []string{"Owner-only", "10 read-only", "Approval+Canary", "20 read+write"}
	var got []string
	for i := range m.StageSequence.Stages {
		got = append(got, m.StageSequence.Stages[i].Name)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("stage sequence = %v, want %v", got, want)
	}
	if errs := stageMappingErrors(m); len(errs) != 0 {
		t.Errorf("stage mapping: %v", errs)
	}
	// Negative control: mapping Approval+Canary to a gate is refused.
	bad := m
	bad.StageSequence.Stages = append([]stageEntry(nil), m.StageSequence.Stages...)
	bad.StageSequence.Stages[2].MapsTo = []string{"G4"}
	if len(stageMappingErrors(bad)) == 0 {
		t.Error("Approval+Canary mapped to a gate was not refused")
	}
}

// stageMappingErrors checks that every recorded stage maps to real gate or
// dependency ids, and that Approval+Canary stays a distinct dependency phase
// (#55, #57 as G4 dependencies) rather than a gate or an operational GO.
func stageMappingErrors(m matrix) []string {
	var errs []string
	g4 := m.gate("G4")
	last := -1
	for i := range m.StageSequence.Stages {
		s := &m.StageSequence.Stages[i]
		if len(s.MapsTo) == 0 {
			errs = append(errs, s.Name+" maps to nothing")
		}
		for _, id := range s.MapsTo {
			switch s.Kind {
			case "gates":
				idx := -1
				for j := range m.Gates {
					if m.Gates[j].ID == id {
						idx = j
					}
				}
				switch {
				case idx < 0:
					errs = append(errs, s.Name+" maps to unknown gate "+id)
				case idx <= last:
					errs = append(errs, s.Name+" gate "+id+" is out of order")
				default:
					last = idx
				}
			case "dependency-phase":
				if m.gate(id) != nil {
					errs = append(errs, s.Name+" is a dependency phase but maps to gate "+id)
				}
				if g4 == nil || !contains(g4.DependsOn, id) {
					errs = append(errs, s.Name+" dependency "+id+" is not a G4 dependency")
				}
			default:
				errs = append(errs, s.Name+" has kind "+s.Kind)
			}
		}
	}
	return append(errs, stageGoldenErrors(m)...)
}

type goldenStage struct {
	name, kind string
	maps       []string
}

// goldenStages pins the complete per-stage maps independently of the JSON.
var goldenStages = []goldenStage{
	{"Owner-only", "gates", []string{"G0", "G1"}},
	{"10 read-only", "gates", []string{"G2", "G3"}},
	{"Approval+Canary", "dependency-phase", []string{"#55", "#57"}},
	{"20 read+write", "gates", []string{"G4"}},
}

// stageGoldenErrors requires the exact recorded stage maps, coverage of G0
// through G4 by the gate stages, and Approval+Canary to be exactly {#55,#57}.
func stageGoldenErrors(m matrix) []string {
	var errs []string
	st := m.StageSequence.Stages
	if len(st) != len(goldenStages) {
		errs = append(errs, fmt.Sprintf("stage map count %d, golden %d", len(st), len(goldenStages)))
	}
	covered := map[string]bool{}
	for i := range st {
		if st[i].Kind == "gates" {
			for _, id := range st[i].MapsTo {
				covered[id] = true
			}
		}
		if i >= len(goldenStages) {
			continue
		}
		w := goldenStages[i]
		if st[i].Name != w.name || st[i].Kind != w.kind || !reflect.DeepEqual(st[i].MapsTo, w.maps) {
			errs = append(errs, fmt.Sprintf("stage %q map %v differs from golden %q %v", st[i].Name, st[i].MapsTo, w.name, w.maps))
		}
		if st[i].Name == "Approval+Canary" && !sameSet(st[i].MapsTo, []string{"#55", "#57"}) {
			errs = append(errs, "Approval+Canary must map exactly #55 and #57")
		}
	}
	for _, id := range []string{"G0", "G1", "G2", "G3", "G4"} {
		if !covered[id] {
			errs = append(errs, "stage maps do not cover "+id)
		}
	}
	return errs
}

func TestPreGOPrerequisites(t *testing.T) {
	m := loadMatrix(t)
	g0 := m.gate("G0")
	want := []string{
		"owner-decision-49", "disabled-write-proof-41", "linking-42", "cutover-2777",
		"operations-46", "deploy-false", "vault-control-identity", "proxy",
		"migrations", "image-pins", "rollback-kill", "oauth", "scopes",
		"glovo-home-writes", "consent-versions", "isolation", "capacity",
	}
	for _, id := range want {
		if !contains(g0.Prerequisites, id) {
			t.Errorf("G0 lacks prerequisite %s", id)
		}
		tp := m.topic(id)
		if tp == nil {
			t.Errorf("prerequisite topic %s missing", id)
			continue
		}
		if tp.Earliest != "G0" {
			t.Errorf("prerequisite %s earliest gate = %s, want G0", id, tp.Earliest)
		}
	}
	// Named issue refs sit at G0.
	refs := map[string]string{
		"owner-decision-49": "#49", "disabled-write-proof-41": "#41", "linking-42": "#42",
		"cutover-2777": "mctl-gitops#2777", "operations-46": "#46",
	}
	for id, ref := range refs {
		if tp := m.topic(id); tp == nil || !contains(tp.Refs, ref) {
			t.Errorf("topic %s must reference %s", id, ref)
		}
	}
	// No live-results cycle: G0 never needs G1 or #43.
	if len(g0.DependsOn) != 0 {
		t.Errorf("G0 depends on %v, want nothing", g0.DependsOn)
	}
	for _, p := range append(append([]string{}, g0.Prerequisites...), g0.DependsOn...) {
		if p == "G1" || p == "#43" {
			t.Errorf("G0 requires live results %s", p)
		}
	}
	if contains(g0.Refs, "#43") || contains(g0.Requires, "#43") {
		t.Error("G0 references #43 live results")
	}
	g1 := m.gate("G1")
	if !contains(g1.DependsOn, "G0") || !contains(g1.Refs, "#43") {
		t.Error("G1 must depend on G0 and hold the #43 results")
	}
	// Every topic with earliest G0 must be a G0 prerequisite.
	for i := range m.Topics {
		tp := &m.Topics[i]
		if tp.Earliest == "G0" && !contains(g0.Prerequisites, tp.ID) {
			t.Errorf("topic %s has earliest gate G0 but is not a G0 prerequisite", tp.ID)
		}
	}
}

var laterGates = []string{"G1", "G2", "G3", "G4"}

// oneShotCarry lists the only G0 topics that are not carried to every later
// gate, each with its exact expected carry. Any other G0 topic must be carried
// to all of G1 to G4. Reasons: the owner decision #49 is itself the G0
// decision; the cutover plan and the deploy:false state are one-shot
// preconditions of the first live E2E (G1); the epic externals are G0
// prerequisites of the pinned plan epic and carry nothing further.
var oneShotCarry = map[string][]string{
	"owner-decision-49": {},
	"cutover-2777":      {"G1"},
	"deploy-false":      {"G1"},
	"ext-mctl-api-1108": {},
	"ext-mctl-api-1110": {},
	"ext-mctl-api-1111": {},
	"ext-mctl-api-1079": {},
	"ext-gitops-2742":   {},
	"ext-gitops-2766":   {},
	"ext-gitops-2709":   {},
}

// orderingErrors reports prerequisite and evidence ordering violations: a G0
// prerequisite postponed beyond G0, a G0 topic missing from the G0
// prerequisites, a readiness topic not carried to every later phase, and live
// results (G1, #43) leaking before or into G0.
func orderingErrors(m matrix) []string {
	var errs []string
	g0, g1 := m.gate("G0"), m.gate("G1")
	if g0 == nil || g1 == nil {
		return []string{"G0 or G1 missing"}
	}
	for _, id := range g0.Prerequisites {
		if tp := m.topic(id); tp == nil || tp.Earliest != "G0" {
			errs = append(errs, "prerequisite "+id+" postponed beyond G0")
		}
	}
	for i := range m.Topics {
		tp := &m.Topics[i]
		if tp.Earliest == "G0" && !contains(g0.Prerequisites, tp.ID) {
			errs = append(errs, "topic "+tp.ID+" at G0 is not a G0 prerequisite")
		}
	}
	for _, id := range []string{"oauth", "scopes", "glovo-home-writes", "consent-versions", "isolation", "capacity"} {
		tp := m.topic(id)
		if tp == nil {
			errs = append(errs, "readiness topic "+id+" missing")
			continue
		}
		if tp.Earliest != "G0" || !contains(g0.Prerequisites, id) {
			errs = append(errs, "readiness topic "+id+" is not a G0 prerequisite")
		}
		for _, c := range []string{"G1", "G2", "G3", "G4"} {
			if !contains(tp.CarriedTo, c) {
				errs = append(errs, "readiness topic "+id+" not carried to "+c)
			}
		}
	}
	for i := range m.Topics {
		tp := &m.Topics[i]
		if tp.Earliest != "G0" {
			continue
		}
		want, exception := oneShotCarry[tp.ID]
		if !exception {
			want = laterGates
		}
		if !sameSet(tp.CarriedTo, want) {
			errs = append(errs, fmt.Sprintf("G0 topic %s carried to %v, want exactly %v", tp.ID, tp.CarriedTo, want))
		}
	}
	if len(g0.DependsOn) != 0 || contains(g0.Prerequisites, "G1") || contains(g0.Refs, "#43") || contains(g0.Requires, "#43") {
		errs = append(errs, "live results required before G0")
	}
	if !contains(g1.DependsOn, "G0") || !contains(g1.Refs, "#43") {
		errs = append(errs, "G1 must follow G0 and hold the #43 live results")
	}
	return errs
}

func TestPrerequisitePostponementRefused(t *testing.T) {
	m := loadMatrix(t)
	for _, e := range orderingErrors(m) {
		t.Errorf("ordering: %s", e)
	}
	// Negative control on an in-memory copy: postponing consent-versions to G2
	// and dropping its G0 prerequisite must be refused by name.
	bad := m
	bad.Topics = append([]topic(nil), m.Topics...)
	bad.Gates = append([]gate(nil), m.Gates...)
	for i := range bad.Topics {
		if bad.Topics[i].ID == "consent-versions" {
			bad.Topics[i].Earliest = "G2"
		}
	}
	for i := range bad.Gates {
		if bad.Gates[i].ID == "G0" {
			var keep []string
			for _, p := range bad.Gates[i].Prerequisites {
				if p != "consent-versions" {
					keep = append(keep, p)
				}
			}
			bad.Gates[i].Prerequisites = keep
		}
	}
	if len(orderingErrors(bad)) == 0 {
		t.Fatal("postponing consent-versions beyond G0 was not refused")
	}
}

func TestHostedProofPointerAndUnknownPR54(t *testing.T) {
	m := loadMatrix(t)
	tp := m.topic("disabled-write-proof-41")
	if tp == nil || !contains(tp.SourcePaths, "cmd/mctl-glovo/mcp.go") || !contains(tp.SourcePaths, "internal/glovotools/hosted.go") {
		t.Error("disabled-write-proof-41 must point at the hosted plane")
	}
	if tp != nil && contains(tp.SourcePaths, "cmd/glovo-home/main.go") {
		t.Error("cmd/glovo-home belongs to the legacy glovo-home inventory only")
	}
	if gh := m.topic("glovo-home-writes"); gh == nil || !contains(gh.SourcePaths, "cmd/glovo-home/main.go") {
		t.Error("glovo-home-writes must keep the legacy glovo-home pointer")
	}
	if !strings.Contains(m.Baseline.DraftsNote, "does not prove PR #54 behavior") || !strings.Contains(m.Baseline.DraftsNote, "UNKNOWN") {
		t.Error("PR #54 behavior must be stated UNKNOWN")
	}
}

func TestDecisionRefsStaySeparate(t *testing.T) {
	m := loadMatrix(t)
	byID := map[string]decisionRef{}
	for _, r := range m.DecisionRefs {
		if _, dup := byID[r.ID]; dup {
			t.Errorf("duplicate decision ref %s", r.ID)
		}
		byID[r.ID] = r
	}
	want := map[string]struct {
		req   bool
		stage string
	}{"#50": {true, "G2"}, "#51": {true, "G3"}, "#52": {false, "G4"}}
	for id, w := range want {
		r, ok := byID[id]
		if !ok {
			t.Errorf("%s missing", id)
			continue
		}
		if r.Required != w.req || r.Stage != w.stage {
			t.Errorf("%s = required %v stage %s, want required %v stage %s", id, r.Required, r.Stage, w.req, w.stage)
		}
		g := m.gate(w.stage)
		if g == nil || !contains(g.Refs, id) {
			t.Errorf("gate %s must carry ref %s", w.stage, id)
		}
	}
	if len(byID) != len(want) {
		t.Errorf("got %d decision refs, want %d", len(byID), len(want))
	}
}

func TestReadOnlyPhasesHaveNoWriteDependency(t *testing.T) {
	m := loadMatrix(t)
	writeOnly := []string{"#55", "#57", "mctlhq/mctl-api#1006", "mctlhq/mctl-api#1011", "mctlhq/mctl-api#1012"}
	for _, id := range []string{"G0", "G1", "G2", "G3"} {
		g := m.gate(id)
		if g.WriteDependency {
			t.Errorf("%s is read-only but declares a write dependency", id)
		}
		for _, dep := range writeOnly {
			if contains(g.DependsOn, dep) || contains(g.Requires, dep) || contains(g.Prerequisites, dep) || contains(g.Refs, dep) {
				t.Errorf("%s depends on write-stage item %s", id, dep)
			}
		}
		if contains(g.AllowedSurfaces, "basket-write") {
			t.Errorf("%s allows basket-write", id)
		}
	}
	// Topics carried into read-only phases must not smuggle in write-stage items.
	for i := range m.Topics {
		tp := &m.Topics[i]
		for _, dep := range writeOnly {
			if contains(tp.Refs, dep) {
				t.Errorf("topic %s references %s", tp.ID, dep)
			}
		}
	}
	g4 := m.gate("G4")
	if !g4.WriteDependency || !contains(g4.AllowedSurfaces, "basket-write") {
		t.Error("G4 must be the write stage")
	}
	for _, dep := range []string{"#55", "#57"} {
		if !contains(g4.DependsOn, dep) {
			t.Errorf("G4 must depend on %s", dep)
		}
	}
	for _, req := range []string{"mctlhq/mctl-api#1006", "mctlhq/mctl-api#1011", "mctlhq/mctl-api#1012"} {
		if !contains(g4.Requires, req) {
			t.Errorf("G4 must require %s", req)
		}
	}
	if g4.Canary == nil || g4.Canary.DecisionID == "" {
		t.Error("G4 needs a separate canary authorization")
	}
}

// forbiddenSurfaces is the canonical forbidden list. place_order is explicit.
var forbiddenSurfaces = []string{"order", "checkout", "payment", "place_order"}

// allowedSurfaces is the explicit per-gate allowlist: G0 has no operational
// surface, G1 to G3 are read only and G4 adds basket-write. Nothing else.
var allowedSurfaces = map[string][]string{
	"G0": {},
	"G1": {"read"},
	"G2": {"read"},
	"G3": {"read"},
	"G4": {"read", "basket-write"},
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]bool{}
	for _, x := range a {
		seen[x] = true
	}
	if len(seen) != len(a) {
		return false
	}
	for _, x := range b {
		if !seen[x] {
			return false
		}
	}
	return true
}

// surfaceErrors reports every deviation from the explicit allowlists and any
// forbidden surface that is missing, allowed or implied.
func surfaceErrors(m matrix) []string {
	var errs []string
	for _, s := range forbiddenSurfaces {
		if !contains(m.Forbidden, s) {
			errs = append(errs, s+" missing from forbidden_surfaces")
		}
	}
	for i := range m.Gates {
		g := &m.Gates[i]
		want, ok := allowedSurfaces[g.ID]
		if !ok {
			errs = append(errs, g.ID+" has no explicit surface allowlist")
			continue
		}
		if !sameSet(g.AllowedSurfaces, want) {
			errs = append(errs, fmt.Sprintf("%s allowed surfaces %v, want exactly %v", g.ID, g.AllowedSurfaces, want))
		}
		for _, s := range g.AllowedSurfaces {
			if contains(m.Forbidden, s) || contains(forbiddenSurfaces, s) {
				errs = append(errs, g.ID+" allows forbidden surface "+s)
			}
		}
	}
	return errs
}

// canaryErrors reports canary authorization anywhere but G4, and a missing or
// empty one on G4.
func canaryErrors(m matrix) []string {
	var errs []string
	for i := range m.Gates {
		g := &m.Gates[i]
		switch {
		case g.ID == "G4" && (g.Canary == nil || g.Canary.DecisionID == ""):
			errs = append(errs, "G4 lacks its own canary authorization")
		case g.ID != "G4" && g.Canary != nil:
			errs = append(errs, g.ID+" carries a canary authorization; only G4 may")
		}
	}
	return errs
}

func TestForbiddenSurfaces(t *testing.T) {
	m := loadMatrix(t)
	for _, e := range surfaceErrors(m) {
		t.Error(e)
	}
}

func TestSurfaceAllowlistNegativeControls(t *testing.T) {
	m := loadMatrix(t)
	with := func(id string, surfaces []string) matrix {
		bad := m
		bad.Gates = append([]gate(nil), m.Gates...)
		for i := range bad.Gates {
			if bad.Gates[i].ID == id {
				bad.Gates[i].AllowedSurfaces = surfaces
			}
		}
		return bad
	}
	cases := map[string]matrix{
		"G4 place_order":       with("G4", []string{"read", "basket-write", "place_order"}),
		"G4 checkout":          with("G4", []string{"read", "basket-write", "checkout"}),
		"G4 arbitrary surface": with("G4", []string{"read", "basket-write", "admin"}),
		"G3 basket-write":      with("G3", []string{"read", "basket-write"}),
		"G2 order":             with("G2", []string{"read", "order"}),
		"G1 empty":             with("G1", nil),
		"G0 read":              with("G0", []string{"read"}),
	}
	for name, bad := range cases {
		if len(surfaceErrors(bad)) == 0 {
			t.Errorf("%s was not refused", name)
		}
	}
	dropped := m
	dropped.Forbidden = []string{"order", "checkout", "payment"}
	if len(surfaceErrors(dropped)) == 0 {
		t.Error("place_order dropped from forbidden_surfaces was not refused")
	}
}

func TestCanaryOnlyOnG4(t *testing.T) {
	m := loadMatrix(t)
	for _, e := range canaryErrors(m) {
		t.Error(e)
	}
	for _, id := range []string{"G0", "G1", "G2", "G3"} {
		if g := m.gate(id); g.Canary != nil {
			t.Errorf("read-only stage %s carries a canary authorization", id)
		}
	}
	for _, id := range []string{"G0", "G1", "G2", "G3"} {
		bad := m
		bad.Gates = append([]gate(nil), m.Gates...)
		for i := range bad.Gates {
			if bad.Gates[i].ID == id {
				bad.Gates[i].Canary = &canary{DecisionID: "D-synthetic-canary", Status: "UNKNOWN"}
			}
		}
		if len(canaryErrors(bad)) == 0 {
			t.Errorf("canary on %s was not refused", id)
		}
	}
	noCanary := m
	noCanary.Gates = append([]gate(nil), m.Gates...)
	for i := range noCanary.Gates {
		if noCanary.Gates[i].ID == "G4" {
			noCanary.Gates[i].Canary = nil
		}
	}
	if len(canaryErrors(noCanary)) == 0 {
		t.Error("G4 without a canary was not refused")
	}
}

func TestRealMatrixNeverPasses(t *testing.T) {
	m := loadMatrix(t)
	if m.Issue40Done {
		t.Error("issue 40 must not be marked satisfied by this artifact")
	}
	if m.Freshness.OwnerApproved || m.Freshness.EffectiveTTL != nil {
		t.Error("no evidence TTL is owner-approved; do not invent one")
	}
	if len(m.Freshness.Proposals) == 0 {
		t.Error("freshness proposals missing")
	}
	for i := range m.Gates {
		g := &m.Gates[i]
		if g.Status == "PASS" {
			t.Errorf("%s is PASS in the planning artifact", g.ID)
		}
		// Even with a generous synthetic ttl the committed state must not pass.
		if v, _ := verdict(*g, refTime, dur(24*365)); v == "PASS" {
			t.Errorf("%s evaluates to PASS", g.ID)
		}
		if g.EvidenceSHA != "UNKNOWN" || g.EvidenceURL != "UNKNOWN" || g.Freshness.State != "UNKNOWN" {
			t.Errorf("%s has live evidence values that are not labelled UNKNOWN", g.ID)
		}
	}
	if m.Gates[4].Canary.Status == "PASS" {
		t.Error("canary must not be PASS")
	}
}

func TestSourcePinsAndAuthority(t *testing.T) {
	m := loadMatrix(t)
	if !sha40.MatchString(m.Baseline.SHA) || !strings.Contains(m.Baseline.EvidenceURL, m.Baseline.SHA) {
		t.Error("baseline must be pinned to a full SHA and matching URL")
	}
	a := m.Authority.Section94
	if a.SourceOfTruth != "#44" || a.Override || len(a.Unresolved) != 3 {
		t.Error("#44 must stay the sole linked source for section 9.4 items 1-3")
	}
	if a.URL != "https://github.com/mctlhq/mctl-glovo/issues/44" {
		t.Errorf("#44 url = %q", a.URL)
	}
	if m.Authority.Issue11.Owner != "lead" || m.Authority.Issue11.Status != "FOLLOW_UP_NOT_DONE" {
		t.Error("#11 correction must be a lead-owned follow-up")
	}
	// Baseline code facts: every cited path exists in this checkout.
	if len(m.Baseline.SourcePaths) == 0 {
		t.Error("baseline source paths missing")
	}
	for _, p := range m.Baseline.SourcePaths {
		if _, err := os.Stat("../../" + p); err != nil {
			t.Errorf("baseline source path %s: %v", p, err)
		}
	}
	// Open Draft PRs are separate from the baseline and never merged.
	wantDrafts := map[int]string{54: "a2b20b65", 56: "c1179519", 53: "a3af531c"}
	if len(m.Baseline.OpenDrafts) != len(wantDrafts) {
		t.Errorf("got %d draft PRs, want %d", len(m.Baseline.OpenDrafts), len(wantDrafts))
	}
	for _, d := range m.Baseline.OpenDrafts {
		if wantDrafts[d.Number] != d.Head || d.Merged || d.State != "draft" {
			t.Errorf("draft PR #%d = %+v, want open draft head %s", d.Number, d, wantDrafts[d.Number])
		}
	}
	if len(m.Baseline.RuntimeUnknowns) == 0 {
		t.Error("runtime unknowns missing")
	}
}

func TestTopicsCoverRequiredAreasWithMetadata(t *testing.T) {
	m := loadMatrix(t)
	required := []string{
		"owner-decision-49", "disabled-write-proof-41", "linking-42", "cutover-2777",
		"operations-46", "deploy-false", "vault-control-identity", "proxy", "migrations",
		"image-pins", "rollback-kill", "oauth", "scopes", "glovo-home-writes",
		"consent-versions", "isolation", "capacity",
		"ext-mctl-api-1108", "ext-mctl-api-1110", "ext-mctl-api-1111", "ext-mctl-api-1079",
		"ext-gitops-2742", "ext-gitops-2766", "ext-gitops-2709",
	}
	order := map[string]int{"G0": 0, "G1": 1, "G2": 2, "G3": 3, "G4": 4}
	roles := []string{"owner", "lead", "platform-operator", "service-maintainer"}
	for _, id := range required {
		tp := m.topic(id)
		if tp == nil {
			t.Errorf("topic %s missing", id)
			continue
		}
		if tp.Status != "UNKNOWN" {
			t.Errorf("topic %s has live status %q, want UNKNOWN", id, tp.Status)
		}
		if !contains(roles, tp.Role) {
			t.Errorf("topic %s has role %q", id, tp.Role)
		}
		if len(tp.Refs) == 0 || tp.Rollback == "" {
			t.Errorf("topic %s lacks refs or rollback pointer", id)
		}
		if tp.EvidenceSHA != "UNKNOWN" || tp.EvidenceURL != "UNKNOWN" || tp.Freshness.State != "UNKNOWN" || tp.Freshness.ObservedAt != nil {
			t.Errorf("topic %s has evidence not labelled UNKNOWN", id)
		}
		if v, _ := verdict(gate{Status: "PASS", EvidenceSHA: tp.EvidenceSHA, EvidenceURL: tp.EvidenceURL, Freshness: tp.Freshness}, refTime, dur(24*365)); v == "PASS" {
			t.Errorf("topic %s evidence evaluates to PASS", id)
		}
		e, ok := order[tp.Earliest]
		if !ok || m.gate(tp.Earliest) == nil {
			t.Errorf("topic %s earliest gate %q unknown", id, tp.Earliest)
			continue
		}
		for _, c := range tp.CarriedTo {
			if o, ok := order[c]; !ok || o <= e {
				t.Errorf("topic %s carried to %s, not after %s", id, c, tp.Earliest)
			}
		}
		for _, p := range tp.SourcePaths {
			if _, err := os.Stat("../../" + p); err != nil {
				t.Errorf("topic %s source path %s: %v", id, p, err)
			}
		}
	}
	if len(m.Topics) != len(required) {
		t.Errorf("got %d topics, want %d", len(m.Topics), len(required))
	}
}

func list(xs []string) string {
	if len(xs) == 0 {
		return "-"
	}
	return strings.Join(xs, ", ")
}

func yesno(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func renderTables(m matrix) string {
	var b strings.Builder
	b.WriteString("| Ref | Required | Stage | Note |\n|---|---|---|---|\n")
	for _, r := range m.DecisionRefs {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", r.ID, yesno(r.Required), r.Stage, r.Note)
	}
	b.WriteString("\n| Gate | Status | Role | Phase | Depends on | Prerequisites | Evidence SHA | Evidence URL | Freshness | Rollback |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")
	for i := range m.Gates {
		g := &m.Gates[i]
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			g.ID, g.Status, g.Role, g.Phase, list(g.DependsOn), list(g.Prerequisites),
			g.EvidenceSHA, g.EvidenceURL, g.Freshness.State, g.Rollback)
	}
	b.WriteString("\n| Topic | Role | Refs | Earliest gate | Carried to | Status | Evidence SHA | Evidence URL | Freshness | Rollback |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")
	for i := range m.Topics {
		tp := &m.Topics[i]
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			tp.Name, tp.Role, list(tp.Refs), tp.Earliest, list(tp.CarriedTo), tp.Status,
			tp.EvidenceSHA, tp.EvidenceURL, tp.Freshness.State, tp.Rollback)
	}
	return b.String()
}

const (
	matrixBegin = "<!-- BEGIN MATRIX -->\n"
	matrixEnd   = "<!-- END MATRIX -->"
	epicsBegin  = "<!-- BEGIN EPICS -->\n"
	epicsEnd    = "<!-- END EPICS -->"
)

// mdBlock returns the text between two markers.
func mdBlock(doc, begin, end string) (string, bool) {
	i, j := strings.Index(doc, begin), strings.Index(doc, end)
	if i < 0 || j < i {
		return "", false
	}
	return doc[i+len(begin) : j], true
}

// replaceBlock swaps the text between two markers.
func replaceBlock(doc, begin, end, body string) string {
	i, j := strings.Index(doc, begin), strings.Index(doc, end)
	return doc[:i+len(begin)] + body + doc[j:]
}

// markdownBlockErrors reports every generated Markdown block that differs
// from the rendering of the JSON data.
func markdownBlockErrors(m matrix, doc string) []string {
	var errs []string
	for _, blk := range []struct{ begin, end, want string }{
		{matrixBegin, matrixEnd, renderTables(m)},
		{epicsBegin, epicsEnd, renderEpics(m)},
	} {
		got, ok := mdBlock(doc, blk.begin, blk.end)
		if !ok {
			errs = append(errs, fmt.Sprintf("markers %q missing in markdown", blk.begin))
		} else if got != blk.want {
			errs = append(errs, fmt.Sprintf("block %q drifted.\nwant:\n%s\ngot:\n%s", blk.begin, blk.want, got))
		}
	}
	return errs
}

func TestMarkdownMatchesJSON(t *testing.T) {
	m := loadMatrix(t)
	raw, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatalf("read %s: %v", mdPath, err)
	}
	doc := string(raw)
	if errs := markdownBlockErrors(m, doc); len(errs) != 0 {
		t.Fatalf("markdown drifted from %s:\n%s", jsonPath, strings.Join(errs, "\n"))
	}
	if !strings.Contains(doc, m.Baseline.SHA) {
		t.Error("markdown must name the baseline SHA")
	}
	for _, p := range m.Baseline.SourcePaths {
		if !strings.Contains(doc, "`"+p+"`") {
			t.Errorf("markdown must cite baseline source path %s", p)
		}
	}
	for _, d := range m.Baseline.OpenDrafts {
		if !strings.Contains(doc, fmt.Sprintf("#%d (head `%s`)", d.Number, d.Head)) {
			t.Errorf("markdown must cite open draft PR #%d head %s", d.Number, d.Head)
		}
	}
	for _, u := range m.Baseline.RuntimeUnknowns {
		if !strings.Contains(doc, "- "+u+"\n") {
			t.Errorf("markdown must list runtime unknown %q", u)
		}
	}
	// #44 is linked from a single place.
	if n := strings.Count(doc, m.Authority.Section94.URL); n != 1 {
		t.Errorf("#44 url appears %d times in markdown, want 1", n)
	}
	// Stage and mapping statements in prose must not drift from the data.
	// Whitespace is normalized so line wrapping does not matter.
	flat := strings.Join(strings.Fields(doc), " ")
	for _, s := range []string{
		"#50 maps to G2", "#51 to G3", "#52 to optional G4",
		"Owner-only, 10 read-only, Approval+Canary, 20 read+write",
		"Owner-only maps to G0 and G1; 10 read-only maps to G2 and G3; 20 read+write maps to G4.",
		"Approval+Canary is a distinct dependency phase (#55, #57), not a gate and not an operational GO.",
		"G4 (twenty-user writes) depends on G3, #47, #44, #55 and #57",
		forbiddenStatement(m),
		"Allowed surfaces are explicit allowlists: G0 none, G1 to G3 read only, G4 read and basket-write only.",
		"Canary authorization belongs to G4 only.",
		m.PlanSource.URL,
		"not merged and not live authorization",
		"A field that was not transcribed is shown as `UNKNOWN (not transcribed)`. That is never a claim of zero dependencies.",
	} {
		if !strings.Contains(flat, s) {
			t.Errorf("markdown lost statement %q", s)
		}
	}
}

// forbiddenStatement is the canonical prose the Markdown must keep, built from
// the JSON forbidden list so the two cannot drift.
func forbiddenStatement(m matrix) string {
	f := m.Forbidden
	if len(f) < 2 {
		return "forbidden surfaces missing"
	}
	return "The " + strings.Join(f[:len(f)-1], ", ") + " and " + f[len(f)-1] + " surfaces are forbidden at every stage."
}

func renderEpics(m matrix) string {
	var b strings.Builder
	b.WriteString("| Epic | Issue | Maps to | Depends on | External depends on |\n|---|---|---|---|---|\n")
	for i := range m.PlanSource.Epics {
		e := &m.PlanSource.Epics[i]
		var ext []string
		for _, x := range e.External {
			ext = append(ext, x.Ref)
		}
		deps, exts := list(e.DependsOn), list(ext)
		// Only the dependency sets of gate-realizing epics, and the externals
		// of gate-carrying epics, were transcribed. An empty field outside that
		// scope is unknown, never "no dependencies".
		if len(e.DependsOn) == 0 && e.RealizedBy == "" {
			deps = notTranscribed
		}
		if len(ext) == 0 && e.RealizedBy == "" && e.CarriedBy == "" {
			exts = notTranscribed
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", e.Epic, e.Issue, e.Target, deps, exts)
	}
	return b.String()
}

const notTranscribed = "UNKNOWN (not transcribed)"

// epicErrors compares the actual dependency SETS of each gate with the pinned
// plan table. Any added or removed dependency is reported. The additional
// offline safeguards are documented separately and excluded from the epic set.
func epicErrors(m matrix) []string {
	var errs []string
	ps := m.PlanSource
	if !sha40.MatchString(ps.HeadSHA) || !strings.Contains(ps.URL, ps.HeadSHA) || !strings.Contains(ps.URL, ps.Path) || !validHTTPS(ps.URL) {
		errs = append(errs, "plan source must be pinned to a full head SHA, path and https url")
	}
	if ps.Merged || ps.PRState != "draft" || ps.PR != 366 {
		errs = append(errs, "plan source must be the open Draft PR 366, not merged")
	}
	byID := map[string]*epic{}
	for i := range ps.Epics {
		e := &ps.Epics[i]
		if _, dup := byID[e.Epic]; dup {
			errs = append(errs, "duplicate epic "+e.Epic)
		}
		byID[e.Epic] = e
	}
	expected := map[string][]string{}
	covered := map[string]bool{}
	for i := range ps.Epics {
		e := &ps.Epics[i]
		for _, d := range e.DependsOn {
			dep, ok := byID[d]
			if !ok {
				errs = append(errs, e.Epic+" depends on unknown epic "+d)
				continue
			}
			if e.RealizedBy != "" {
				expected[e.RealizedBy] = append(expected[e.RealizedBy], dep.Target)
			}
		}
		if e.RealizedBy != "" {
			covered[e.RealizedBy] = true
			if e.Target != e.RealizedBy {
				errs = append(errs, e.Epic+" realized_by "+e.RealizedBy+" must equal its target "+e.Target)
			}
		}
		for _, x := range e.External {
			if e.CarriedBy == "" {
				errs = append(errs, e.Epic+" has externals but no carrier gate")
				continue
			}
			covered[e.CarriedBy] = true
			expected[e.CarriedBy] = append(expected[e.CarriedBy], x.Target)
			if tp := m.topic(x.Target); tp != nil {
				if !contains(tp.Refs, x.Ref) || tp.Earliest != e.CarriedBy {
					errs = append(errs, "external topic "+x.Target+" must carry ref "+x.Ref+" at "+e.CarriedBy)
				}
			} else if x.Target != x.Ref {
				errs = append(errs, "external "+x.Ref+" has no topic and is not carried by its own ref")
			}
		}
		if tp := m.topic(e.Target); tp != nil && !contains(tp.Refs, e.Issue) {
			errs = append(errs, "topic "+tp.ID+" must reference "+e.Issue)
		}
	}
	for _, want := range []string{"G0", "G2", "G3", "G4"} {
		if !covered[want] {
			errs = append(errs, "no pinned epic covers "+want)
		}
	}
	for i := range m.Gates {
		g := &m.Gates[i]
		if !covered[g.ID] {
			continue
		}
		extra := ps.Safeguards[g.ID]
		var actual []string
		for _, set := range [][]string{g.DependsOn, g.Prerequisites, g.Requires} {
			for _, d := range set {
				if !contains(extra, d) {
					actual = append(actual, d)
				}
			}
		}
		for _, x := range extra {
			if !contains(g.DependsOn, x) && !contains(g.Prerequisites, x) && !contains(g.Requires, x) {
				errs = append(errs, g.ID+" documents safeguard "+x+" that it does not list")
			}
		}
		if !sameSet(actual, expected[g.ID]) {
			errs = append(errs, fmt.Sprintf("%s dependency set %v differs from the pinned plan %v", g.ID, actual, expected[g.ID]))
		}
	}
	return errs
}

func TestEpicDependenciesMatchPinnedPlan(t *testing.T) {
	m := loadMatrix(t)
	for _, e := range epicErrors(m) {
		t.Error(e)
	}
	mutate := func(id string, f func(*gate)) matrix {
		bad := m
		bad.Gates = append([]gate(nil), m.Gates...)
		for i := range bad.Gates {
			if bad.Gates[i].ID == id {
				f(&bad.Gates[i])
			}
		}
		return bad
	}
	without := func(xs []string, x string) []string {
		var out []string
		for _, v := range xs {
			if v != x {
				out = append(out, v)
			}
		}
		return out
	}
	cases := map[string]matrix{
		"G3 drops #45":        mutate("G3", func(g *gate) { g.DependsOn = without(g.DependsOn, "#45") }),
		"G3 drops #44":        mutate("G3", func(g *gate) { g.DependsOn = without(g.DependsOn, "#44") }),
		"G3 adds #47":         mutate("G3", func(g *gate) { g.DependsOn = append(append([]string(nil), g.DependsOn...), "#47") }),
		"G4 drops #47":        mutate("G4", func(g *gate) { g.DependsOn = without(g.DependsOn, "#47") }),
		"G4 drops #44":        mutate("G4", func(g *gate) { g.DependsOn = without(g.DependsOn, "#44") }),
		"G4 drops 1006":       mutate("G4", func(g *gate) { g.Requires = without(g.Requires, "mctlhq/mctl-api#1006") }),
		"G2 adds #44":         mutate("G2", func(g *gate) { g.DependsOn = append(append([]string(nil), g.DependsOn...), "#44") }),
		"G0 drops 1108":       mutate("G0", func(g *gate) { g.Prerequisites = without(g.Prerequisites, "ext-mctl-api-1108") }),
		"G0 drops cutover":    mutate("G0", func(g *gate) { g.Prerequisites = without(g.Prerequisites, "cutover-2777") }),
		"G0 adds unknown dep": mutate("G0", func(g *gate) { g.Prerequisites = append(append([]string(nil), g.Prerequisites...), "extra-dep") }),
	}
	for name, bad := range cases {
		if len(epicErrors(bad)) == 0 {
			t.Errorf("%s was not refused", name)
		}
	}
	// Epic-set change in the plan table itself is also visible.
	bad := m
	bad.PlanSource.Epics = append([]epic(nil), m.PlanSource.Epics...)
	for i := range bad.PlanSource.Epics {
		if bad.PlanSource.Epics[i].Epic == "c2-go-10" {
			bad.PlanSource.Epics[i].DependsOn = []string{"c2-go-2"}
		}
	}
	if len(epicErrors(bad)) == 0 {
		t.Error("plan table losing G3 dependencies was not refused")
	}
}

// Public-artifact hygiene. This is heuristic: it flags likely principal
// identifiers, tailnet host suffixes, token prefixes, long digit runs and
// order-content markers. It does not claim to find all private data or
// arbitrary shopping prose.
type hygieneRule struct {
	class string
	re    *regexp.Regexp
}

var hygieneRules = []hygieneRule{
	{"principal", regexp.MustCompile(`(?i)\bprincipals?\s*[:=]\s*\S+`)},
	{"principal", regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)},
	{"principal", regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)},
	{"tailnet", regexp.MustCompile(`(?i)\.ts\.net\b|\.tailscale\.(net|com)\b|\btailnet\b`)},
	{"token", regexp.MustCompile(`\b(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{8,}`)},
	{"token", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{8,}`)},
	{"token", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}`)},
	{"token", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{8,}`)},
	{"token", regexp.MustCompile(`\bAKIA[0-9A-Z]{12,}`)},
	{"token", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.`)},
	{"token", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}`)},
	{"token", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY`)},
	{"digits", regexp.MustCompile(`\d{9,}`)},
	{"order-content", regexp.MustCompile(`(?i)\border[ _-]?(id|number|no)\b`)},
	{"order-content", regexp.MustCompile(`(?i)\b(delivery|shipping) address\b`)},
	{"order-content", regexp.MustCompile(`\b\d+\s?[x×]\s+[A-Za-z]{3,}`)},
}

var fullSHA = regexp.MustCompile(`\b[0-9a-f]{40}\b`)

// hygieneFindings returns the classes that match text. The one exception is a
// full 40-hex commit SHA that contains a hex letter: it is removed before the
// digit-run rule so legitimate pins do not trip it. All other text, including
// note strings, is scanned.
func hygieneFindings(text string) []string {
	text = fullSHA.ReplaceAllStringFunc(text, func(s string) string {
		if strings.Trim(s, "0123456789") == "" {
			return s
		}
		return " "
	})
	var out []string
	for _, r := range hygieneRules {
		if r.re.MatchString(text) && !contains(out, r.class) {
			out = append(out, r.class)
		}
	}
	return out
}

// jsonStrings returns every string value, object key and number literal in a
// JSON document.
func jsonStrings(raw []byte) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var out []string
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case string:
			out = append(out, t)
		case json.Number:
			out = append(out, t.String())
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for k, e := range t {
				out = append(out, k)
				walk(e)
			}
		}
	}
	walk(v)
	return out, nil
}

func scanMarkdown(doc string) []string { return hygieneFindings(doc) }

func scanJSON(raw []byte) ([]string, error) {
	strs, err := jsonStrings(raw)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range strs {
		for _, c := range hygieneFindings(s) {
			if !contains(out, c) {
				out = append(out, c)
			}
		}
	}
	return out, nil
}

// hygieneSamples are harmless synthetic strings built at run time. None is a
// real secret, identity or order.
func hygieneSamples() map[string][]string {
	return map[string][]string{
		"principal": {
			"principal: " + "user-" + strings.Repeat("x", 6),
			"owner" + "@" + "example.invalid",
			"11111111-2222-3333-4444-555555555555",
		},
		"tailnet": {"mini-host.tail1234." + "ts" + ".net", "the tailnet name"},
		"token": {
			"ghp" + "_" + strings.Repeat("A", 24),
			"github_pat" + "_" + strings.Repeat("B", 24),
			"sk" + "-" + strings.Repeat("c", 20),
			"xoxb" + "-" + strings.Repeat("1", 10),
			"Bearer " + strings.Repeat("d", 20),
			"eyJ" + strings.Repeat("e", 20) + ".payload",
		},
		"digits":        {"ref " + strings.Repeat("7", 12)},
		"order-content": {"order id 12", "delivery address line", "2 x margherita"},
	}
}

func TestHygieneDetectorsOnSyntheticCases(t *testing.T) {
	for class, samples := range hygieneSamples() {
		for i, s := range samples {
			if !contains(scanMarkdown("Some prose. "+s+" more prose."), class) {
				t.Errorf("markdown sample %s/%d not detected", class, i)
			}
			raw, err := json.Marshal(map[string]any{"note": s, "list": []string{"ok", s}})
			if err != nil {
				t.Fatal(err)
			}
			got, err := scanJSON(raw)
			if err != nil || !contains(got, class) {
				t.Errorf("json sample %s/%d not detected (%v, %v)", class, i, got, err)
			}
			// A key or a bare number is scanned as well.
			raw, err = json.Marshal(map[string]any{s: "ok"})
			if err != nil {
				t.Fatal(err)
			}
			if got, err = scanJSON(raw); err != nil || !contains(got, class) {
				t.Errorf("json key sample %s/%d not detected (%v, %v)", class, i, got, err)
			}
		}
	}
	if got, err := scanJSON([]byte(`{"n": 123456789012}`)); err != nil || !contains(got, "digits") {
		t.Errorf("json number literal not detected (%v, %v)", got, err)
	}
}

func TestHygieneAvoidsLegitimateValues(t *testing.T) {
	m := loadMatrix(t)
	good := []string{
		m.Baseline.SHA,
		m.Baseline.EvidenceURL,
		m.PlanSource.HeadSHA,
		m.PlanSource.URL,
		"2026-11-18T12:00:00Z",
		"https://github.com/mctlhq/mctl-glovo/issues/44",
		"mctlhq/mctl-api#1006",
		strings.Repeat("a", 40),
	}
	for _, s := range good {
		if got := hygieneFindings(s); len(got) != 0 {
			t.Errorf("legitimate value %q flagged as %v", s, got)
		}
	}
	// A 40-digit run is not a commit SHA and is still flagged.
	if !contains(hygieneFindings(strings.Repeat("9", 40)), "digits") {
		t.Error("a 40-digit run must still be flagged")
	}
}

func TestPublicArtifactsHaveNoSensitiveMarkers(t *testing.T) {
	rawJSON, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := scanJSON(rawJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("%s contains likely sensitive marker classes %v", jsonPath, got)
	}
	rawMD, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := scanMarkdown(string(rawMD)); len(got) != 0 {
		t.Errorf("%s contains likely sensitive marker classes %v", mdPath, got)
	}
}

// ---------------------------------------------------------------------------
// Independent golden data for the pinned epic plan (mctlhq/.github Draft PR 366
// at head 97371421997c67260843168783d343d4c7d5fff9). It is written separately
// from the JSON and the Markdown so that a change made consistently in the
// gate, the JSON epic row and the Markdown table is still caught. Nothing is
// fetched: this is a second hand transcription.
// ---------------------------------------------------------------------------

func extd(ref, target string) extDep { return extDep{Ref: ref, Target: target} }

var goldenEpics = []epic{
	{
		Epic: "c2-go-owner", Issue: "#40", Target: "G0", RealizedBy: "G0", CarriedBy: "G0",
		DependsOn: []string{"c2-write-gate-decision", "c2-write-disabled-proof", "c2-fresh-link-identity", "c2-cutover-package", "c2-pilot-operations"},
		External: []extDep{
			extd("mctlhq/mctl-api#1108", "ext-mctl-api-1108"),
			extd("mctlhq/mctl-api#1110", "ext-mctl-api-1110"),
			extd("mctlhq/mctl-api#1111", "ext-mctl-api-1111"),
			extd("mctlhq/mctl-api#1079", "ext-mctl-api-1079"),
			extd("mctlhq/mctl-gitops#2742", "ext-gitops-2742"),
			extd("mctlhq/mctl-gitops#2766", "ext-gitops-2766"),
			extd("mctlhq/mctl-gitops#2709", "ext-gitops-2709"),
		},
	},
	{Epic: "c2-write-gate-decision", Issue: "#49", Target: "owner-decision-49"},
	{Epic: "c2-write-disabled-proof", Issue: "#41", Target: "disabled-write-proof-41"},
	{Epic: "c2-fresh-link-identity", Issue: "#42", Target: "linking-42"},
	{Epic: "c2-cutover-package", Issue: "mctl-gitops#2777", Target: "cutover-2777"},
	{Epic: "c2-pilot-operations", Issue: "#46", Target: "operations-46"},
	{Epic: "c2-e2e-resilience", Issue: "#43", Target: "G1"},
	{Epic: "c2-go-2", Issue: "#50", Target: "G2", RealizedBy: "G2", DependsOn: []string{"c2-e2e-resilience"}},
	{Epic: "c2-security-boundary", Issue: "#44", Target: "#44"},
	{Epic: "c2-order-intelligence", Issue: "#45", Target: "#45"},
	{Epic: "c2-go-10", Issue: "#51", Target: "G3", RealizedBy: "G3", DependsOn: []string{"c2-go-2", "c2-security-boundary", "c2-order-intelligence"}},
	{Epic: "c2-cohort-feedback", Issue: "#47", Target: "#47"},
	{
		Epic: "c2-write-approval-proof", Issue: "#55", Target: "#55", CarriedBy: "G4",
		External: []extDep{
			extd("mctlhq/mctl-api#1006", "mctlhq/mctl-api#1006"),
			extd("mctlhq/mctl-api#1011", "mctlhq/mctl-api#1011"),
			extd("mctlhq/mctl-api#1012", "mctlhq/mctl-api#1012"),
		},
	},
	{Epic: "c2-write-canary", Issue: "#57", Target: "#57"},
	{Epic: "c2-go-20", Issue: "#52", Target: "G4", RealizedBy: "G4", DependsOn: []string{"c2-go-10", "c2-cohort-feedback", "c2-security-boundary", "c2-write-approval-proof", "c2-write-canary"}},
}

// goldenEpicTable is the expected Markdown rendering of the source table.
const goldenEpicTable = `| Epic | Issue | Maps to | Depends on | External depends on |
|---|---|---|---|---|
| c2-go-owner | #40 | G0 | c2-write-gate-decision, c2-write-disabled-proof, c2-fresh-link-identity, c2-cutover-package, c2-pilot-operations | mctlhq/mctl-api#1108, mctlhq/mctl-api#1110, mctlhq/mctl-api#1111, mctlhq/mctl-api#1079, mctlhq/mctl-gitops#2742, mctlhq/mctl-gitops#2766, mctlhq/mctl-gitops#2709 |
| c2-write-gate-decision | #49 | owner-decision-49 | UNKNOWN (not transcribed) | UNKNOWN (not transcribed) |
| c2-write-disabled-proof | #41 | disabled-write-proof-41 | UNKNOWN (not transcribed) | UNKNOWN (not transcribed) |
| c2-fresh-link-identity | #42 | linking-42 | UNKNOWN (not transcribed) | UNKNOWN (not transcribed) |
| c2-cutover-package | mctl-gitops#2777 | cutover-2777 | UNKNOWN (not transcribed) | UNKNOWN (not transcribed) |
| c2-pilot-operations | #46 | operations-46 | UNKNOWN (not transcribed) | UNKNOWN (not transcribed) |
| c2-e2e-resilience | #43 | G1 | UNKNOWN (not transcribed) | UNKNOWN (not transcribed) |
| c2-go-2 | #50 | G2 | c2-e2e-resilience | - |
| c2-security-boundary | #44 | #44 | UNKNOWN (not transcribed) | UNKNOWN (not transcribed) |
| c2-order-intelligence | #45 | #45 | UNKNOWN (not transcribed) | UNKNOWN (not transcribed) |
| c2-go-10 | #51 | G3 | c2-go-2, c2-security-boundary, c2-order-intelligence | - |
| c2-cohort-feedback | #47 | #47 | UNKNOWN (not transcribed) | UNKNOWN (not transcribed) |
| c2-write-approval-proof | #55 | #55 | UNKNOWN (not transcribed) | mctlhq/mctl-api#1006, mctlhq/mctl-api#1011, mctlhq/mctl-api#1012 |
| c2-write-canary | #57 | #57 | UNKNOWN (not transcribed) | UNKNOWN (not transcribed) |
| c2-go-20 | #52 | G4 | c2-go-10, c2-cohort-feedback, c2-security-boundary, c2-write-approval-proof, c2-write-canary | - |
`

// goldenGateSet is the expected dependency content of one gate. The epic part
// (internal plus external) comes from the pinned plan. The safeguards are the
// extra offline readiness items that are NOT epic dependencies.
type goldenGateSet struct {
	internal   []string
	external   []string
	safeguards []string
}

var goldenGateSets = map[string]goldenGateSet{
	"G0": {
		internal: []string{"owner-decision-49", "disabled-write-proof-41", "linking-42", "cutover-2777", "operations-46"},
		external: []string{"ext-mctl-api-1108", "ext-mctl-api-1110", "ext-mctl-api-1111", "ext-mctl-api-1079", "ext-gitops-2742", "ext-gitops-2766", "ext-gitops-2709"},
		safeguards: []string{
			"deploy-false", "vault-control-identity", "proxy", "migrations", "image-pins", "rollback-kill",
			"oauth", "scopes", "glovo-home-writes", "consent-versions", "isolation", "capacity",
		},
	},
	"G1": {internal: []string{"G0"}},
	"G2": {internal: []string{"G1"}},
	"G3": {internal: []string{"G2", "#44", "#45"}},
	"G4": {
		internal: []string{"G3", "#47", "#44", "#55", "#57"},
		external: []string{"mctlhq/mctl-api#1006", "mctlhq/mctl-api#1011", "mctlhq/mctl-api#1012"},
	},
}

// goldenErrors compares the actual gate sets, the transcribed JSON epic rows
// and the Markdown source table with the independent expectation above.
func goldenErrors(m matrix, doc string) []string {
	var errs []string
	if len(m.PlanSource.Epics) != len(goldenEpics) {
		errs = append(errs, fmt.Sprintf("epic row count %d, golden %d", len(m.PlanSource.Epics), len(goldenEpics)))
	}
	for wi := range goldenEpics {
		want := &goldenEpics[wi]
		var got *epic
		for i := range m.PlanSource.Epics {
			if m.PlanSource.Epics[i].Epic == want.Epic {
				got = &m.PlanSource.Epics[i]
			}
		}
		switch {
		case got == nil:
			errs = append(errs, "epic "+want.Epic+" missing from the JSON table")
		case !reflect.DeepEqual(*got, *want):
			errs = append(errs, "epic "+want.Epic+" JSON row differs from the golden row")
		}
	}
	if renderEpics(m) != goldenEpicTable {
		errs = append(errs, "rendered JSON epic table differs from the golden table")
	}
	if blk, ok := mdBlock(doc, epicsBegin, epicsEnd); !ok || blk != goldenEpicTable {
		errs = append(errs, "Markdown epic table differs from the golden table")
	}
	for _, id := range []string{"G0", "G1", "G2", "G3", "G4"} {
		want := goldenGateSets[id]
		g := m.gate(id)
		if g == nil {
			errs = append(errs, "gate "+id+" missing")
			continue
		}
		var all []string
		all = append(all, g.DependsOn...)
		all = append(all, g.Prerequisites...)
		all = append(all, g.Requires...)
		if !sameSet(m.PlanSource.Safeguards[id], want.safeguards) {
			errs = append(errs, fmt.Sprintf("%s documented safeguards %v differ from golden %v", id, m.PlanSource.Safeguards[id], want.safeguards))
		}
		for _, s := range want.safeguards {
			if !contains(all, s) {
				errs = append(errs, id+" does not list safeguard "+s)
			}
		}
		var epicPart []string
		for _, d := range all {
			if !contains(want.safeguards, d) {
				epicPart = append(epicPart, d)
			}
		}
		wantEpic := append(append([]string(nil), want.internal...), want.external...)
		if !sameSet(epicPart, wantEpic) {
			errs = append(errs, fmt.Sprintf("%s epic dependency set %v differs from golden %v", id, epicPart, wantEpic))
		}
	}
	for id := range m.PlanSource.Safeguards {
		if id != "G0" {
			errs = append(errs, "unexpected safeguards documented for "+id)
		}
	}
	return errs
}

func TestPinnedPlanMatchesGolden(t *testing.T) {
	m := loadMatrix(t)
	raw, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	if m.PlanSource.HeadSHA != "97371421997c67260843168783d343d4c7d5fff9" {
		t.Errorf("plan head = %s, golden data is for 97371421997c67260843168783d343d4c7d5fff9", m.PlanSource.HeadSHA)
	}
	for _, e := range goldenErrors(m, string(raw)) {
		t.Error(e)
	}
}

// cloneMatrix copies the slices that the mutation checks edit.
func cloneMatrix(m matrix) matrix {
	m.Gates = append([]gate(nil), m.Gates...)
	m.Topics = append([]topic(nil), m.Topics...)
	m.PlanSource.Epics = append([]epic(nil), m.PlanSource.Epics...)
	return m
}

func TestGoldenCatchesCoordinatedChange(t *testing.T) {
	m := loadMatrix(t)
	raw, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	cases := []struct {
		name     string
		gateID   string
		gateDeps []string
		epicName string
		epicDeps []string
	}{
		{"G3 drops #45", "G3", []string{"G2", "#44"}, "c2-go-10", []string{"c2-go-2", "c2-security-boundary"}},
		{"G3 adds #47", "G3", []string{"G2", "#44", "#45", "#47"}, "c2-go-10", []string{"c2-go-2", "c2-security-boundary", "c2-order-intelligence", "c2-cohort-feedback"}},
		{"G4 drops #47", "G4", []string{"G3", "#44", "#55", "#57"}, "c2-go-20", []string{"c2-go-10", "c2-security-boundary", "c2-write-approval-proof", "c2-write-canary"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bad := cloneMatrix(m)
			for i := range bad.Gates {
				if bad.Gates[i].ID == c.gateID {
					bad.Gates[i].DependsOn = c.gateDeps
				}
			}
			for i := range bad.PlanSource.Epics {
				if bad.PlanSource.Epics[i].Epic == c.epicName {
					bad.PlanSource.Epics[i].DependsOn = c.epicDeps
				}
			}
			badDoc := replaceBlock(doc, matrixBegin, matrixEnd, renderTables(bad))
			badDoc = replaceBlock(badDoc, epicsBegin, epicsEnd, renderEpics(bad))
			// The coordinated change is internally consistent: the gate, the
			// JSON epic row and the Markdown agree, so these checks pass.
			if errs := epicErrors(bad); len(errs) != 0 {
				t.Fatalf("fixture is not coordinated, epicErrors: %v", errs)
			}
			if errs := markdownBlockErrors(bad, badDoc); len(errs) != 0 {
				t.Fatalf("fixture is not coordinated, markdown drift: %v", errs)
			}
			// Only the independent golden data catches it.
			errs := goldenErrors(bad, badDoc)
			if len(errs) == 0 {
				t.Fatal("coordinated change was not caught by the golden data")
			}
			if !anyContains(errs, c.gateID) {
				t.Errorf("golden errors do not name %s: %v", c.gateID, errs)
			}
		})
	}
	// The extra offline safeguards stay separate from the epic dependencies:
	// folding one into the documented epic set is caught.
	bad := cloneMatrix(m)
	bad.PlanSource.Safeguards = map[string][]string{"G0": append(append([]string(nil), m.PlanSource.Safeguards["G0"]...), "owner-decision-49")}
	if len(goldenErrors(bad, doc)) == 0 {
		t.Error("an epic dependency relabelled as a safeguard was not caught")
	}
}

func anyContains(xs []string, sub string) bool {
	for _, x := range xs {
		if strings.Contains(x, sub) {
			return true
		}
	}
	return false
}

func TestLeafEpicDependenciesNotClaimedZero(t *testing.T) {
	m := loadMatrix(t)
	for _, e := range m.PlanSource.Epics {
		row := ""
		for _, line := range strings.Split(renderEpics(m), "\n") {
			if strings.HasPrefix(line, "| "+e.Epic+" |") {
				row = line
			}
		}
		if row == "" {
			t.Errorf("epic %s has no rendered row", e.Epic)
			continue
		}
		leaf := e.RealizedBy == "" && e.CarriedBy == ""
		if leaf && (strings.Contains(row, "| - |") || strings.Count(row, notTranscribed) != 2) {
			t.Errorf("leaf epic %s must show both fields as not transcribed: %s", e.Epic, row)
		}
		if !leaf && strings.Contains(row, notTranscribed) && e.Epic != "c2-write-approval-proof" {
			t.Errorf("transcribed epic %s shows an unknown field: %s", e.Epic, row)
		}
	}
}

// ---------------------------------------------------------------------------
// Safety prose that the Markdown must keep. Each clause has a small in-memory
// mutation that flips its meaning; the same checker that validates the
// committed file must then name the clause as missing.
// ---------------------------------------------------------------------------

type proseClause struct {
	name     string
	text     string
	weakened string
}

var proseClauses = []proseClause{
	{
		"glovo-home-no-order-checkout-payment",
		"glovo-home never gains an order, checkout or payment tool.",
		"glovo-home may gain an order, checkout or payment tool.",
	},
	{
		"no-pass-or-flag-authorization",
		"Nothing here marks a condition PASS or authorizes opening any flag.",
		"Something here marks a condition PASS or authorizes opening any flag.",
	},
	{
		"pass-never-crosses-stages",
		"A PASS at one stage never authorizes another.",
		"A PASS at one stage may authorize another.",
	},
	{
		"writes-use-existing-approval-no-second-ledger",
		"Basket writes may only use the approval defined by mctlhq/mctl-api#1011 and #1012 (status UNKNOWN, not fetched here); mctl-glovo introduces no second approval ledger.",
		"Basket writes may use the approval defined by mctlhq/mctl-api#1011 and #1012 (status UNKNOWN, not fetched here); mctl-glovo introduces a second approval ledger.",
	},
	{
		"canary-needs-separate-owner-go",
		"The #57 write canary requires a separate operational owner GO, which is never implied by the dependency phase or a G4 PASS.",
		"The #57 write canary requires a separate operational owner GO, which is implied by the dependency phase or a G4 PASS.",
	},
}

// proseErrors names every required clause that is absent from doc. Whitespace
// is normalized so line wrapping does not matter.
func proseErrors(doc string) []string {
	flat := strings.Join(strings.Fields(doc), " ")
	var errs []string
	for _, c := range proseClauses {
		if !strings.Contains(flat, c.text) {
			errs = append(errs, "missing clause "+c.name)
		}
	}
	return append(errs, bareExistingErrors(doc)...)
}

func TestSafetyProsePinned(t *testing.T) {
	raw, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	if errs := proseErrors(doc); len(errs) != 0 {
		t.Fatalf("committed Markdown lost safety prose: %v", errs)
	}
	flat := strings.Join(strings.Fields(doc), " ")
	for _, c := range proseClauses {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(flat, c.text) {
				t.Fatal("clause text not found")
			}
			mutated := strings.Replace(flat, c.text, c.weakened, 1)
			if mutated == flat {
				t.Fatal("mutation changed nothing")
			}
			errs := proseErrors(mutated)
			if len(errs) != 1 || errs[0] != "missing clause "+c.name {
				t.Fatalf("mutation of %s reported %v, want exactly that clause", c.name, errs)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test-only authorization model. Synthetic fixtures only; the committed
// matrix never passes. This helper is not operational authority.
// ---------------------------------------------------------------------------

func goodTopic(id string, refs ...string) topic {
	return topic{
		ID: id, Status: "PASS", Refs: refs,
		EvidenceSHA: strings.Repeat("b", 40),
		EvidenceURL: "https://example.invalid/evidence/" + id,
		Freshness:   freshness{ObservedAt: strp("2026-11-18T11:00:00Z"), State: "FRESH"},
	}
}

// syntheticMatrix is a fully passing G0 to G4 chain: every prerequisite topic,
// every symbolic issue reference, every external and the G4 canary pass.
func syntheticMatrix() matrix {
	g0 := goodGate("G0", "D0")
	g0.Prerequisites = []string{"prereq-a", "prereq-b"}
	g1 := goodGate("G1", "")
	g1.DependsOn = []string{"G0"}
	g2 := goodGate("G2", "D2")
	g2.DependsOn = []string{"G1"}
	g3 := goodGate("G3", "D3")
	g3.DependsOn = []string{"G2", "#44", "#45"}
	g4 := goodGate("G4", "D4")
	g4.DependsOn = []string{"G3", "#47", "#44", "#55", "#57"}
	g4.Requires = []string{"mctlhq/mctl-api#1006", "mctlhq/mctl-api#1011", "mctlhq/mctl-api#1012"}
	g4.Canary = &canary{
		DecisionID: "D-canary", Status: "PASS",
		EvidenceSHA: strings.Repeat("c", 40),
		EvidenceURL: "https://example.invalid/evidence/canary",
		Freshness:   freshness{ObservedAt: strp("2026-11-18T11:00:00Z"), State: "FRESH"},
	}
	return matrix{
		Gates: []gate{g0, g1, g2, g3, g4},
		Topics: []topic{
			goodTopic("prereq-a", "#1"), goodTopic("prereq-b", "#2"),
			goodTopic("dep-44", "#44"), goodTopic("dep-45", "#45"), goodTopic("dep-47", "#47"),
			goodTopic("dep-55", "#55"), goodTopic("dep-57", "#57"),
			goodTopic("ext-1006", "mctlhq/mctl-api#1006"),
			goodTopic("ext-1011", "mctlhq/mctl-api#1011"),
			goodTopic("ext-1012", "mctlhq/mctl-api#1012"),
		},
	}
}

func dropGate(m *matrix, id string) {
	var keep []gate
	for i := range m.Gates {
		if m.Gates[i].ID != id {
			keep = append(keep, m.Gates[i])
		}
	}
	m.Gates = keep
}

func dropTopic(m *matrix, id string) {
	var keep []topic
	for i := range m.Topics {
		if m.Topics[i].ID != id {
			keep = append(keep, m.Topics[i])
		}
	}
	m.Topics = keep
}

func setTopic(m *matrix, id string, f func(*topic)) {
	for i := range m.Topics {
		if m.Topics[i].ID == id {
			f(&m.Topics[i])
		}
	}
}

func TestAuthorizationModelPositiveFixture(t *testing.T) {
	m := syntheticMatrix()
	for _, id := range []string{"G0", "G1", "G2", "G3", "G4"} {
		if errs := authorizationErrors(m, id, refTime, dur(24)); len(errs) != 0 {
			t.Errorf("%s should be authorized by the synthetic fixture: %v", id, errs)
		}
	}
	// Without an approved ttl nothing passes even in the fixture.
	if authorized(m, "G0", refTime, nil) {
		t.Error("fixture authorized without an approved ttl")
	}
}

func TestAuthorizationModelRedChecks(t *testing.T) {
	cases := []struct {
		name         string
		mutate       func(*matrix)
		unauthorized []string
		stillOK      []string
		want         string
	}{
		{"G3 missing", func(m *matrix) { dropGate(m, "G3") }, []string{"G3", "G4"}, []string{"G2"}, "gate G3 missing"},
		{"G3 not PASS", func(m *matrix) { m.gate("G3").Status = "UNKNOWN" }, []string{"G3", "G4"}, []string{"G2"}, "gate G3 is UNKNOWN"},
		{"G3 stale evidence", func(m *matrix) { m.gate("G3").Freshness.ObservedAt = strp("2025-01-01T00:00:00Z") }, []string{"G4"}, []string{"G2"}, "evidence stale"},
		{"G2 not PASS blocks G3", func(m *matrix) { m.gate("G2").Status = "FAIL" }, []string{"G2", "G3", "G4"}, []string{"G1"}, "gate G2 is FAIL"},
		{"UNKNOWN prerequisite topic", func(m *matrix) { setTopic(m, "prereq-a", func(tp *topic) { tp.Status = "UNKNOWN" }) }, []string{"G0", "G1", "G2", "G3", "G4"}, nil, "topic prereq-a"},
		{"missing prerequisite topic", func(m *matrix) { dropTopic(m, "prereq-b") }, []string{"G0", "G4"}, nil, "reference \"prereq-b\" is unresolved"},
		{"prerequisite topic without fresh evidence", func(m *matrix) { setTopic(m, "prereq-a", func(tp *topic) { tp.Freshness.State = "UNKNOWN" }) }, []string{"G0", "G4"}, nil, "freshness state is not FRESH"},
		{"UNKNOWN canary", func(m *matrix) { m.gate("G4").Canary.Status = "UNKNOWN" }, []string{"G4"}, []string{"G3"}, "canary is UNKNOWN"},
		{"canary stale evidence", func(m *matrix) { m.gate("G4").Canary.Freshness.ObservedAt = strp("2025-01-01T00:00:00Z") }, []string{"G4"}, []string{"G3"}, "canary is UNKNOWN: evidence stale"},
		{"canary absent", func(m *matrix) { m.gate("G4").Canary = nil }, []string{"G4"}, []string{"G3"}, "canary missing"},
		{"unobserved symbolic #44", func(m *matrix) { dropTopic(m, "dep-44") }, []string{"G3", "G4"}, []string{"G2"}, "reference \"#44\" is unresolved"},
		{"symbolic #55 not PASS", func(m *matrix) { setTopic(m, "dep-55", func(tp *topic) { tp.Status = "UNKNOWN" }) }, []string{"G4"}, []string{"G3"}, "topic dep-55"},
		{"symbolic ref with one non-PASS topic", func(m *matrix) {
			extra := goodTopic("dep-44-b", "#44")
			extra.Status = "UNKNOWN"
			m.Topics = append(m.Topics, extra)
		}, []string{"G3", "G4"}, []string{"G2"}, "topic dep-44-b"},
		{"unobserved external 1011", func(m *matrix) { dropTopic(m, "ext-1011") }, []string{"G4"}, []string{"G3"}, "reference \"mctlhq/mctl-api#1011\" is unresolved"},
		{"literal UNKNOWN reference", func(m *matrix) { m.gate("G0").Prerequisites = append(m.gate("G0").Prerequisites, "UNKNOWN") }, []string{"G0", "G4"}, nil, "reference \"UNKNOWN\" is unresolved"},
		{"cycle between G1 and G2", func(m *matrix) { m.gate("G1").DependsOn = []string{"G0", "G2"} }, []string{"G1", "G2", "G4"}, []string{"G0"}, "dependency cycle"},
		{"self cycle on G0", func(m *matrix) { m.gate("G0").DependsOn = []string{"G0"} }, []string{"G0", "G4"}, nil, "dependency cycle"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := syntheticMatrix()
			if !authorized(m, "G4", refTime, dur(24)) {
				t.Fatal("positive control failed before the mutation")
			}
			c.mutate(&m)
			for _, id := range c.unauthorized {
				errs := authorizationErrors(m, id, refTime, dur(24))
				if len(errs) == 0 {
					t.Errorf("%s stayed authorized", id)
					continue
				}
				if !anyContains(errs, c.want) {
					t.Errorf("%s errors %v lack %q", id, errs, c.want)
				}
			}
			for _, id := range c.stillOK {
				if errs := authorizationErrors(m, id, refTime, dur(24)); len(errs) != 0 {
					t.Errorf("%s should be unaffected: %v", id, errs)
				}
			}
		})
	}
}

func TestAuthorizationModelStructuralChecks(t *testing.T) {
	m0 := syntheticMatrix()
	for _, id := range []string{"G0", "G1", "G2", "G3", "G4"} {
		if errs := authorizationErrors(m0, id, refTime, dur(24)); len(errs) != 0 {
			t.Fatalf("positive control: %s not authorized: %v", id, errs)
		}
	}
	setDeps := func(m *matrix, id string, deps []string) { m.gate(id).DependsOn = deps }
	cases := []struct {
		name         string
		mutate       func(*matrix)
		unauthorized []string
		stillOK      []string
		want         string
	}{
		{"dropped G4 link to G3", func(m *matrix) {
			m.gate("G3").Status = "UNKNOWN"
			setDeps(m, "G4", []string{"#47", "#44", "#55", "#57"})
		}, []string{"G4"}, []string{"G2"}, "gate G3 is UNKNOWN"},
		{"dropped G4 link to a missing G3", func(m *matrix) {
			dropGate(m, "G3")
			setDeps(m, "G4", []string{"#47", "#44", "#55", "#57"})
		}, []string{"G4"}, []string{"G2"}, "gate G3 missing"},
		{"dropped G2 link to G1", func(m *matrix) {
			m.gate("G1").Status = "UNKNOWN"
			setDeps(m, "G2", nil)
		}, []string{"G2", "G3", "G4"}, []string{"G0"}, "gate G1 is UNKNOWN"},
		{"dropped G4.Requires", func(m *matrix) {
			m.gate("G4").Requires = nil
			dropTopic(m, "ext-1011")
		}, []string{"G4"}, []string{"G3"}, "reference \"mctlhq/mctl-api#1011\" is unresolved"},
		{"dropped G4.Requires with non-PASS proof", func(m *matrix) {
			m.gate("G4").Requires = nil
			setTopic(m, "ext-1006", func(tp *topic) { tp.Status = "UNKNOWN" })
		}, []string{"G4"}, []string{"G3"}, "topic ext-1006"},
		{"dropped G4 input dependencies", func(m *matrix) {
			setDeps(m, "G4", nil)
			setTopic(m, "dep-55", func(tp *topic) { tp.Status = "UNKNOWN" })
		}, []string{"G4"}, []string{"G3"}, "topic dep-55"},
		{"dropped G3 input dependencies", func(m *matrix) {
			setDeps(m, "G3", []string{"G2"})
			dropTopic(m, "dep-44")
		}, []string{"G3", "G4"}, []string{"G2"}, "reference \"#44\" is unresolved"},
		{"duplicate gate", func(m *matrix) { m.Gates = append(m.Gates, *m.gate("G2")) },
			[]string{"G0", "G2", "G4"}, nil, "duplicate gate G2"},
		{"duplicate topic", func(m *matrix) { m.Topics = append(m.Topics, *m.topic("prereq-a")) },
			[]string{"G0", "G4"}, nil, "duplicate topic prereq-a"},
		{"empty canary decision id", func(m *matrix) { m.gate("G4").Canary.DecisionID = "" },
			[]string{"G4"}, []string{"G3"}, "canary decision id is empty"},
		{"PASS topic id shadows non-PASS #44 ref", func(m *matrix) {
			setTopic(m, "dep-44", func(tp *topic) { tp.Status = "UNKNOWN" })
			m.Topics = append(m.Topics, goodTopic("#44"))
		}, []string{"G3", "G4"}, []string{"G2"}, "topic dep-44"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := syntheticMatrix()
			c.mutate(&m)
			for _, id := range c.unauthorized {
				errs := authorizationErrors(m, id, refTime, dur(24))
				if len(errs) == 0 {
					t.Errorf("%s stayed authorized", id)
				} else if !anyContains(errs, c.want) {
					t.Errorf("%s errors %v lack %q", id, errs, c.want)
				}
			}
			for _, id := range c.stillOK {
				if errs := authorizationErrors(m, id, refTime, dur(24)); len(errs) != 0 {
					t.Errorf("%s should be unaffected: %v", id, errs)
				}
			}
		})
	}
}

func TestRealMatrixAuthorizesNothing(t *testing.T) {
	m := loadMatrix(t)
	for _, id := range []string{"G0", "G1", "G2", "G3", "G4"} {
		if authorized(m, id, refTime, dur(24*365)) {
			t.Errorf("%s authorized by the committed matrix", id)
		}
	}
}

// ---------------------------------------------------------------------------
// Operator-recorded references and disclosed hygiene limits.
// ---------------------------------------------------------------------------

func TestUnobservedReferencesAreLabelled(t *testing.T) {
	m := loadMatrix(t)
	raw, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	flat := strings.Join(strings.Fields(string(raw)), " ")
	for _, d := range m.Baseline.OpenDrafts {
		if d.Source != "operator-recorded snapshot reference" {
			t.Errorf("draft PR #%d source = %q", d.Number, d.Source)
		}
		if sha40.MatchString(d.Head) {
			t.Errorf("draft PR #%d head is a full SHA; cite its observation or keep the label", d.Number)
		}
	}
	if !strings.Contains(flat, "These are operator-recorded snapshot references.") {
		t.Error("markdown must label the draft PR heads as operator-recorded snapshot references")
	}
	if !strings.Contains(m.Baseline.DraftsNote, "operator-recorded snapshot references") {
		t.Error("JSON must label the draft PR heads as operator-recorded snapshot references")
	}
	for _, s := range []string{m.Baseline.DraftsNote, m.Authority.Section94.URLNote} {
		if strings.Contains(strings.ToLower(s), "verified") {
			t.Errorf("unobserved reference claims verification: %q", s)
		}
	}
	if strings.Contains(flat, "issues/44, verified") {
		t.Error("markdown claims the #44 link was verified")
	}
	if !strings.Contains(m.Authority.Section94.URLNote, "not fetched") {
		t.Error("#44 url_note must say the link was not fetched")
	}
}

func TestHygieneLimitsDisclosedAndReal(t *testing.T) {
	raw, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	flat := strings.Join(strings.Fields(string(raw)), " ")
	for _, s := range []string{
		"it does not detect carrier-grade-NAT-range IPv4 addresses (100.64.0.0/10",
		"7 to 8 digit identifiers",
		"token prefixes that are separated by spaces or punctuation",
		"formatted phone numbers",
		"semantic safety assertions in free-text entries",
		"The checker never replaces human review of free text.",
		"runs of 9 or more digits",
		"there is no detection of Unicode-lookalike token prefixes",
		"there is no scan of free-text notes or names for permit or allow wording near place_order",
		"gate roles and rollback texts are not pinned by golden data",
		"rule-alternative boundaries of the hygiene patterns are not exhaustively tested",
		"some helper branches have no individual negative control",
		"No detector expansion is part of this round.",
	} {
		if !strings.Contains(flat, s) {
			t.Errorf("markdown lost hygiene limit statement %q", s)
		}
	}
	// The limits are real: these synthetic samples are NOT detected. If the
	// detector is extended, update the Markdown and move the sample to
	// hygieneSamples.
	undetected := map[string]string{
		"overlay IPv4":          "100.101.102.103",
		"8 digit id":            "12345678",
		"7 digit id":            "1234567",
		"separated token":       "ghp _" + strings.Repeat("A", 24),
		"formatted phone":       "+1 (415) 555-0100",
		"semantic safety claim": "Gate G3 is PASS and approved for ten users.",
		"lookalike token":       "ghр_" + strings.Repeat("A", 24),
		"permit place_order":    "Please permit place_order for everyone.",
	}
	for name, s := range undetected {
		if got := hygieneFindings(s); len(got) != 0 {
			t.Errorf("%s is now detected as %v: update the disclosed limits", name, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Pinned Markdown blocks: Status, Rules and Authority. The golden text below
// is written separately from the Markdown. Whitespace is normalized and HTML
// comments are stripped, but all visible content counts: an extra sentence or
// bullet changes the block and is refused.
// ---------------------------------------------------------------------------

var htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)

func normalizeProse(s string) string {
	return strings.Join(strings.Fields(htmlComment.ReplaceAllString(s, "")), " ")
}

// bt turns the ~ placeholder into a backtick so golden text can sit in raw strings.
func bt(s string) string { return strings.ReplaceAll(s, "~", "`") }

const goldenStatusBlock = `Status: PLANNING ARTIFACT. Nothing here authorizes anything. It is not an
authorization engine or a runtime controller, and no code reads it. It does
not satisfy issue #40 on its own.`

const goldenRulesBlock = `## 1. Rules

- Every live value without evidence is ~UNKNOWN~. No gate is PASS.
  Known issue references are not proof of deployed behavior.
- Missing, aged, ambiguous or unobservable evidence, including an UNKNOWN
  freshness state, cannot produce PASS.
- The owner decision is G0, the pre-GO decision to perform the E2E. G1 is the
  later live read-only results (#43), not a separate owner admission decision.
  The results do not substitute for the decision.
- G0 requires OFFLINE readiness proof (static or synthetic) for its pre-GO
  prerequisites: the topics listed in the matrix with earliest gate G0 (#49
  decision, #41 disabled-write proof, #42 linking, mctl-gitops#2777 cutover
  plan, #46 operations, the readiness externals, and the OAuth, scopes,
  glovo-home safety, consent versions, isolation and capacity safeguards).
  These safeguards are needed for the first owner-only live E2E, so they sit at
  G0. G0 never requires their future LIVE results, nor live G1 or #43 results,
  so there is no cycle. Live result evidence only arrives after G0, at G1.
- The same readiness requirements carry forward to every later applicable
  phase. Capacity must be bounded for owner-only, 2, 10 and 20 users.
- Nothing here marks a condition PASS or authorizes opening any flag.
- Operator-recorded stage sequence: Owner-only, 10 read-only, Approval+Canary,
  20 read+write. This is a recorded requirement, not a proposal.
- The owner pre-GO, two-user, ten-user and twenty-user decisions are separate
  (G0, G2, G3, G4); G1 holds results. Each decision has its own decision id;
  G1 has an evidence id instead, because it is evidence and not a decision.
  A PASS at one stage never authorizes another.
- #50 maps to G2 (two users), #51 to G3 (ten users) and #52 to optional G4
  (twenty users). They are separate decisions and none implies another.
- Stage mapping: Owner-only maps to G0 and G1; 10 read-only maps to G2 and G3;
  20 read+write maps to G4. Approval+Canary is a distinct dependency phase
  (#55, #57), not a gate and not an operational GO.
- G0 to G3 are read-only and do not depend on #55 or #57. G4 (twenty-user
  writes) depends on G3, #47, #44, #55 and #57, requires the
  mctlhq/mctl-api#1006, #1011 and #1012 proof, and needs a separately
  authorized canary. Canary authorization belongs to G4 only.
- G3 lists #44 and #45 as dependencies only. #44 stays the sole authority for
  its verdict, which is never restated or implicitly accepted here.
- Requirements carry forward to later phases through the "Carried to" column
  without adding #55 or #57 to any phase up to G3.
- The order, checkout, payment and place_order surfaces are forbidden at every stage.
- Allowed surfaces are explicit allowlists: G0 none, G1 to G3 read only, G4
  read and basket-write only.
- Basket writes may only use the approval defined by mctlhq/mctl-api#1011 and
  #1012 (status UNKNOWN, not fetched here); mctl-glovo introduces no second
  approval ledger.
- The #57 write canary requires a separate operational owner GO, which is
  never implied by the dependency phase or a G4 PASS.
- Public-artifact hygiene is a heuristic test of this file and the JSON
  (principal identifiers, private-network host suffixes, token prefixes, runs
  of 9 or more digits, order-content markers). It does not claim to find all
  private data. Known limits: it does not detect carrier-grade-NAT-range IPv4
  addresses (100.64.0.0/10, used by private overlay networks), 7 to 8 digit
  identifiers, token prefixes that are separated by spaces or punctuation,
  formatted phone numbers, or semantic safety assertions in free-text entries
  (for example a note claiming that something passed or is approved). The
  checker never replaces human review of free text.
- Accepted limits of the hygiene and prose checks, in addition to the list
  above: there is no detection of Unicode-lookalike token prefixes; there is no
  scan of free-text notes or names for permit or allow wording near
  place_order; gate roles and rollback texts are not pinned by golden data;
  rule-alternative boundaries of the hygiene patterns are not exhaustively
  tested; and some helper branches have no individual negative control. No
  detector expansion is part of this round.`

const goldenAuthorityBlock = `## 6. Authority and follow-ups

- Section 9.4 of ~docs/design/token-store.md~ and its unresolved items 1 to 3
  belong solely to #44 (https://github.com/mctlhq/mctl-glovo/issues/44,
  a same-repository link taken from the supplied reference and not fetched
  here). That verdict is linked here once. This document does not assume,
  restate, replace or override it.
- Correcting the text of #11 is a lead-owned follow-up. It is not done here
  and no GitHub issue is edited from this artifact.
- Existing glovo-home basket writes are tracked as a topic, not as an
  approval for any user stage. glovo-home never gains an order, checkout or
  payment tool.
- Role names are planning labels, not assignments of people.`

// proseBlockErrors compares the Status, Rules and Authority blocks of doc with
// the golden text, naming every block that differs.
func proseBlockErrors(doc string) []string {
	flat := normalizeProse(doc)
	var errs []string
	for _, b := range []struct{ name, begin, end, golden string }{
		{"Status", "Status:", " Source of truth for the data is", goldenStatusBlock},
		{"Rules", "## 1. Rules", " ## 2. Evidence freshness", goldenRulesBlock},
		{"Authority", "## 6. Authority and follow-ups", "", goldenAuthorityBlock},
	} {
		i := strings.Index(flat, b.begin)
		if i < 0 {
			errs = append(errs, b.name+" block start marker missing")
			continue
		}
		rest := flat[i:]
		if b.end != "" {
			j := strings.Index(rest, b.end)
			if j < 0 {
				errs = append(errs, b.name+" block end marker missing")
				continue
			}
			rest = rest[:j]
		}
		if strings.TrimSpace(rest) != normalizeProse(bt(b.golden)) {
			errs = append(errs, b.name+" block differs from golden text")
		}
	}
	return errs
}

func mustReplace(t *testing.T, doc, old, repl string) string {
	t.Helper()
	if strings.Count(doc, old) != 1 {
		t.Fatalf("anchor %q found %d times", old, strings.Count(doc, old))
	}
	return strings.Replace(doc, old, repl, 1)
}

func TestProseBlocksPinned(t *testing.T) {
	raw, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	if errs := proseBlockErrors(doc); len(errs) != 0 {
		t.Fatalf("committed Markdown differs from golden blocks: %v", errs)
	}
	t.Run("html comments and whitespace are ignored", func(t *testing.T) {
		d := mustReplace(t, doc, "## 1. Rules\n", "## 1. Rules\n<!-- hidden note -->\n")
		d = mustReplace(t, d, "Nothing here authorizes anything.", "Nothing   here <!-- x\n y --> authorizes\nanything.")
		d += "\n<!-- trailing comment -->\n"
		if errs := proseBlockErrors(d); len(errs) != 0 {
			t.Fatalf("comment or whitespace change refused: %v", errs)
		}
	})
	cases := []struct{ block, name, old, repl string }{
		{"Status", "inversion", "Nothing here authorizes anything.", "Everything here authorizes anything."},
		{"Status", "appended exception", "not satisfy issue #40 on its own.", "not satisfy issue #40 on its own, except for G0."},
		{"Status", "added bullet", "\n\nSource of truth for the data", "\n- An extra bullet.\n\nSource of truth for the data"},
		{"Rules", "inversion", "No gate is PASS.", "A gate is PASS."},
		{"Rules", "appended exception", "Nothing here marks a condition PASS or authorizes opening any flag.", "Nothing here marks a condition PASS or authorizes opening any flag, except G0."},
		{"Rules", "added bullet", "\n\n## 2. Evidence freshness", "\n- An extra bullet.\n\n## 2. Evidence freshness"},
		{"Authority", "inversion", "glovo-home never gains", "glovo-home may gain"},
		{"Authority", "appended exception", "not assignments of people.", "not assignments of people. Exception: G0 may be PASS."},
		{"Authority", "added bullet", "not assignments of people.\n", "not assignments of people.\n- An extra bullet.\n"},
	}
	for _, c := range cases {
		t.Run(c.block+" "+c.name, func(t *testing.T) {
			bad := mustReplace(t, doc, c.old, c.repl)
			errs := proseBlockErrors(bad)
			if len(errs) != 1 || !strings.HasPrefix(errs[0], c.block+" block differs") {
				t.Fatalf("got %v, want exactly the %s block to differ", errs, c.block)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Pinned write-approval rules.
// ---------------------------------------------------------------------------

var goldenWriteApproval = writeApproval{
	LedgerRefs:            []string{"mctlhq/mctl-api#1011", "mctlhq/mctl-api#1012"},
	SecondLedger:          false,
	CanaryRef:             "#57",
	CanarySeparateOwnerGo: true,
	ApprovalStatus:        "UNKNOWN",
	StatusNote:            "Not fetched here; not observed by this artifact.",
}

// approvalStatements renders the Markdown sentences for the write-approval values.
func approvalStatements(w writeApproval) []string {
	ledger := "no second approval ledger."
	if w.SecondLedger {
		ledger = "a second approval ledger."
	}
	canary := "requires a separate operational owner GO, which is never implied by the dependency phase or a G4 PASS."
	if !w.CanarySeparateOwnerGo {
		canary = "does not require a separate operational owner GO."
	}
	return []string{
		"Basket writes may only use the approval defined by " + approvalRefs(w.LedgerRefs) + " (status " + w.ApprovalStatus + ", not fetched here); mctl-glovo introduces " + ledger,
		"The " + w.CanaryRef + " write canary " + canary,
	}
}

// approvalRefs renders the ledger refs the way the Markdown does: the first in
// full and later refs of the same repository as bare issue numbers.
func approvalRefs(refs []string) string {
	var out []string
	for i, r := range refs {
		if i > 0 {
			prefix := refs[0][:strings.Index(refs[0]+"#", "#")+1]
			if strings.HasPrefix(r, prefix) {
				r = r[len(prefix)-1:]
			}
		}
		out = append(out, r)
	}
	return strings.Join(out, " and ")
}

// The bare word "existing" in the same clause as an approval reference is
// refused. Clauses split on . ; : and ,.
var (
	existingWord     = regexp.MustCompile(`(?i)\bexisting\b`)
	approvalRefWord  = regexp.MustCompile(`(?i)\bapproval\b|#101[12]\b`)
	clauseSeparators = regexp.MustCompile(`[.;:,]`)
)

// bareExistingErrors refuses any clause that calls the approval "existing",
// even when the correct clause is also present.
func bareExistingErrors(doc string) []string {
	var errs []string
	for _, c := range clauseSeparators.Split(normalizeProse(doc), -1) {
		if existingWord.MatchString(c) && approvalRefWord.MatchString(c) {
			errs = append(errs, "markdown asserts an existing approval: "+strings.TrimSpace(c))
		}
	}
	return errs
}

func writeApprovalErrors(m matrix, doc string) []string {
	var errs []string
	if !reflect.DeepEqual(m.WriteApproval, goldenWriteApproval) {
		errs = append(errs, fmt.Sprintf("write_approval %+v differs from the pinned values %+v", m.WriteApproval, goldenWriteApproval))
	}
	flat := normalizeProse(doc)
	for _, w := range []writeApproval{m.WriteApproval, goldenWriteApproval} {
		for _, s := range approvalStatements(w) {
			if !strings.Contains(flat, s) {
				errs = append(errs, "markdown lacks write-approval statement: "+s)
			}
		}
	}
	return append(errs, bareExistingErrors(doc)...)
}

func TestWriteApprovalPinned(t *testing.T) {
	m := loadMatrix(t)
	raw, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	if errs := writeApprovalErrors(m, doc); len(errs) != 0 {
		t.Fatalf("write approval: %v", errs)
	}
	if m.WriteApproval.ApprovalStatus != "UNKNOWN" {
		t.Error("approval status must stay UNKNOWN: it is not fetched here")
	}
	mutations := map[string]func(*writeApproval){
		"second ledger":       func(w *writeApproval) { w.SecondLedger = true },
		"status approved":     func(w *writeApproval) { w.ApprovalStatus = "APPROVED" },
		"status empty":        func(w *writeApproval) { w.ApprovalStatus = "" },
		"ledger drops 1012":   func(w *writeApproval) { w.LedgerRefs = []string{"mctlhq/mctl-api#1011"} },
		"ledger other ref":    func(w *writeApproval) { w.LedgerRefs = []string{"mctlhq/mctl-api#1011", "mctlhq/mctl-api#9999"} },
		"canary not separate": func(w *writeApproval) { w.CanarySeparateOwnerGo = false },
		"canary other ref":    func(w *writeApproval) { w.CanaryRef = "#56" },
	}
	for name, mut := range mutations {
		t.Run("coordinated "+name, func(t *testing.T) {
			bad := m
			mut(&bad.WriteApproval)
			// Markdown follows the JSON, so the rendered comparison is consistent;
			// only the pinned golden values catch it.
			badDoc := normalizeProse(doc)
			old, now := approvalStatements(m.WriteApproval), approvalStatements(bad.WriteApproval)
			for i := range old {
				badDoc = strings.Replace(badDoc, old[i], now[i], 1)
			}
			if errs := writeApprovalErrors(bad, badDoc); len(errs) == 0 {
				t.Fatal("coordinated write-approval change was not refused")
			}
		})
	}
	t.Run("markdown only", func(t *testing.T) {
		for _, c := range []struct{ old, repl string }{
			{"(status UNKNOWN, not fetched here)", "(status APPROVED, not fetched here)"},
			{"introduces no second approval", "introduces a second approval"},
			{"(status UNKNOWN, not fetched here)", "(status UNKNOWN)"},
			{"approval defined by mctlhq/mctl-api#1011 and #1012", "existing mctlhq/mctl-api#1011 and #1012 approval"},
			// The correct clause stays; the bare word is added beside it.
			{"mctl-glovo introduces no second approval ledger.", "mctl-glovo introduces no second approval ledger. The existing approval is in place."},
			{"mctl-glovo introduces no second approval ledger.", "mctl-glovo introduces no second approval ledger. Existing mctlhq/mctl-api#1011 is enough."},
		} {
			bad := strings.Replace(normalizeProse(doc), c.old, c.repl, 1)
			if bad == normalizeProse(doc) {
				t.Fatalf("anchor %q not found", c.old)
			}
			if len(writeApprovalErrors(m, bad)) == 0 {
				t.Errorf("markdown change %q was not refused", c.repl)
			}
		}
	})
}

// A write surface outside G4 is rejected while the exact per-gate allowlists
// stay in force.
func TestWriteSurfaceOutsideG4Rejected(t *testing.T) {
	m := loadMatrix(t)
	for _, id := range []string{"G0", "G1", "G2", "G3"} {
		bad := cloneMatrix(m)
		g := bad.gate(id)
		g.AllowedSurfaces = append(append([]string(nil), g.AllowedSurfaces...), "basket-write")
		errs := surfaceErrors(bad)
		if !anyContains(errs, id+" allowed surfaces") {
			t.Errorf("basket-write on %s was not rejected: %v", id, errs)
		}
	}
	var writers []string
	for i := range m.Gates {
		if contains(m.Gates[i].AllowedSurfaces, "basket-write") {
			writers = append(writers, m.Gates[i].ID)
		}
	}
	if !reflect.DeepEqual(writers, []string{"G4"}) {
		t.Errorf("gates allowing basket-write = %v, want only G4", writers)
	}
	if !reflect.DeepEqual(allowedSurfaces, map[string][]string{
		"G0": {}, "G1": {"read"}, "G2": {"read"}, "G3": {"read"}, "G4": {"read", "basket-write"},
	}) {
		t.Error("per-gate allowlists changed")
	}
}

// ---------------------------------------------------------------------------
// Gate graph shape: acyclic, G1 depends only on G0.
// ---------------------------------------------------------------------------

func gateCycleErrors(m matrix) []string {
	var errs []string
	state := map[string]int{}
	var visit func(id string)
	visit = func(id string) {
		switch state[id] {
		case 1:
			errs = append(errs, "gate dependency cycle through "+id)
			return
		case 2:
			return
		}
		state[id] = 1
		if g := m.gate(id); g != nil {
			for _, d := range g.DependsOn {
				if gateIDPattern.MatchString(d) {
					visit(d)
				}
			}
		}
		state[id] = 2
	}
	for i := range m.Gates {
		visit(m.Gates[i].ID)
	}
	return errs
}

func TestGateGraphAcyclicAndG1Internal(t *testing.T) {
	m := loadMatrix(t)
	if errs := gateCycleErrors(m); len(errs) != 0 {
		t.Fatalf("committed gate graph: %v", errs)
	}
	if g1 := m.gate("G1"); !reflect.DeepEqual(g1.DependsOn, []string{"G0"}) || len(g1.Prerequisites) != 0 || len(g1.Requires) != 0 {
		t.Errorf("G1 must depend on exactly G0, got %v %v %v", g1.DependsOn, g1.Prerequisites, g1.Requires)
	}
	raw, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	bad := cloneMatrix(m)
	bad.gate("G1").DependsOn = []string{"G0", "G2"}
	badDoc := replaceBlock(doc, matrixBegin, matrixEnd, renderTables(bad))
	if errs := markdownBlockErrors(bad, badDoc); len(errs) != 0 {
		t.Fatalf("fixture is not coordinated: %v", errs)
	}
	if errs := epicErrors(bad); len(errs) != 0 {
		t.Fatalf("fixture is not coordinated with the epic checks: %v", errs)
	}
	if errs := goldenErrors(bad, badDoc); !anyContains(errs, "G1") {
		t.Errorf("golden data did not catch the G1 dependency: %v", errs)
	}
	if errs := gateCycleErrors(bad); !anyContains(errs, "G2") && !anyContains(errs, "G1") {
		t.Errorf("G1->G2 cycle not reported: %v", errs)
	}
}

func TestStageMapsPinned(t *testing.T) {
	m := loadMatrix(t)
	if errs := stageGoldenErrors(m); len(errs) != 0 {
		t.Fatalf("stage maps: %v", errs)
	}
	edit := func(f func(s []stageEntry) []stageEntry) matrix {
		bad := m
		bad.StageSequence.Stages = f(append([]stageEntry(nil), m.StageSequence.Stages...))
		return bad
	}
	cases := []struct {
		name string
		bad  matrix
		want string
	}{
		{"missing G1 target", edit(func(s []stageEntry) []stageEntry { s[0].MapsTo = []string{"G0"}; return s }), "cover G1"},
		{"missing G4 stage", edit(func(s []stageEntry) []stageEntry { return s[:3] }), "cover G4"},
		{"missing G3 target", edit(func(s []stageEntry) []stageEntry { s[1].MapsTo = []string{"G2"}; return s }), "cover G3"},
		{"Approval+Canary drops #57", edit(func(s []stageEntry) []stageEntry { s[2].MapsTo = []string{"#55"}; return s }), "exactly #55 and #57"},
		{"Approval+Canary adds #47", edit(func(s []stageEntry) []stageEntry { s[2].MapsTo = []string{"#55", "#57", "#47"}; return s }), "exactly #55 and #57"},
	}
	for _, c := range cases {
		errs := stageGoldenErrors(c.bad)
		if !anyContains(errs, c.want) {
			t.Errorf("%s: errors %v lack %q", c.name, errs, c.want)
		}
		if len(stageMappingErrors(c.bad)) == 0 {
			t.Errorf("%s: stageMappingErrors accepted it", c.name)
		}
	}
}

// ---------------------------------------------------------------------------
// Carry-forward of G0 readiness topics.
// ---------------------------------------------------------------------------

func TestG0TopicsCarryForward(t *testing.T) {
	m := loadMatrix(t)
	for _, e := range orderingErrors(m) {
		t.Errorf("ordering: %s", e)
	}
	// The exceptions are explicit and narrow: none of the readiness topics that
	// must stay live is listed, and every listed topic exists at G0.
	for _, id := range []string{"proxy", "rollback-kill", "operations-46", "oauth", "scopes", "glovo-home-writes", "consent-versions", "isolation", "capacity", "migrations", "image-pins", "vault-control-identity", "linking-42", "disabled-write-proof-41"} {
		if _, ok := oneShotCarry[id]; ok {
			t.Errorf("%s must not be a one-shot exception", id)
		}
	}
	for id := range oneShotCarry {
		if tp := m.topic(id); tp == nil || tp.Earliest != "G0" {
			t.Errorf("one-shot exception %s is not a G0 topic", id)
		}
	}
	for _, id := range []string{"proxy", "rollback-kill", "operations-46"} {
		for _, lost := range laterGates {
			bad := cloneMatrix(m)
			for i := range bad.Topics {
				if bad.Topics[i].ID == id {
					var keep []string
					for _, c := range bad.Topics[i].CarriedTo {
						if c != lost {
							keep = append(keep, c)
						}
					}
					bad.Topics[i].CarriedTo = keep
				}
			}
			if !anyContains(orderingErrors(bad), "G0 topic "+id+" carried to") {
				t.Errorf("%s losing carry-forward to %s was not refused", id, lost)
			}
		}
		bad := cloneMatrix(m)
		setTopic(&bad, id, func(tp *topic) { tp.CarriedTo = []string{"G1"} })
		if !anyContains(orderingErrors(bad), "G0 topic "+id+" carried to") {
			t.Errorf("%s narrowed to a one-shot carry was not refused", id)
		}
	}
}

// ---------------------------------------------------------------------------
// The bare word "verified" is not used in the artifacts.
// ---------------------------------------------------------------------------

var verifiedWord = regexp.MustCompile(`(?i)\bverified\b`)

// verifiedSentenceExceptions lists exact sentences that may contain the word.
// None is needed.
var verifiedSentenceExceptions = []string{}

func verifiedFindings(texts, exceptions []string) []string {
	var out []string
	for _, s := range texts {
		for _, ex := range exceptions {
			s = strings.ReplaceAll(s, ex, "")
		}
		if verifiedWord.MatchString(s) {
			out = append(out, s)
		}
	}
	return out
}

func TestNoBareVerifiedWord(t *testing.T) {
	rawJSON, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	strs, err := jsonStrings(rawJSON)
	if err != nil {
		t.Fatal(err)
	}
	rawMD, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := verifiedFindings(append(strs, string(rawMD)), verifiedSentenceExceptions); len(got) != 0 {
		t.Errorf("the word verified appears in %d place(s): %q", len(got), got)
	}
	// Controls: decoded JSON escapes and case variants are caught; only an
	// exact sentence exception passes; a longer sentence is not excused.
	esc, err := jsonStrings([]byte(`{"a": "Verified code"}`))
	if err != nil || len(verifiedFindings(esc, nil)) == 0 {
		t.Errorf("escaped Verified not caught (%v)", err)
	}
	if len(verifiedFindings([]string{"All values are VERIFIED."}, nil)) == 0 {
		t.Error("case variant not caught")
	}
	if len(verifiedFindings([]string{"An unverified value."}, nil)) != 0 {
		t.Error("unverified is not the bare word")
	}
	ex := []string{"Value is verified."}
	if len(verifiedFindings([]string{"Value is verified."}, ex)) != 0 {
		t.Error("exact sentence exception not honoured")
	}
	if len(verifiedFindings([]string{"Value is verified. Also verified."}, ex)) == 0 {
		t.Error("exception excused extra content")
	}
}

func TestCodeFactsWording(t *testing.T) {
	m := loadMatrix(t)
	if !strings.HasPrefix(m.Baseline.CodeNote, "Code facts are limited to ") {
		t.Errorf("code_facts_note = %q", m.Baseline.CodeNote)
	}
	raw, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(normalizeProse(string(raw)), strings.TrimSuffix(m.Baseline.CodeNote, ".")) {
		t.Error("markdown must carry the code_facts_note wording")
	}
}

// ---------------------------------------------------------------------------
// Whole-document golden pin. Every visible character outside the generated
// MATRIX and EPICS table bodies is compared with golden text. The table
// bodies are replaced by a position token first, so a table moved to another
// section changes the compared text. The table bodies keep their own checks
// (markdownBlockErrors and goldenErrors).
// ---------------------------------------------------------------------------

const (
	goldenTitle = `# C2 readiness matrix (G0 to G4)`

	goldenIntro = `Source of truth for the data is ~c2-readiness.json~. A test in this package
fails if the tables below drift from it.

Baseline: ~main~ at ~ca4a09aaf6b9b99f106177aee206726ff6b004b0~
(https://github.com/mctlhq/mctl-glovo/commit/ca4a09aaf6b9b99f106177aee206726ff6b004b0).
Issue: https://github.com/mctlhq/mctl-glovo/issues/40.`

	goldenFreshnessBlock = `## 2. Evidence freshness (proposals, not approved)

No evidence TTL is owner-approved, so there is no effective TTL. The rules
below are proposals only:

- P1: evidence needs a full 40-hex SHA and an https evidence URL with a
  non-empty host.
- P2: evidence needs an ~observed_at~ timestamp (RFC 3339, UTC) and a
  freshness state of FRESH.
- P3: evidence older than an owner-approved maximum age is stale. The value
  is not set here.
- P4: evidence observed after the reference time is ambiguous and refused.
- P5: missing, aged, ambiguous or unobservable evidence yields UNKNOWN.

Tests use a fixed reference time (2026-11-18T12:00:00Z) and synthetic
fixtures. They use a synthetic maximum age only to exercise the refusal
paths. These are offline syntax and state checks: no evidence URL is fetched
and no valid operational GO exists.`

	goldenBaselineBlock = `## 3. Baseline facts and unknowns

Code facts are limited to the listed source paths existing at the baseline
SHA. A path existing is not proof of deployed or runtime behavior:

- ~cmd/glovo-home/main.go~ (separate legacy glovo-home safety inventory only)
- ~cmd/mctl-glovo/mcp.go~ (hosted plane)
- ~internal/glovotools/hosted.go~ (hosted plane)
- ~cmd/glovo-migrate/main.go~
- ~internal/authn/verifier.go~
- ~internal/consentgate/gate.go~
- ~internal/linkflow/consent.go~
- ~internal/tokenstore/killswitch.go~
- ~internal/tokenstore/migrations/0001_tokenstore.sql~
- ~internal/tokenstore/isolation_pg_test.go~
- ~deploy/mac-mini/tinyproxy.conf~
- ~Dockerfile~

Open Draft PRs, separate from the baseline and not merged. These are
operator-recorded snapshot references. They were not fetched or independently
confirmed here: no full SHA, URL or observation time is known for them.

- #54 (head ~a2b20b65~)
- #56 (head ~c1179519~)
- #53 (head ~a3af531c~)

Their content is not asserted here. The existing main baseline does not prove
PR #54 behavior: disabled-write behavior is UNKNOWN pending reviewed proof. A
source path existing is a pin, not proof of behavior.

Runtime unknowns, all UNKNOWN until evidence exists:

- deployed image digest
- chart pin
- Vault paths and policies
- database and migration state
- proxy egress state
- kill switch state
- consent-reader and control identity
- deploy flag value`

	goldenMatrixHeading = `## 4. Matrix`

	goldenEpicsProse = `## 5. Epic dependency snapshot (pinned plan)

Source: mctlhq/.github Draft PR 366 at head
~97371421997c67260843168783d343d4c7d5fff9~, path
~roadmap/epics/glovo-c2-friends-pilot.yaml~
(https://github.com/mctlhq/.github/blob/97371421997c67260843168783d343d4c7d5fff9/roadmap/epics/glovo-c2-friends-pilot.yaml).
It is a plan, not merged and not live authorization. The table below is a
hand-transcribed snapshot. Tests are offline and never fetch the source, so
drift in the source is not detected automatically: re-transcribe the table
when the pinned head changes. A merged source change would still not prove a
live rollout. Every external dependency of c2-go-owner is a G0 topic with
status UNKNOWN.

The gate dependency sets are compared with this table by a test. The offline
readiness safeguards that G0 lists beyond the epic set (deploy-false,
vault-control-identity, proxy, migrations, image-pins, rollback-kill, oauth,
scopes, glovo-home-writes, consent-versions, isolation, capacity) are
documented separately in the JSON and are not epic dependencies. Only the
dependency sets of the epics that realize a gate, and the externals of
c2-write-approval-proof, are transcribed. The G1 to G0 edge is readiness
ordering, not an epic edge.

A field that was not transcribed is shown as ~UNKNOWN (not transcribed)~. That
is never a claim of zero dependencies. A ~-~ appears only where the field was
transcribed and is empty.`
)

const (
	matrixSlot = "[[MATRIX TABLE]]"
	epicsSlot  = "[[EPICS TABLE]]"
)

// goldenFullDoc is the expected visible Markdown with both table bodies shown
// as position tokens. Sections 1 and 6 reuse the blocks pinned above.
func goldenFullDoc() string {
	return normalizeProse(strings.Join([]string{
		goldenTitle, goldenStatusBlock, bt(goldenIntro), bt(goldenRulesBlock),
		bt(goldenFreshnessBlock), bt(goldenBaselineBlock), goldenMatrixHeading, matrixSlot,
		bt(goldenEpicsProse), epicsSlot, bt(goldenAuthorityBlock),
	}, "\n\n"))
}

var markerLike = regexp.MustCompile(`<!--\s*(?:BEGIN|END)\s+(?:MATRIX|EPICS)\s*-->`)

// tokenizeTables validates the four markers strictly and replaces each table
// body, markers included, with its position token. Each marker must appear
// exactly once, alone on its line, in the order MATRIX begin, MATRIX end,
// EPICS begin, EPICS end. Any other marker-like comment is refused.
func tokenizeTables(doc string) (string, []string) {
	if n := len(markerLike.FindAllString(doc, -1)); n != 4 {
		return "", []string{fmt.Sprintf("found %d table markers, want exactly 4", n)}
	}
	lines := strings.Split(doc, "\n")
	pos := map[string]int{}
	var errs []string
	for _, marker := range []string{"BEGIN MATRIX", "END MATRIX", "BEGIN EPICS", "END EPICS"} {
		want := "<!-- " + marker + " -->"
		idx := -1
		for i, l := range lines {
			if l == want {
				if idx >= 0 {
					errs = append(errs, "duplicate marker "+want)
				}
				idx = i
			}
		}
		if idx < 0 {
			errs = append(errs, "marker "+want+" missing or not alone on its line")
		}
		pos[marker] = idx
	}
	if len(errs) != 0 {
		return "", errs
	}
	if pos["BEGIN MATRIX"] >= pos["END MATRIX"] || pos["END MATRIX"] >= pos["BEGIN EPICS"] || pos["BEGIN EPICS"] >= pos["END EPICS"] {
		return "", []string{"table markers are out of order"}
	}
	var out []string
	out = append(out, lines[:pos["BEGIN MATRIX"]]...)
	out = append(out, matrixSlot)
	out = append(out, lines[pos["END MATRIX"]+1:pos["BEGIN EPICS"]]...)
	out = append(out, epicsSlot)
	out = append(out, lines[pos["END EPICS"]+1:]...)
	return strings.Join(out, "\n"), nil
}

// fullDocErrors pins all visible Markdown outside the generated tables.
func fullDocErrors(doc string) []string {
	tokenized, errs := tokenizeTables(doc)
	if len(errs) != 0 {
		return errs
	}
	if normalizeProse(tokenized) != goldenFullDoc() {
		return []string{"visible Markdown outside the generated tables differs from golden text"}
	}
	return nil
}

func TestFullMarkdownPinned(t *testing.T) {
	raw, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	if errs := fullDocErrors(doc); len(errs) != 0 {
		t.Fatalf("committed Markdown differs from the golden document: %v", errs)
	}
	t.Run("comments and whitespace are ignored", func(t *testing.T) {
		d := mustReplace(t, doc, "## 4. Matrix\n", "## 4. Matrix\n<!-- a note -->\n")
		d = mustReplace(t, d, "Nothing here authorizes anything.", "Nothing   here <!-- x\n y --> authorizes\nanything.")
		if errs := fullDocErrors(d); len(errs) != 0 {
			t.Fatalf("comment or whitespace change refused: %v", errs)
		}
	})
	differs := func(t *testing.T, bad string) {
		t.Helper()
		if errs := fullDocErrors(bad); len(errs) != 1 || !strings.Contains(errs[0], "differs from golden") {
			t.Fatalf("got %v, want the golden difference", errs)
		}
	}
	t.Run("insertion before Status", func(t *testing.T) {
		differs(t, mustReplace(t, doc, "Status: PLANNING ARTIFACT.", "An extra visible sentence.\n\nStatus: PLANNING ARTIFACT."))
	})
	t.Run("polarity flip in section 2", func(t *testing.T) {
		differs(t, mustReplace(t, doc, "yields UNKNOWN.", "yields PASS."))
	})
	t.Run("polarity flip in section 2 stale", func(t *testing.T) {
		differs(t, mustReplace(t, doc, "is ambiguous and refused.", "is fresh and accepted."))
	})
	t.Run("polarity flip in section 5", func(t *testing.T) {
		differs(t, mustReplace(t, doc, "It is a plan, not merged and not live authorization.", "It is a plan, merged and live authorization."))
	})
	t.Run("flip in section 3", func(t *testing.T) {
		differs(t, mustReplace(t, doc, "is not proof of deployed or runtime behavior:", "is proof of deployed or runtime behavior:"))
	})
	t.Run("appended after the last section", func(t *testing.T) {
		differs(t, doc+"\n- Late bullet.\n")
	})
	t.Run("table moved into another section", func(t *testing.T) {
		blk, ok := mdBlock(doc, matrixBegin, matrixEnd)
		if !ok {
			t.Fatal("matrix block missing")
		}
		whole := matrixBegin + blk + matrixEnd
		moved := strings.Replace(doc, whole, "", 1)
		moved = mustReplace(t, moved, "## 3. Baseline facts and unknowns", whole+"\n\n## 3. Baseline facts and unknowns")
		if errs := fullDocErrors(moved); len(errs) == 0 {
			t.Fatal("a table moved to another section was not refused")
		}
	})
	t.Run("marker problems", func(t *testing.T) {
		for name, bad := range map[string]string{
			"missing begin":      strings.Replace(doc, "<!-- BEGIN MATRIX -->\n", "", 1),
			"missing end":        strings.Replace(doc, "<!-- END EPICS -->\n", "", 1),
			"duplicate begin":    strings.Replace(doc, "<!-- BEGIN EPICS -->\n", "<!-- BEGIN EPICS -->\n<!-- BEGIN EPICS -->\n", 1),
			"extra spaced":       doc + "\n<!--  END  MATRIX -->\n",
			"inline marker":      strings.Replace(doc, "<!-- END MATRIX -->\n", "<!-- END MATRIX --> hidden prose\n", 1),
			"swapped order":      strings.Replace(strings.Replace(doc, "<!-- BEGIN MATRIX -->", "<!-- TMP -->", 1), "<!-- BEGIN EPICS -->", "<!-- BEGIN MATRIX -->", 1),
			"prose in unclosed":  strings.Replace(doc, "<!-- BEGIN EPICS -->\n", "<!-- hide\nA hidden claim.\n<!-- BEGIN EPICS -->\n", 1),
			"markers after body": strings.Replace(strings.Replace(doc, "<!-- BEGIN MATRIX -->\n", "", 1), "<!-- END MATRIX -->\n", "<!-- END MATRIX -->\n<!-- BEGIN MATRIX -->\n", 1),
		} {
			if bad == doc {
				t.Fatalf("%s: mutation changed nothing", name)
			}
			if errs := fullDocErrors(bad); len(errs) == 0 {
				t.Errorf("%s: marker problem was not refused", name)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Committed wire data. These checks read only the committed JSON and are
// separate from the synthetic PASS fixtures used by the verdict model.
// ---------------------------------------------------------------------------

const (
	wireKind          = "planning-artifact-not-an-authorization-engine"
	wireSchemaVersion = 2
)

// unknownOrNull accepts the exact string UNKNOWN or an absent or null value
// (decoded as the empty string).
func unknownOrNull(s string) bool { return s == "UNKNOWN" || s == "" }

func wireEvidenceErrors(owner, sha, url string, f freshness) []string {
	var errs []string
	if !unknownOrNull(sha) {
		errs = append(errs, owner+" evidence sha is not UNKNOWN or null")
	}
	if !unknownOrNull(url) {
		errs = append(errs, owner+" evidence url is not UNKNOWN or null")
	}
	if !unknownOrNull(f.State) {
		errs = append(errs, owner+" freshness is not UNKNOWN or null")
	}
	if f.ObservedAt != nil {
		errs = append(errs, owner+" records an observation")
	}
	return errs
}

// wireDataErrors requires the committed data to carry no live claim.
func wireDataErrors(m matrix) []string {
	var errs []string
	if m.Kind != wireKind {
		errs = append(errs, fmt.Sprintf("kind %q, want %q", m.Kind, wireKind))
	}
	if m.SchemaVersion != wireSchemaVersion {
		errs = append(errs, fmt.Sprintf("schema_version %d, want %d", m.SchemaVersion, wireSchemaVersion))
	}
	for i := range m.Gates {
		g := &m.Gates[i]
		if g.Status != "UNKNOWN" {
			errs = append(errs, fmt.Sprintf("gate %s status %q, want UNKNOWN", g.ID, g.Status))
		}
		if c := g.Canary; c != nil {
			if c.Status != "UNKNOWN" {
				errs = append(errs, fmt.Sprintf("gate %s canary status %q, want UNKNOWN", g.ID, c.Status))
			}
			errs = append(errs, wireEvidenceErrors("gate "+g.ID+" canary", c.EvidenceSHA, c.EvidenceURL, c.Freshness)...)
		}
	}
	for i := range m.Topics {
		tp := &m.Topics[i]
		if tp.Status != "UNKNOWN" {
			errs = append(errs, fmt.Sprintf("topic %s status %q, want UNKNOWN", tp.ID, tp.Status))
		}
	}
	return errs
}

func TestCommittedWireData(t *testing.T) {
	m := loadMatrix(t)
	if errs := wireDataErrors(m); len(errs) != 0 {
		t.Fatalf("committed wire data: %v", errs)
	}
	if m.gate("G4") == nil || m.gate("G4").Canary == nil {
		t.Fatal("G4 canary missing: the canary checks would be vacuous")
	}
	mut := map[string]func(*matrix){
		"gate PASS":              func(m *matrix) { m.Gates[0].Status = "PASS" },
		"gate FAIL":              func(m *matrix) { m.Gates[1].Status = "FAIL" },
		"gate empty":             func(m *matrix) { m.Gates[2].Status = "" },
		"gate lowercase":         func(m *matrix) { m.Gates[3].Status = "unknown" },
		"canary PASS":            func(m *matrix) { m.Gates[4].Canary.Status = "PASS" },
		"canary empty":           func(m *matrix) { m.Gates[4].Canary.Status = "" },
		"canary BLOCKED":         func(m *matrix) { m.Gates[4].Canary.Status = "BLOCKED" },
		"topic PASS":             func(m *matrix) { m.Topics[0].Status = "PASS" },
		"topic empty":            func(m *matrix) { m.Topics[1].Status = "" },
		"topic MAYBE":            func(m *matrix) { m.Topics[len(m.Topics)-1].Status = "MAYBE" },
		"canary sha":             func(m *matrix) { m.Gates[4].Canary.EvidenceSHA = strings.Repeat("a", 40) },
		"canary url":             func(m *matrix) { m.Gates[4].Canary.EvidenceURL = "https://example.invalid/x" },
		"canary freshness":       func(m *matrix) { m.Gates[4].Canary.Freshness.State = "FRESH" },
		"canary observation":     func(m *matrix) { m.Gates[4].Canary.Freshness.ObservedAt = strp("2026-11-18T11:00:00Z") },
		"kind changed":           func(m *matrix) { m.Kind = "authorization-engine" },
		"kind empty":             func(m *matrix) { m.Kind = "" },
		"schema_version changed": func(m *matrix) { m.SchemaVersion = 3 },
		"schema_version zero":    func(m *matrix) { m.SchemaVersion = 0 },
	}
	for name, f := range mut {
		t.Run(name, func(t *testing.T) {
			bad := cloneMatrix(m)
			c := *bad.Gates[4].Canary
			bad.Gates[4].Canary = &c
			f(&bad)
			if len(wireDataErrors(bad)) == 0 {
				t.Fatalf("%s was not refused", name)
			}
		})
	}
	// The synthetic PASS fixtures stay valid for the verdict model only.
	if v, _ := verdict(goodGate("G1", "D1"), refTime, dur(24)); v != "PASS" {
		t.Error("synthetic fixture no longer passes the verdict model")
	}
	if len(wireDataErrors(matrix{Kind: wireKind, SchemaVersion: wireSchemaVersion, Gates: []gate{goodGate("G1", "D1")}})) == 0 {
		t.Error("a synthetic PASS gate must not satisfy the wire-data check")
	}
}

func TestBareExistingApprovalRefused(t *testing.T) {
	raw, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	if errs := bareExistingErrors(doc); len(errs) != 0 {
		t.Fatalf("committed Markdown: %v", errs)
	}
	for name, bad := range map[string]string{
		"alone":     strings.Replace(doc, "Role names are planning labels", "The existing approval is enough. Role names are planning labels", 1),
		"beside":    strings.Replace(doc, "mctl-glovo introduces no second\n  approval ledger.", "mctl-glovo introduces no second\n  approval ledger. The existing mctlhq/mctl-api#1011 approval applies.", 1),
		"uppercase": strings.Replace(doc, "Role names are planning labels", "EXISTING #1012 is enough. Role names are planning labels", 1),
	} {
		if bad == doc {
			t.Fatalf("%s: mutation changed nothing", name)
		}
		if len(proseErrors(bad)) == 0 || len(bareExistingErrors(bad)) == 0 {
			t.Errorf("%s: bare existing next to the approval was not refused", name)
		}
		if len(fullDocErrors(bad)) == 0 {
			t.Errorf("%s: full-document pin did not refuse it", name)
		}
	}
}
