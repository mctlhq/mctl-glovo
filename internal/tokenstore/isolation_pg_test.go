package tokenstore_test

// Cross-user isolation suite for the hosted path (C2-2 slice 7).
//
// Everything here runs the REAL Postgres store (a fresh schema per test),
// the real Sessions cache and the real glovotools hosted Backend, against a
// fake Glovo that attributes every request to the owner of the access token
// it carried. The invariant: user A never reads, writes, refreshes or even
// observes user B's session, tokens, location, link state or audit rows.
//
// Users carry a tag ("TAGA", "TAGB", ...) in every value that belongs to
// them (tokens, device urn, city, query). A request whose token belongs to
// one user but whose bytes mention another user's tag is a crossing.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/mctlhq/mctl-glovo/internal/authn"
	"github.com/mctlhq/mctl-glovo/internal/glovoapi"
	"github.com/mctlhq/mctl-glovo/internal/glovotools"
	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

var isoUsers = []string{"A", "B", "C"}

func isoPID(u string) string { return "prn_iso" + strings.ToLower(u) + "0000000000000000" }
func isoTag(u string) string { return "TAG" + u }

type isoReq struct {
	owner  string // owner of the access token, "" for an unknown token
	kind   string // "api" or "refresh"
	blob   string // method, URL, headers, body
	status int
}

// isoGlovo is a fake Glovo: it hands out and rotates tokens per owner and
// records every request with the owner its token belongs to.
type isoGlovo struct {
	srv *httptest.Server

	mu          sync.Mutex
	access      map[string]string // access token -> owner
	refresh     map[string]string // refresh token -> owner
	failRefresh map[string]int    // owner -> status to answer a refresh with
	expired     map[string]bool   // access token -> answers 401
	seq         int
	reqs        []isoReq
}

func newIsoGlovo(t *testing.T) *isoGlovo {
	t.Helper()
	g := &isoGlovo{access: map[string]string{}, refresh: map[string]string{}, failRefresh: map[string]int{}, expired: map[string]bool{}}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *isoGlovo) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var hdr strings.Builder
	for k, v := range r.Header {
		hdr.WriteString(k + "=" + strings.Join(v, ",") + ";")
	}
	blob := r.Method + " " + r.URL.String() + " " + hdr.String() + " " + string(body)
	g.mu.Lock()
	defer g.mu.Unlock()
	rec := isoReq{blob: blob}
	finish := func(status int) {
		rec.status = status
		g.reqs = append(g.reqs, rec)
	}
	if r.URL.Path == "/oauth/refresh" {
		rec.kind = "refresh"
		var in map[string]string
		_ = json.Unmarshal(body, &in)
		owner, ok := g.refresh[in["refreshToken"]]
		rec.owner = owner
		if st := g.failRefresh[owner]; st != 0 {
			finish(st)
			w.WriteHeader(st)
			return
		}
		if !ok {
			finish(http.StatusUnauthorized)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		delete(g.refresh, in["refreshToken"])
		g.seq++
		acc, ref := "acc-"+isoTag(owner)+"-r"+strconv.Itoa(g.seq), "ref-"+isoTag(owner)+"-r"+strconv.Itoa(g.seq)
		g.access[acc], g.refresh[ref] = owner, owner
		finish(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": acc, "refreshToken": ref, "expiresIn": 3600})
		return
	}
	rec.kind = "api"
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	owner, ok := g.access[tok]
	rec.owner = owner
	if !ok || g.expired[tok] {
		finish(http.StatusUnauthorized)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	finish(http.StatusOK)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}

// session registers a fresh session of owner and returns it.
func (g *isoGlovo) session(owner, gen string) glovoapi.Session {
	g.mu.Lock()
	defer g.mu.Unlock()
	acc, ref := "acc-"+isoTag(owner)+"-"+gen, "ref-"+isoTag(owner)+"-"+gen
	g.access[acc], g.refresh[ref] = owner, owner
	return glovoapi.Session{
		"accessToken":  acc,
		"refreshToken": ref,
		"deviceUrn":    "urn:device:" + isoTag(owner),
		"customerId":   "cust-" + isoTag(owner),
		"createdAt":    json.Number(strconv.FormatInt(time.Now().UnixMilli(), 10)),
		"expiresIn":    json.Number("3600"),
		"location":     map[string]any{"countryCode": "XX", "cityCode": "CITY" + isoTag(owner), "latitude": "1", "longitude": "2"},
	}
}

// expireAccess makes every access token of owner answer 401, so its next
// request forces a refresh.
func (g *isoGlovo) expireAccess(owner string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for tok, o := range g.access {
		if o == owner {
			g.expired[tok] = true
		}
	}
}

func (g *isoGlovo) setFailRefresh(owner string, status int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failRefresh[owner] = status
}

func (g *isoGlovo) snapshot() []isoReq {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]isoReq(nil), g.reqs...)
}

