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
	DecisionID string `json:"decision_id"`
	Status     string `json:"status"`
	Note       string `json:"note"`
}

type gate struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Kind            string    `json:"kind"`
	Phase           string    `json:"phase"`
	Status          string    `json:"status"`
	Role            string    `json:"role"`
	DecisionID      string    `json:"decision_id"`
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
		Source string   `json:"source"`
		Order  []string `json:"order"`
	} `json:"stage_sequence"`
	Authority struct {
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

// authorized is the per-stage check: a stage is authorized by its own gate
// only. A PASS elsewhere never grants it.
func authorized(m matrix, id string, now time.Time, maxAge *time.Duration) bool {
	g := m.gate(id)
	if g == nil {
		return false
	}
	v, _ := verdict(*g, now, maxAge)
	return v == "PASS"
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
		g := m.Gates[i]
		if g.ID != id {
			t.Fatalf("gate %d is %s, want %s", i, g.ID, id)
		}
		add(g.ID, g.DecisionID)
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
	if strings.Join(m.StageSequence.Order, ",") != strings.Join(want, ",") {
		t.Errorf("stage sequence = %v, want %v", m.StageSequence.Order, want)
	}
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
	for _, tp := range m.Topics {
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
	for _, tp := range m.Topics {
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
	for _, id := range []string{"G0", "G1", "G2", "G3"} {
		g := m.gate(id)
		if g.WriteDependency {
			t.Errorf("%s is read-only but declares a write dependency", id)
		}
		for _, dep := range []string{"#55", "#57"} {
			if contains(g.DependsOn, dep) || contains(g.Requires, dep) || contains(g.Prerequisites, dep) || contains(g.Refs, dep) {
				t.Errorf("%s depends on %s", id, dep)
			}
		}
		for _, req := range []string{"#1011", "#1012"} {
			if contains(g.Requires, req) {
				t.Errorf("%s requires write proof %s", id, req)
			}
		}
		if contains(g.AllowedSurfaces, "basket-write") {
			t.Errorf("%s allows basket-write", id)
		}
	}
	// Topics carried into read-only phases must not smuggle in #55 or #57.
	for _, tp := range m.Topics {
		for _, dep := range []string{"#55", "#57"} {
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
	for _, req := range []string{"#1011", "#1012"} {
		if !contains(g4.Requires, req) {
			t.Errorf("G4 must require %s", req)
		}
	}
	if g4.Canary == nil || g4.Canary.DecisionID == "" {
		t.Error("G4 needs a separate canary authorization")
	}
}

func TestForbiddenSurfaces(t *testing.T) {
	m := loadMatrix(t)
	for _, s := range []string{"order", "checkout", "payment"} {
		if !contains(m.Forbidden, s) {
			t.Errorf("%s missing from forbidden_surfaces", s)
		}
	}
	for _, g := range m.Gates {
		for _, s := range g.AllowedSurfaces {
			if contains(m.Forbidden, s) || s == "order" || s == "checkout" || s == "payment" {
				t.Errorf("%s allows forbidden surface %s", g.ID, s)
			}
		}
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
	for _, g := range m.Gates {
		if g.Status == "PASS" {
			t.Errorf("%s is PASS in the planning artifact", g.ID)
		}
		// Even with a generous synthetic ttl the committed state must not pass.
		if v, _ := verdict(g, refTime, dur(24*365)); v == "PASS" {
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
	for _, g := range m.Gates {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			g.ID, g.Status, g.Role, g.Phase, list(g.DependsOn), list(g.Prerequisites),
			g.EvidenceSHA, g.EvidenceURL, g.Freshness.State, g.Rollback)
	}
	b.WriteString("\n| Topic | Role | Refs | Earliest gate | Carried to | Status | Evidence SHA | Evidence URL | Freshness | Rollback |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")
	for _, tp := range m.Topics {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			tp.Name, tp.Role, list(tp.Refs), tp.Earliest, list(tp.CarriedTo), tp.Status,
			tp.EvidenceSHA, tp.EvidenceURL, tp.Freshness.State, tp.Rollback)
	}
	return b.String()
}

func TestMarkdownMatchesJSON(t *testing.T) {
	m := loadMatrix(t)
	raw, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatalf("read %s: %v", mdPath, err)
	}
	doc := string(raw)
	const begin, end = "<!-- BEGIN MATRIX -->\n", "<!-- END MATRIX -->"
	i, j := strings.Index(doc, begin), strings.Index(doc, end)
	if i < 0 || j < i {
		t.Fatal("matrix markers missing in markdown")
	}
	got := doc[i+len(begin) : j]
	if want := renderTables(m); got != want {
		t.Fatalf("markdown matrix drifted from %s.\nwant:\n%s\ngot:\n%s", jsonPath, want, got)
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
	for _, s := range []string{
		"#50 maps to G2", "#51 to G3", "#52 to optional G4",
		"Owner-only, 10 read-only, Approval+Canary,\n  20 read+write",
		"G4 (twenty-user\n  writes) depends on G3, #55 and #57",
	} {
		if !strings.Contains(doc, s) {
			t.Errorf("markdown lost statement %q", s)
		}
	}
}
