// Package design checks the C2 readiness planning artifact. The matrix is
// documentation, not an authorization engine: nothing here is used at runtime
// and no test contacts GitHub or any provider.
package design

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	Canary          *canary   `json:"canary_authorization"`
}

type topic struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Status string   `json:"status"`
	Gates  []string `json:"gates"`
}

type matrix struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	Issue         string `json:"issue"`
	Issue40Done   bool   `json:"issue_40_fully_satisfied"`
	Baseline      struct {
		Repo        string `json:"repo"`
		Branch      string `json:"branch"`
		SHA         string `json:"sha"`
		EvidenceURL string `json:"evidence_url"`
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
	Forbidden []string `json:"forbidden_surfaces"`
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
	DecisionRefs []struct {
		ID       string `json:"id"`
		Required bool   `json:"required"`
		Stage    string `json:"stage"`
		Note     string `json:"note"`
	} `json:"decision_refs"`
	Gates  []gate  `json:"gates"`
	Topics []topic `json:"topics"`
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

var sha40 = regexp.MustCompile(`^[0-9a-f]{40}$`)

// verdict reports whether a gate may be treated as PASS at time now. A gate is
// PASS only when it claims PASS and its evidence is present, observable and not
// aged. maxAge is a synthetic proposal supplied by the caller; the real matrix
// has no approved TTL, so with a nil maxAge nothing can pass.
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
	if !strings.HasPrefix(g.EvidenceURL, "https://") {
		return "UNKNOWN", "missing evidence url"
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
		"empty sha":        func(g *gate) { g.EvidenceSHA = "" },
		"UNKNOWN sha":      func(g *gate) { g.EvidenceSHA = "UNKNOWN" },
		"short sha":        func(g *gate) { g.EvidenceSHA = "abc123" },
		"empty url":        func(g *gate) { g.EvidenceURL = "" },
		"UNKNOWN url":      func(g *gate) { g.EvidenceURL = "UNKNOWN" },
		"http url":         func(g *gate) { g.EvidenceURL = "http://example.invalid/x" },
		"no observed_at":   func(g *gate) { g.Freshness.ObservedAt = nil },
		"bad observed_at":  func(g *gate) { g.Freshness.ObservedAt = strp("yesterday") },
		"status UNKNOWN":   func(g *gate) { g.Status = "UNKNOWN" },
		"status empty":     func(g *gate) { g.Status = "" },
		"status ambiguous": func(g *gate) { g.Status = "MAYBE" },
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
	// Owner pre-GO (G0) and its later live results (G1) are different kinds.
	if m.gate("G0").Kind != "decision" || m.gate("G1").Kind != "result" {
		t.Error("G0 must be a decision and G1 its results")
	}
}

func TestDecisionRefsStaySeparate(t *testing.T) {
	m := loadMatrix(t)
	byID := map[string]bool{}
	for _, r := range m.DecisionRefs {
		if byID[r.ID] {
			t.Errorf("duplicate decision ref %s", r.ID)
		}
		byID[r.ID] = r.Required
	}
	if req, ok := byID["#50"]; !ok || !req {
		t.Error("#50 must be present and required")
	}
	if req, ok := byID["#51"]; !ok || !req {
		t.Error("#51 must be present and required")
	}
	if req, ok := byID["#52"]; !ok || req {
		t.Error("#52 must be present and optional")
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestReadOnlyPhasesHaveNoWriteDependency(t *testing.T) {
	m := loadMatrix(t)
	for _, id := range []string{"G0", "G1", "G2", "G3"} {
		g := m.gate(id)
		if g.WriteDependency {
			t.Errorf("%s is read-only but declares a write dependency", id)
		}
		for _, dep := range []string{"#55", "#57"} {
			if contains(g.DependsOn, dep) || contains(g.Requires, dep) {
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
	g4 := m.gate("G4")
	if !g4.WriteDependency || !contains(g4.AllowedSurfaces, "basket-write") {
		t.Error("G4 must be the write stage")
	}
	for _, req := range []string{"#1011", "#1012"} {
		if !contains(g4.Requires, req) {
			t.Errorf("G4 must require %s", req)
		}
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
	if m.Authority.Issue11.Owner != "lead" || m.Authority.Issue11.Status != "FOLLOW_UP_NOT_DONE" {
		t.Error("#11 correction must be a lead-owned follow-up")
	}
}

func TestTopicsCoverRequiredAreas(t *testing.T) {
	m := loadMatrix(t)
	required := []string{
		"cutover-2777", "rollback-kill", "oauth", "consent-versions", "scopes",
		"vault-control-identity", "proxy", "migrations", "image-pins", "capacity",
		"glovo-home-writes", "isolation",
	}
	have := map[string]topic{}
	for _, tp := range m.Topics {
		have[tp.ID] = tp
	}
	for _, id := range required {
		tp, ok := have[id]
		if !ok {
			t.Errorf("topic %s missing", id)
			continue
		}
		if tp.Status != "UNKNOWN" {
			t.Errorf("topic %s has live status %q, want UNKNOWN", id, tp.Status)
		}
		for _, gid := range tp.Gates {
			if m.gate(gid) == nil {
				t.Errorf("topic %s references unknown gate %s", id, gid)
			}
		}
	}
	if len(m.Topics) != len(required) {
		t.Errorf("got %d topics, want %d", len(m.Topics), len(required))
	}
}

func renderTables(m matrix) string {
	var b strings.Builder
	b.WriteString("| Gate | Status | Role | Phase | Evidence SHA | Evidence URL | Freshness | Rollback |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|\n")
	for _, g := range m.Gates {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s |\n",
			g.ID, g.Status, g.Role, g.Phase, g.EvidenceSHA, g.EvidenceURL, g.Freshness.State, g.Rollback)
	}
	b.WriteString("\n| Topic | Status | Gates |\n|---|---|---|\n")
	for _, tp := range m.Topics {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", tp.Name, tp.Status, strings.Join(tp.Gates, ", "))
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
}