// count is the number of requests of kind carrying owner's tokens.
func (g *isoGlovo) count(owner, kind string) int {
	n := 0
	for _, r := range g.snapshot() {
		if r.owner == owner && (kind == "" || r.kind == kind) {
			n++
		}
	}
	return n
}

// crossings lists requests whose token belongs to one user while the bytes
// name another. It is the core detector of the suite.
func (g *isoGlovo) crossings() []string {
	var bad []string
	for _, r := range g.snapshot() {
		for _, u := range isoUsers {
			if u != r.owner && strings.Contains(r.blob, isoTag(u)) && r.owner != "" {
				bad = append(bad, fmt.Sprintf("%s request carries %s data: %.200s", r.owner, u, r.blob))
			}
		}
	}
	return bad
}

type isoEnv struct {
	t     *testing.T
	pg    *tokenstore.PGIso
	glovo *isoGlovo
	b     *glovotools.Backend
	srcs  *tokenstore.Sessions
}

func newIsoEnv(t *testing.T, caps tokenstore.Caps) *isoEnv {
	t.Helper()
	g := newIsoGlovo(t)
	pg := tokenstore.NewPGIso(t, g.srv.URL, caps, nil)
	b, err := glovotools.New(glovotools.Config{Clients: pg.Sessions})
	if err != nil {
		t.Fatal(err)
	}
	return &isoEnv{t: t, pg: pg, glovo: g, b: b, srcs: pg.Sessions}
}

func isoCaps() tokenstore.Caps {
	c := tokenstore.DefaultCaps()
	c.PIDRequestsPerMinute, c.PIDRequestsPerDay, c.GlobalRequestsPerMinute = 1000, 100000, 100000
	c.PIDDecryptsPerHour, c.GlobalDecryptsPerHour = 100000, 100000
	return c
}

func pidPrincipal(id string) context.Context {
	return authn.WithPrincipal(context.Background(), &authn.Principal{ID: id})
}

// link runs the real code flow for owner and returns the session uploaded.
func (e *isoEnv) link(owner, gen string) glovoapi.Session {
	e.t.Helper()
	ctx := context.Background()
	sess := e.glovo.session(owner, gen)
	pid := tokenstore.PIDFrom(&authn.Principal{ID: isoPID(owner)})
	code, _, err := e.pg.Store.CreateLinkCode(ctx, pid, "consent-v1")
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.pg.Store.Link(ctx, code, sess); err != nil {
		e.t.Fatal(err)
	}
	return sess
}

// call runs one hosted tool as user u and returns its text and error text.
func (e *isoEnv) call(u, tool string, a map[string]any) (string, error) {
	res, err := e.b.Call(pidPrincipal(isoPID(u)), tool, a)
	return resText(res), err
}

func resText(r *mcp.CallToolResult) string {
	if r == nil {
		return ""
	}
	var sb strings.Builder
	for _, c := range r.Content {
		if t, ok := c.(mcp.TextContent); ok {
			sb.WriteString(t.Text)
		}
	}
	return sb.String()
}

func (e *isoEnv) noCrossings() {
	e.t.Helper()
	if bad := e.glovo.crossings(); len(bad) > 0 {
		e.t.Fatalf("cross-user data reached Glovo:\n%s", strings.Join(bad, "\n"))
	}
}

// rowSnapshot is every column of pid's link row except none: a write by
// another user must not change a single one.
func rowSnapshot(t *testing.T, db *sql.DB, pid string) string {
	t.Helper()
	var s sql.NullString
	err := db.QueryRow(`SELECT row(l.*)::text FROM glovo_links l WHERE pid = $1`, pid).Scan(&s)
	if err == sql.ErrNoRows {
		return "<none>"
	}
	if err != nil {
		t.Fatal(err)
	}
	return s.String
}

