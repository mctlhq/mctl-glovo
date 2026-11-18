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

type matrix struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	Issue         string `json:"issue"`
	Issue40Done   bool   `json:"issue_40_fully_satisfied"`
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
	for _, set := range [][]string{g.Prerequisites, g.DependsOn, g.Requires} {
		for _, ref := range set {
			if !c.ref(id, ref) {
				ok = false
			}
		}
	}
	switch {
	case g.Canary != nil:
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
	var found []topic
	for i := range c.m.Topics {
		if c.m.Topics[i].ID == ref {
			found = append(found, c.m.Topics[i])
		}
	}
	if len(found) == 0 && ref != "" && ref != "UNKNOWN" {
		for i := range c.m.Topics {
			if contains(c.m.Topics[i].Refs, ref) {
				found = append(found, c.m.Topics[i])
			}
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
	c.gate(id)
	return c.errs
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
	for _, id := range []string{"G0", "G2", "G3", "G4"} {
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
		"Basket writes may only use the existing mctlhq/mctl-api#1011 and mctlhq/mctl-api#1012 approval; mctl-glovo introduces no second approval ledger.",
		"Basket writes may use the existing mctlhq/mctl-api#1011 and mctlhq/mctl-api#1012 approval; mctl-glovo introduces a second approval ledger.",
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
	return errs
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
	}
	for name, s := range undetected {
		if got := hygieneFindings(s); len(got) != 0 {
			t.Errorf("%s is now detected as %v: update the disclosed limits", name, got)
		}
	}
}