// auditRows returns "event/outcome/reason" of the audit rows of pid.
func auditRows(t *testing.T, db *sql.DB, pid string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT a.event || '/' || a.outcome || '/' || a.reason || '/' || coalesce(a.link_id, '')
		FROM glovo_store_audit a JOIN glovo_audit_subjects s USING (subject_id) WHERE s.pid = $1 ORDER BY a.id`, pid)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// everyHostedCall is one call per tool that is allowed to reach Glovo, with
// valid arguments, plus crafted identity fields.
func everyHostedCall(user string) map[string]map[string]any {
	other := isoTag("B")
	if user == "B" {
		other = isoTag("A")
	}
	craft := map[string]any{"pid": isoPID("B"), "principal": isoPID("B"), "user_id": isoPID("B"), "customer_id": "cust-" + other}
	merge := func(m map[string]any) map[string]any {
		for k, v := range craft {
			m[k] = v
		}
		return m
	}
	q := "q" + isoTag(user)
	return map[string]map[string]any{
		"glovo_auth_status":             merge(map[string]any{}),
		"glovo_get_location":            merge(map[string]any{}),
		"glovo_get_saved_locations":     merge(map[string]any{}),
		"glovo_search_locations":        merge(map[string]any{"query": q}),
		"glovo_select_location":         merge(map[string]any{"place_id": q}),
		"glovo_browse_stores":           merge(map[string]any{}),
		"glovo_get_store":               merge(map[string]any{"store": q}),
		"glovo_get_store_menu":          merge(map[string]any{"store_id": 1, "store_address_id": 2}),
		"glovo_search_store_items":      merge(map[string]any{"store_id": 1, "store_address_id": 2, "query": q}),
		"glovo_get_product":             merge(map[string]any{"store_id": 1, "store_address_id": 2, "product_id": 3}),
		"glovo_get_basket":              merge(map[string]any{}),
		"glovo_add_to_basket":           merge(map[string]any{"store_id": 1, "store_address_id": 2, "product_id": 3, "quantity": 1}),
		"glovo_set_quantity":            merge(map[string]any{"store_id": 1, "product_id": 3, "quantity": 2}),
		"glovo_remove_from_basket":      merge(map[string]any{"store_id": 1, "product_id": 3}),
		"glovo_get_purchase_history":    merge(map[string]any{}),
		"glovo_get_order_items":         merge(map[string]any{"order_id": q}),
		"glovo_preview_reorder":         merge(map[string]any{"order_id": q}),
		"glovo_set_location":            merge(map[string]any{"country_code": "XX", "city_code": "CITY" + isoTag(user) + "-NEW", "latitude": "1.5", "longitude": "2.5"}),
		"glovo_get_suggestions":         merge(map[string]any{"query": q}),
		"glovo_browse_store_catalog":    merge(map[string]any{"store_id": 1, "store_address_id": 2, "content_uri": q}),
		"glovo_get_store_order_options": merge(map[string]any{"store_id": 1, "store_address_id": 2}),
	}
}

// Every hosted tool, called by A with identity fields naming B, runs only on
// A's tokens, and nothing of B appears in anything A sends or receives.
func TestIsoEveryToolRunsOnTheCallersTokensOnly(t *testing.T) {
	e := newIsoEnv(t, isoCaps())
	e.link("A", "0")
	e.link("B", "0")
	rowB := rowSnapshot(t, e.pg.DB, isoPID("B"))
	auditB := auditRows(t, e.pg.DB, isoPID("B"))
	storedB := e.pg.Stored(isoPID("B"))

	var results strings.Builder
	calls := everyHostedCall("A")
	names := make([]string, 0, len(calls))
	for n := range calls {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, tool := range names {
		text, err := e.call("A", tool, calls[tool])
		results.WriteString(text)
		if err != nil {
			results.WriteString(err.Error())
		}
	}
	// A tool name not covered above would be a hole in this suite.
	covered := map[string]bool{"glovo_login": true, "glovo_get_shopping_guide": true, "glovo_get_store_recommendations": true,
		"glovo_plan_reorder": true, "glovo_get_order_stats": true, "glovo_analyze_order_history": true}
	for _, n := range e.b.Names() {
		if _, ok := calls[n]; !ok && !covered[n] {
			t.Errorf("tool %s is served but not exercised by the isolation suite", n)
		}
	}
	if e.glovo.count("A", "api") == 0 {
		t.Fatal("no request reached Glovo: the suite proves nothing")
	}
	if n := e.glovo.count("B", ""); n != 0 {
		t.Fatalf("B's tokens were used %d times while only A called", n)
	}
	e.noCrossings()
	for _, tag := range []string{isoTag("B"), isoPID("B")} {
		if strings.Contains(results.String(), tag) {
			t.Fatalf("A's results mention %s: %.300s", tag, results.String())
		}
	}
	if got := rowSnapshot(t, e.pg.DB, isoPID("B")); got != rowB {
		t.Fatalf("B's link row changed while only A called:\n%s\n%s", rowB, got)
	}
	if got := auditRows(t, e.pg.DB, isoPID("B")); strings.Join(got, "|") != strings.Join(auditB, "|") {
		t.Fatalf("B's audit rows changed while only A called: %v -> %v", auditB, got)
	}
	if got := e.pg.Stored(isoPID("B")); got["accessToken"] != storedB["accessToken"] {
		t.Fatal("B's stored session changed")
	}
}

// Identity fields in arguments never select, or touch, another user's row:
// a location write names its own city only for the caller.
func TestIsoLocationIsPerUserAndStoredEncrypted(t *testing.T) {
	e := newIsoEnv(t, isoCaps())
	e.link("A", "0")
	e.link("B", "0")
	rowB := rowSnapshot(t, e.pg.DB, isoPID("B"))

	_, err := e.call("A", "glovo_set_location", map[string]any{
		"country_code": "XX", "city_code": "NEWA", "latitude": "7", "longitude": "8", "pid": isoPID("B"), "user_id": isoPID("B"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := e.pg.Stored(isoPID("A"))["location"].(map[string]any); got["cityCode"] != "NEWA" {
		t.Fatalf("A's location = %v", got)
	}
	if got := rowSnapshot(t, e.pg.DB, isoPID("B")); got != rowB {
		t.Fatalf("A's location change touched B's row:\n%s\n%s", rowB, got)
	}
	b, _ := e.call("B", "glovo_get_location", map[string]any{})
	if !strings.Contains(b, "CITY"+isoTag("B")) || strings.Contains(b, "NEWA") {
		t.Fatalf("B reads %s", b)
	}
	a, _ := e.call("A", "glovo_get_location", map[string]any{})
	if !strings.Contains(a, "NEWA") || strings.Contains(a, isoTag("B")) {
		t.Fatalf("A reads %s", a)
	}
	// No clear-text location in the database: the city lives in ciphertext only.
	var n int
	if err := e.pg.DB.QueryRow(`SELECT count(*) FROM glovo_links WHERE row(glovo_links.*)::text LIKE '%NEWA%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("location in clear in glovo_links: %d %v", n, err)
	}
	e.noCrossings()
}

// A concurrent mix of both users over every kind of tool, with both users'
// access tokens expired so both refresh at the same time: each request still
// runs on its own caller's tokens, each user's refresh spends that user's
// refresh token, and each row ends up holding its own owner's tokens.
func TestIsoConcurrentUsersWithConcurrentRefreshStayIsolated(t *testing.T) {
	e := newIsoEnv(t, isoCaps())
	e.link("A", "0")
	e.link("B", "0")
	e.glovo.expireAccess("A")
	e.glovo.expireAccess("B")

	start := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string
	for _, u := range []string{"A", "B"} {
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tool, args := "glovo_search_locations", map[string]any{"query": "q" + isoTag(u) + strconv.Itoa(i), "pid": isoPID(map[string]string{"A": "B", "B": "A"}[u])}
				if i%3 == 1 {
					tool, args = "glovo_get_saved_locations", map[string]any{}
				}
				if i%3 == 2 {
					tool, args = "glovo_set_location", map[string]any{"country_code": "XX", "city_code": "C" + isoTag(u) + strconv.Itoa(i), "latitude": "1", "longitude": "2"}
				}
				text, err := e.call(u, tool, args)
				// A set_location racing a refresh of the same user used to
				// stall both on the store's busy timeout (glovoapi
				// setLocation held the session lock across the lease wait).
				if strings.Contains(text, "busy") {
					mu.Lock()
					failures = append(failures, u+": "+text)
					mu.Unlock()
				}
				if err != nil {
					mu.Lock()
					failures = append(failures, err.Error())
					mu.Unlock()
				}
				for _, o := range isoUsers {
					if o != u && strings.Contains(text, isoTag(o)) {
						mu.Lock()
						failures = append(failures, u+" result mentions "+o)
						mu.Unlock()
					}
				}
			}()
		}
	}
	wg.Wait()
	if len(failures) > 0 {
		t.Fatalf("%v", failures)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("concurrent calls took %v: a lease wait stalled them", d)
	}
	e.noCrossings()
	for _, u := range []string{"A", "B"} {
		if n := e.glovo.count(u, "refresh"); n == 0 {
			t.Fatalf("%s never refreshed: the expiry did not take", u)
		}
		s := e.pg.Stored(isoPID(u))
		for _, k := range []string{"accessToken", "refreshToken", "deviceUrn", "customerId"} {
			if v, _ := s[k].(string); !strings.Contains(v, isoTag(u)) {
				t.Fatalf("%s's stored %s = %q: not its own", u, k, v)
			}
		}
	}
	// Two cached clients, never one shared.
	ca, _ := e.srcs.Client(pidPrincipal(isoPID("A")), tokenstore.PIDFrom(&authn.Principal{ID: isoPID("A")}))
	cb, _ := e.srcs.Client(pidPrincipal(isoPID("B")), tokenstore.PIDFrom(&authn.Principal{ID: isoPID("B")}))
	if ca == nil || cb == nil || ca == cb || e.srcs.Len() != 2 {
		t.Fatalf("session cache shares or loses clients: %p %p len=%d", ca, cb, e.srcs.Len())
	}
}

// Pids that merely resemble another user's are different users, and a
// principal id that is not a well-formed prn_ id is refused before any lookup.
func TestIsoLookalikePrincipalsAreDifferentUsersAndMalformedAreRefused(t *testing.T) {
	e := newIsoEnv(t, isoCaps())
	e.link("A", "0")
	a := isoPID("A")
	// Well-formed lookalikes are other principals, never A.
	for _, id := range []string{a + "_", a[:len(a)-1], "prn_" + strings.ToUpper(a[4:])} {
		if p := tokenstore.PIDFrom(&authn.Principal{ID: id}); p == (tokenstore.PID{}) || p == tokenstore.PIDFrom(&authn.Principal{ID: a}) {
			t.Fatalf("lookalike %q maps to %v", id, p)
		}
	}
	// Malformed ids are no principal at all: the zero PID, not a lookup that
	// merely finds nothing (the database is not the only line of defence).
	for _, id := range []string{a + "\n", a + " ", " " + a, a + "|" + isoPID("B"), "prn_x' OR '1'='1", "prn_%",
		"prn_" + strings.Repeat("a", 200), "", "PRN_" + a[4:], a + "\x00", "prn_a/../b"} {
		if p := tokenstore.PIDFrom(&authn.Principal{ID: id}); p != (tokenstore.PID{}) {
			t.Fatalf("malformed principal id %q gave the PID %v", id, p)
		}
	}
	for _, id := range []string{
		a + "_", a[:len(a)-1], strings.ToUpper(a), a + "\n", a + " ", " " + a, a + "|" + isoPID("B"),
		"prn_x' OR '1'='1", "prn_%", "prn_" + strings.Repeat("a", 200), "", "PRN_" + a[4:], a + "\x00",
	} {
		before := len(e.glovo.snapshot())
		res, err := e.b.Call(pidPrincipal(id), "glovo_get_saved_locations", map[string]any{})
		if res != nil && !res.IsError {
			t.Fatalf("principal %q was served: %s", id, resText(res))
		}
		if txt := resText(res); strings.Contains(txt, isoTag("A")) {
			t.Fatalf("principal %q saw A's data: %s", id, txt)
		}
		_ = err
		if after := e.glovo.snapshot(); len(after) != before {
			t.Fatalf("principal %q reached Glovo", id)
		}
	}
}

// What A learns from a refusal does not depend on B's state: "not linked" is
// the same answer whether B is linked, suspended, needs relink or absent.
func TestIsoRefusalsDoNotRevealOtherUsersState(t *testing.T) {
	answer := func(e *isoEnv) string {
		text, err := e.call("C", "glovo_get_saved_locations", map[string]any{})
		return fmt.Sprintf("%q|%v", text, err)
	}
	e := newIsoEnv(t, isoCaps())
	base := answer(e) // nobody linked
	e.link("B", "0")
	if got := answer(e); got != base {
		t.Fatalf("C's answer changed when B linked: %s vs %s", base, got)
	}
	if _, err := e.pg.DB.Exec(`UPDATE glovo_links SET state = 'suspended' WHERE pid = $1`, isoPID("B")); err != nil {
		t.Fatal(err)
	}
	if got := answer(e); got != base {
		t.Fatalf("C's answer changed when B was suspended: %s vs %s", base, got)
	}
	if _, err := e.pg.DB.Exec(`UPDATE glovo_links SET state = 'needs_relink' WHERE pid = $1`, isoPID("B")); err != nil {
		t.Fatal(err)
	}
	if got := answer(e); got != base {
		t.Fatalf("C's answer changed when B needed relink: %s vs %s", base, got)
	}
	if !strings.Contains(base, "not linked") {
		t.Fatalf("unexpected base answer %s", base)
	}
}

// B's state changes (suspension, needs_relink, relink with a new session,
// unlink) never change what A gets, and a relink gives B the new session
// only: never the old one, never A's.
func TestIsoOtherUsersLifecycleDoesNotAffectCaller(t *testing.T) {
	e := newIsoEnv(t, isoCaps())
	e.link("A", "0")
	e.link("B", "0")
	if _, err := e.call("A", "glovo_get_saved_locations", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.call("B", "glovo_get_saved_locations", nil); err != nil {
		t.Fatal(err)
	}
	rowA := rowSnapshot(t, e.pg.DB, isoPID("A"))
	cached := e.srcs.Len()

	if _, err := e.pg.DB.Exec(`UPDATE glovo_links SET state = 'suspended', version = nextval('glovo_link_version_seq') WHERE pid = $1`, isoPID("B")); err != nil {
		t.Fatal(err)
	}
	if text, _ := e.call("B", "glovo_get_saved_locations", nil); !strings.Contains(text, "suspended") {
		t.Fatalf("B suspended: %q", text)
	}
	if text, err := e.call("A", "glovo_get_saved_locations", nil); err != nil || strings.Contains(text, "suspended") {
		t.Fatalf("A affected by B's suspension: %q %v", text, err)
	}
	// B unlinks and relinks with a new session.
	bPID := tokenstore.PIDFrom(&authn.Principal{ID: isoPID("B")})
	if err := e.pg.Store.Unlink(context.Background(), bPID, "user"); err != nil {
		t.Fatal(err)
	}
	e.srcs.Drop(bPID)
	if text, _ := e.call("B", "glovo_get_saved_locations", nil); !strings.Contains(text, "not linked") {
		t.Fatalf("B after unlink: %q", text)
	}
	e.link("B", "1")
	before := len(e.glovo.snapshot())
	if _, err := e.call("B", "glovo_get_saved_locations", nil); err != nil {
		t.Fatal(err)
	}
	for _, r := range e.glovo.snapshot()[before:] {
		if !strings.Contains(r.blob, "acc-"+isoTag("B")+"-1") {
			t.Fatalf("B's call after relink did not use the new session: %.200s", r.blob)
		}
	}
	if got := rowSnapshot(t, e.pg.DB, isoPID("A")); got != rowA {
		t.Fatalf("A's row changed by B's lifecycle:\n%s\n%s", rowA, got)
	}
	if e.srcs.Len() < cached-1 {
		t.Fatalf("B's unlink dropped A's cached client: %d -> %d", cached, e.srcs.Len())
	}
	e.noCrossings()
}

// A failed refresh of A (Glovo refuses it, until A needs relink) does not
// touch B: B keeps serving, B's row is byte-identical, and B's failure
// accounting stays at zero.
func TestIsoFailedRefreshOfOneUserDoesNotTouchTheOther(t *testing.T) {
	e := newIsoEnv(t, isoCaps())
	e.link("A", "0")
	e.link("B", "0")
	rowB := rowSnapshot(t, e.pg.DB, isoPID("B"))
	e.glovo.expireAccess("A")
	e.glovo.setFailRefresh("A", http.StatusUnauthorized)

	for i := 0; i < 5; i++ {
		_, _ = e.call("A", "glovo_get_saved_locations", nil)
		if text, err := e.call("B", "glovo_get_saved_locations", nil); err != nil || strings.Contains(text, "expired") {
			t.Fatalf("B affected by A's failing refresh: %q %v", text, err)
		}
	}
	var stateA string
	var failsB int
	if err := e.pg.DB.QueryRow(`SELECT state FROM glovo_links WHERE pid = $1`, isoPID("A")).Scan(&stateA); err != nil {
		t.Fatal(err)
	}
	if stateA != "needs_relink" {
		t.Fatalf("A's state = %s, want needs_relink after repeated refresh failures", stateA)
	}
	if err := e.pg.DB.QueryRow(`SELECT refresh_failures FROM glovo_links WHERE pid = $1`, isoPID("B")).Scan(&failsB); err != nil || failsB != 0 {
		t.Fatalf("B's refresh failures = %d %v", failsB, err)
	}
	if n := e.glovo.count("B", "refresh"); n != 0 {
		t.Fatalf("B refreshed %d times because of A", n)
	}
	// Only the bookkeeping that belongs to B's own successful calls may move
	// B's row; the session and state are untouched.
	var stateB string
	if err := e.pg.DB.QueryRow(`SELECT state FROM glovo_links WHERE pid = $1`, isoPID("B")).Scan(&stateB); err != nil || stateB != "active" {
		t.Fatalf("B's state = %s %v", stateB, err)
	}
	if got := rowSnapshot(t, e.pg.DB, isoPID("B")); got != rowB {
		t.Fatalf("A's failing refresh changed B's row:\n%s\n%s", rowB, got)
	}
	e.noCrossings()
}

// Erasure of A removes A everywhere, including from the audit trail's
// subject table, while B's session, location and audit rows are intact; A's
// cached client is gone, and A relinking later does not resurrect B or the
// old A.
func TestIsoErasureOfOneUserLeavesTheOtherIntact(t *testing.T) {
	e := newIsoEnv(t, isoCaps())
	e.link("A", "0")
	e.link("B", "0")
	for _, u := range []string{"A", "B"} {
		if _, err := e.call(u, "glovo_set_location", map[string]any{"country_code": "XX", "city_code": "LOC" + isoTag(u), "latitude": "1", "longitude": "2"}); err != nil {
			t.Fatal(err)
		}
	}
	rowB := rowSnapshot(t, e.pg.DB, isoPID("B"))
	auditB := auditRows(t, e.pg.DB, isoPID("B"))
	oldLinkA := e.pg.LinkID(isoPID("A"))

	aPID := tokenstore.PIDFrom(&authn.Principal{ID: isoPID("A")})
	if err := e.pg.Store.Erase(context.Background(), aPID); err != nil {
		t.Fatal(err)
	}
	// The cache is NOT told: the very next call must notice the erasure.
	text, _ := e.call("A", "glovo_get_saved_locations", nil)
	if !strings.Contains(text, "not linked") {
		t.Fatalf("A after erasure: %q", text)
	}
	if e.srcs.Len() != 1 {
		t.Fatalf("A's client survived erasure in the cache: len=%d", e.srcs.Len())
	}
	for _, q := range []string{
		`SELECT count(*) FROM glovo_links WHERE pid = $1`,
		`SELECT count(*) FROM glovo_audit_subjects WHERE pid = $1`,
		`SELECT count(*) FROM glovo_link_codes WHERE pid = $1`,
	} {
		var n int
		if err := e.pg.DB.QueryRow(q, isoPID("A")).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s -> %d %v", q, n, err)
		}
	}
	if got := rowSnapshot(t, e.pg.DB, isoPID("B")); got != rowB {
		t.Fatalf("A's erasure changed B's row:\n%s\n%s", rowB, got)
	}
	if got := auditRows(t, e.pg.DB, isoPID("B")); strings.Join(got, "|") != strings.Join(auditB, "|") {
		t.Fatalf("A's erasure changed B's audit rows: %v -> %v", auditB, got)
	}
	// B still decrypts to B and reads its own location.
	if got, _ := e.pg.Stored(isoPID("B"))["location"].(map[string]any); got["cityCode"] != "LOC"+isoTag("B") {
		t.Fatalf("B's location = %v", got)
	}
	// A links again: a new link id, B's data not involved.
	e.link("A", "2")
	if e.pg.LinkID(isoPID("A")) == oldLinkA {
		t.Fatal("relink reused the erased link id")
	}
	text, _ = e.call("A", "glovo_get_location", nil)
	if strings.Contains(text, "LOC"+isoTag("A")) || strings.Contains(text, isoTag("B")) {
		t.Fatalf("A after erase+relink reads %s", text)
	}
	e.noCrossings()
}

// Swapped ciphertexts (an attacker with table write access, or a bug) are an
// integrity failure for both users: never B's session served to A.
func TestIsoSwappedCiphertextsNeverServeAnotherUsersSession(t *testing.T) {
	e := newIsoEnv(t, isoCaps())
	e.link("A", "0")
	e.link("B", "0")
	e.pg.SwapCiphertexts(isoPID("A"), isoPID("B"))
	srcs := e.pg.NewSessions() // a fresh replica: nothing cached
	b, err := glovotools.New(glovotools.Config{Clients: srcs})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"A", "B"} {
		res, err := b.Call(pidPrincipal(isoPID(u)), "glovo_get_saved_locations", nil)
		if err == nil && (res == nil || !res.IsError) {
			t.Fatalf("%s was served from a swapped row: %s", u, resText(res))
		}
	}
	if n := len(e.glovo.snapshot()); n != 0 {
		t.Fatalf("%d requests reached Glovo from swapped rows", n)
	}
}

// The kill switch stops everyone, drops every cached client, reaches Glovo
// with nothing, and when it is lifted each user is back on their own tokens.
func TestIsoKillSwitchStopsAllAndResumesIsolated(t *testing.T) {
	e := newIsoEnv(t, isoCaps())
	e.link("A", "0")
	e.link("B", "0")
	for _, u := range []string{"A", "B"} {
		if _, err := e.call(u, "glovo_get_saved_locations", nil); err != nil {
			t.Fatal(err)
		}
	}
	e.pg.SetKilled(true)
	before := len(e.glovo.snapshot())
	for _, u := range []string{"A", "B"} {
		text, err := e.call(u, "glovo_get_saved_locations", nil)
		if err != nil || !strings.Contains(text, "switched off") {
			t.Fatalf("%s while killed: %q %v", u, text, err)
		}
	}
	if e.srcs.Len() != 0 {
		t.Fatalf("kill left %d cached clients", e.srcs.Len())
	}
	if len(e.glovo.snapshot()) != before {
		t.Fatal("a request reached Glovo while killed")
	}
	e.pg.SetKilled(false)
	for _, u := range []string{"A", "B"} {
		if _, err := e.call(u, "glovo_get_saved_locations", nil); err != nil {
			t.Fatal(err)
		}
	}
	e.noCrossings()
	for _, r := range e.glovo.snapshot()[before:] {
		if r.owner == "" {
			t.Fatalf("request with an unknown token after resume: %.200s", r.blob)
		}
	}
	// An unreadable control row is killed too, for both.
	if _, err := e.pg.DB.Exec(`DELETE FROM glovo_control`); err != nil {
		t.Fatal(err)
	}
	e.pg.SetKilledCacheOnly()
	for _, u := range []string{"A", "B"} {
		text, err := e.call(u, "glovo_get_saved_locations", nil)
		if err != nil || !strings.Contains(text, "switched off") {
			t.Fatalf("%s with unreadable control: %q %v", u, text, err)
		}
	}
}

// Cap exhaustion: A hitting its per-pid cap is refused; B keeps being served
// in full; A's refused calls are not counted against the global quota; and
// the documented global cap is shared by both.
func TestIsoCapExhaustionOfOneUserDoesNotStarveOrLeak(t *testing.T) {
	caps := isoCaps()
	caps.PIDRequestsPerMinute = 5
	caps.GlobalRequestsPerMinute = 10
	e := newIsoEnv(t, caps)
	e.link("A", "0")
	e.link("B", "0")
	e.link("C", "0")

	served, refused := 0, 0
	for i := 0; i < 30; i++ {
		text, err := e.call("A", "glovo_get_saved_locations", nil)
		switch {
		case err == nil && strings.Contains(text, "rate limit"):
			refused++
		case err == nil:
			served++
		default:
			t.Fatalf("A: %v", err)
		}
	}
	perCall := e.glovo.count("A", "api") / served
	if served != 5/perCall || refused != 30-served {
		t.Fatalf("A served %d refused %d (requests per call %d)", served, refused, perCall)
	}
	// B has its own per-pid budget; A's 25 refusals did not use the global one.
	for i := 0; i < 5/perCall; i++ {
		text, err := e.call("B", "glovo_get_saved_locations", nil)
		if err != nil || strings.Contains(text, "rate limit") {
			t.Fatalf("B call %d starved by A: %q %v", i, text, err)
		}
	}
	// The global cap (10 requests/min) is now used up by A and B together:
	// C is refused by the shared cap, which is the documented behaviour.
	text, _ := e.call("C", "glovo_get_saved_locations", nil)
	if !strings.Contains(text, "rate limit") {
		t.Fatalf("global cap not shared: C got %q", text)
	}
	if n := e.glovo.count("C", ""); n != 0 {
		t.Fatalf("C's refused call reached Glovo %d times", n)
	}
	if n := e.glovo.count("B", "api"); n != e.glovo.count("A", "api") {
		t.Fatalf("A made %d requests, B %d", e.glovo.count("A", "api"), n)
	}
	e.noCrossings()
	// Refusals carry a fixed text, no user data.
	if strings.Contains(text, isoTag("A")) || strings.Contains(text, isoPID("A")) {
		t.Fatalf("refusal leaks: %q", text)
	}
}

// Audit rows and log lines produced by one user's activity never contain
// another user's pid, link id or tokens, nor any raw pid at all in the log.
func TestIsoAuditRowsAndLogsAreScopedAndCarryNoPIDOrToken(t *testing.T) {
	e := newIsoEnv(t, isoCaps())
	e.link("A", "0")
	e.link("B", "0")
	linkA, linkB := e.pg.LinkID(isoPID("A")), e.pg.LinkID(isoPID("B"))
	logsBefore := e.pg.Logs()
	e.glovo.expireAccess("A")
	for i := 0; i < 3; i++ {
		_, _ = e.call("A", "glovo_get_saved_locations", nil)
	}
	// B's logs and audit rows so far are not touched by A's activity.
	aLogs := strings.TrimPrefix(e.pg.Logs(), logsBefore)
	if aLogs == "" {
		t.Fatal("A's activity produced no log lines: the check proves nothing")
	}
	for _, bad := range []string{isoPID("A"), isoPID("B"), linkB, isoTag("B"), "acc-", "ref-"} {
		if strings.Contains(aLogs, bad) {
			t.Fatalf("log lines of A's calls contain %q:\n%s", bad, aLogs)
		}
	}
	if !strings.Contains(aLogs, linkA) {
		t.Fatalf("A's log lines do not name A's link id: %s", aLogs)
	}
	all := e.pg.Logs()
	for _, bad := range []string{isoPID("A"), isoPID("B"), isoTag("A"), isoTag("B")} {
		if strings.Contains(all, bad) {
			t.Fatalf("store logs contain %q:\n%s", bad, all)
		}
	}
	for _, row := range auditRows(t, e.pg.DB, isoPID("A")) {
		if strings.Contains(row, linkB) || strings.Contains(row, isoTag("B")) {
			t.Fatalf("A's audit row names B: %s", row)
		}
	}
	for _, row := range auditRows(t, e.pg.DB, isoPID("B")) {
		if strings.Contains(row, linkA) {
			t.Fatalf("B's audit row names A's link: %s", row)
		}
	}
	// The audit table holds no token, location or pid in clear.
	var n int
	if err := e.pg.DB.QueryRow(`SELECT count(*) FROM glovo_store_audit a WHERE row(a.*)::text ~ 'prn_|acc-|ref-|TAG|CITY'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("audit rows with identifiers or tokens in clear: %d %v", n, err)
	}
}
