package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/mctlhq/mctl-glovo/internal/authn"
	"github.com/mctlhq/mctl-glovo/internal/linkflow"
	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

const (
	allowedCred = "svc-token-allowed" //nolint:gosec // test fixture, not a credential
	// secretSubject must never reach a log line or a response.
	secretSubject = "machine-subject-0001"
)

type fakeServiceVerifier struct{}

func (fakeServiceVerifier) Verify(_ context.Context, tok string) (string, error) {
	switch tok {
	case allowedCred:
		return secretSubject, nil
	case "svc-token-unknown-subject":
		return "", &authn.Error{Reason: authn.ReasonSubjectNotAllowed, Status: http.StatusForbidden}
	case "svc-token-jwks-down":
		return "", &authn.Error{Reason: authn.ReasonJWKSDown, Status: http.StatusServiceUnavailable}
	case "svc-token-unclassified":
		return "", errors.New("boom")
	}
	return "", &authn.Error{Reason: authn.ReasonSignature, Status: http.StatusUnauthorized}
}

type recordingFlipper struct {
	mu    sync.Mutex
	calls []bool
	by    []string
	// stored, when non-nil, overrides the answer (the row read back).
	stored *bool
	err    error
}

func (f *recordingFlipper) Set(_ context.Context, killed bool, by string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, killed)
	f.by = append(f.by, by)
	if f.err != nil {
		return true, f.err
	}
	if f.stored != nil {
		return *f.stored, nil
	}
	return killed, nil
}

func (f *recordingFlipper) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type controlEnv struct {
	mux   http.Handler
	flip  *recordingFlipper
	logs  *syncBuf
	audit *syncBuf
}

func newControlEnv(t *testing.T, v serviceVerifier, f flipper) *controlEnv {
	t.Helper()
	rf, _ := f.(*recordingFlipper)
	if f == nil {
		rf = &recordingFlipper{}
		f = rf
	}
	e := &controlEnv{flip: rf, logs: &syncBuf{}, audit: &syncBuf{}}
	r, err := newControlRoutes(v, f, slog.New(slog.NewJSONHandler(e.logs, nil)), slog.New(slog.NewJSONHandler(e.audit, nil)))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	r.Register(mux)
	e.mux = mux
	return e
}

func (e *controlEnv) do(method, token, ctype, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, controlPath, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func TestControlFlipAnswersTheStoredState(t *testing.T) {
	for _, killed := range []bool{true, false} {
		e := newControlEnv(t, fakeServiceVerifier{}, nil)
		body := `{"killed":false}`
		want := `{"killed":false}`
		if killed {
			body, want = `{"killed":true}`, `{"killed":true}`
		}
		rec := e.do("PUT", allowedCred, "application/json; charset=utf-8", body, nil)
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != want {
			t.Fatalf("killed=%v: %d %q", killed, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("answer is cacheable")
		}
		if len(e.flip.calls) != 1 || e.flip.calls[0] != killed {
			t.Fatalf("flips: %v", e.flip.calls)
		}
	}
}

// The answer is what the store read back, never the request echoed.
func TestControlAnswersTheReadBackStateNotTheRequest(t *testing.T) {
	stored := true
	f := &recordingFlipper{stored: &stored}
	e := newControlEnv(t, fakeServiceVerifier{}, f)
	rec := e.do("PUT", allowedCred, "application/json", `{"killed":false}`, nil)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"killed":true}` {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}

// A failed flip is 503 with no state in the body, never 200.
func TestControlFlipFailureIsNeverASuccess(t *testing.T) {
	f := &recordingFlipper{err: tokenstore.ErrUnavailable}
	e := newControlEnv(t, fakeServiceVerifier{}, f)
	for _, body := range []string{`{"killed":false}`, `{"killed":true}`} {
		rec := e.do("PUT", allowedCred, "application/json", body, nil)
		if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), `"killed"`) {
			t.Fatalf("%s: %d %q", body, rec.Code, rec.Body.String())
		}
	}
	if !strings.Contains(e.audit.String(), `"outcome":"unknown"`) {
		t.Fatalf("failed flip not audited:\n%s", e.audit.String())
	}
}

func TestControlRefusals(t *testing.T) {
	big := `{"killed":true,"pad":"` + strings.Repeat("x", 2048) + `"}`
	cases := []struct {
		name, method, token, ctype, body string
		hdr                              map[string]string
		want                             int
	}{
		{"no credentials", "PUT", "", "application/json", `{"killed":false}`, nil, 401},
		{"bad token", "PUT", "svc-token-bad", "application/json", `{"killed":false}`, nil, 401},
		{"unknown subject", "PUT", "svc-token-unknown-subject", "application/json", `{"killed":false}`, nil, 403},
		{"jwks down", "PUT", "svc-token-jwks-down", "application/json", `{"killed":false}`, nil, 503},
		{"unclassified verifier error", "PUT", "svc-token-unclassified", "application/json", `{"killed":false}`, nil, 503},
		{"unauthenticated with a bad body is 401", "PUT", "", "application/json", `garbage`, nil, 401},
		{"basic scheme", "PUT", "", "application/json", `{"killed":false}`, map[string]string{"Authorization": "Basic " + allowedCred}, 401},
		{"non-bool string", "PUT", allowedCred, "application/json", `{"killed":"true"}`, nil, 400},
		{"non-bool number", "PUT", allowedCred, "application/json", `{"killed":1}`, nil, 400},
		{"null", "PUT", allowedCred, "application/json", `{"killed":null}`, nil, 400},
		{"missing field", "PUT", allowedCred, "application/json", `{}`, nil, 400},
		{"unknown field", "PUT", allowedCred, "application/json", `{"killed":true,"x":1}`, nil, 400},
		{"array", "PUT", allowedCred, "application/json", `[true]`, nil, 400},
		{"bare bool", "PUT", allowedCred, "application/json", `true`, nil, 400},
		{"empty body", "PUT", allowedCred, "application/json", ``, nil, 400},
		{"trailing garbage", "PUT", allowedCred, "application/json", `{"killed":true}}`, nil, 400},
		{"second value", "PUT", allowedCred, "application/json", `{"killed":true}{"killed":false}`, nil, 400},
		{"oversize", "PUT", allowedCred, "application/json", big, nil, 413},
		{"wrong content type", "PUT", allowedCred, "text/plain", `{"killed":true}`, nil, 415},
		{"no content type", "PUT", allowedCred, "", `{"killed":true}`, nil, 415},
		{"via a proxy (XFF)", "PUT", allowedCred, "application/json", `{"killed":true}`, map[string]string{"X-Forwarded-For": "203.0.113.9"}, 404},
		{"via a proxy (Forwarded)", "PUT", allowedCred, "application/json", `{"killed":true}`, map[string]string{"Forwarded": "for=203.0.113.9"}, 404},
		{"GET", "GET", allowedCred, "", ``, nil, 405},
		{"POST", "POST", allowedCred, "application/json", `{"killed":true}`, nil, 405},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newControlEnv(t, fakeServiceVerifier{}, nil)
			rec := e.do(c.method, c.token, c.ctype, c.body, c.hdr)
			if rec.Code != c.want {
				t.Fatalf("got %d %q, want %d", rec.Code, rec.Body.String(), c.want)
			}
			if n := e.flip.n(); n != 0 {
				t.Fatalf("a refused request flipped the switch (%d)", n)
			}
			if strings.Contains(rec.Body.String(), `"killed"`) {
				t.Fatalf("a refusal carried a state: %q", rec.Body.String())
			}
		})
	}
}

// With an empty allow-list the real verifier refuses a token before any
// key work, so the route is closed even if mounted.
func TestControlEmptyAllowListRefusesEveryone(t *testing.T) {
	v, err := authn.NewService(authn.ServiceConfig{Issuer: "https://idp.test", Audience: "a", JWKSURL: "https://idp.test/keys"})
	if err != nil {
		t.Fatal(err)
	}
	e := newControlEnv(t, v, nil)
	rec := e.do("PUT", "a.b.c", "application/json", `{"killed":false}`, nil)
	if rec.Code != http.StatusForbidden || e.flip.n() != 0 {
		t.Fatalf("%d, flips %d", rec.Code, e.flip.n())
	}
}

// Neither the subject nor the token reaches a response or a log line.
func TestControlLogsCarryNoSubjectOrToken(t *testing.T) {
	e := newControlEnv(t, fakeServiceVerifier{}, nil)
	for _, tok := range []string{allowedCred, "svc-token-unknown-subject", "svc-token-bad"} {
		rec := e.do("PUT", tok, "application/json", `{"killed":true}`, nil)
		if strings.Contains(rec.Body.String(), secretSubject) || strings.Contains(rec.Body.String(), tok) {
			t.Fatalf("response leaks: %q", rec.Body.String())
		}
	}
	for _, out := range []string{e.logs.String(), e.audit.String()} {
		if strings.Contains(out, secretSubject) || strings.Contains(out, "svc-token") {
			t.Fatalf("log leaks:\n%s", out)
		}
	}
	if got := strings.Count(e.audit.String(), "kill_switch_flip"); got != 1 {
		t.Fatalf("want one flip audit line, got %d:\n%s", got, e.audit.String())
	}
	if !strings.Contains(e.audit.String(), "kill_switch_refused") {
		t.Fatalf("refusals not audited:\n%s", e.audit.String())
	}
}

func TestControlDeps(t *testing.T) {
	l := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := newControlRoutes(nil, &recordingFlipper{}, l, l); err == nil {
		t.Fatal("nil verifier accepted")
	}
	if _, err := newControlRoutes(fakeServiceVerifier{}, nil, l, l); err == nil {
		t.Fatal("nil kill switch accepted")
	}
	var nilRoutes *controlRoutes
	mux := http.NewServeMux()
	nilRoutes.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("PUT", controlPath, strings.NewReader(`{"killed":false}`)))
	if rec.Code != 404 {
		t.Fatalf("unmounted route: %d", rec.Code)
	}
}

func TestLoadControl(t *testing.T) {
	full := map[string]string{
		"GLOVO_C2_CONTROL_SUBJECTS": " m1 , m2 ", "GLOVO_C2_CONTROL_ISSUER": "https://idp.test",
		"GLOVO_C2_CONTROL_AUDIENCE": "aud", "GLOVO_C2_CONTROL_JWKS_URL": "https://idp.test/keys",
	}
	c, err := loadControl(env(full))
	if err != nil || !c.enabled() || len(c.subjects) != 2 || c.subjects[1] != "m2" {
		t.Fatalf("%+v %v", c, err)
	}
	if c, err := loadControl(env(nil)); err != nil || c.enabled() {
		t.Fatalf("unset must be off: %+v %v", c, err)
	}
	for k := range full {
		part := map[string]string{}
		for kk, v := range full {
			if kk != k {
				part[kk] = v
			}
		}
		if _, err := loadControl(env(part)); err == nil {
			t.Errorf("missing %s accepted", k)
		}
	}
	for _, k := range []string{"GLOVO_C2_CONTROL_ISSUER", "GLOVO_C2_CONTROL_JWKS_URL"} {
		for _, bad := range []string{"http://idp.test", "https://u:p@idp.test", "https://idp.test/k?x=1", "not a url", "https://"} {
			m := map[string]string{}
			for kk, v := range full {
				m[kk] = v
			}
			m[k] = bad
			if _, err := loadControl(env(m)); err == nil {
				t.Errorf("%s=%q accepted", k, bad)
			}
		}
	}
	// Subjects only, or only the identity: refused, never an open route.
	if _, err := loadControl(env(map[string]string{"GLOVO_C2_CONTROL_SUBJECTS": "m1"})); err == nil {
		t.Fatal("subjects alone accepted")
	}
}

// loadC2 reads the control identity only when the flag is on, and a bad
// control config stops the boot.
func TestLoadC2ReadsControlOnlyWhenEnabled(t *testing.T) {
	e := fullEnv()
	e["GLOVO_C2_CONTROL_SUBJECTS"] = "m1"
	if _, err := loadC2(env(e)); err == nil {
		t.Fatal("partial control config accepted at boot")
	}
	e["GLOVO_C2_CONTROL_ISSUER"] = "https://idp.test"
	e["GLOVO_C2_CONTROL_AUDIENCE"] = "aud"
	e["GLOVO_C2_CONTROL_JWKS_URL"] = "https://idp.test/keys"
	c, err := loadC2(env(e))
	if err != nil || !c.control.enabled() {
		t.Fatalf("%+v %v", c.control, err)
	}
	e["GLOVO_C2_ENABLED"] = "false"
	c, err = loadC2(env(e))
	if err != nil || c.control.enabled() {
		t.Fatalf("flag off must read nothing: %+v %v", c.control, err)
	}
}

// --- enforcement -------------------------------------------------------

// ctlDB is a control row whose reads can fail; nothing else of the DB is
// reachable (a nil embedded interface panics if the check strays).
type ctlDB struct {
	tokenstore.DB
	mu     sync.Mutex
	killed bool
	err    error
	reads  int
}

func (d *ctlDB) ReadControl(context.Context) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reads++
	if d.err != nil {
		return false, d.err // worst shape: "running" together with an error
	}
	return d.killed, nil
}

func toolNames(t *testing.T, e *mcpEnv, token, session string) []string {
	t.Helper()
	_, body := e.post(t, token, session, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	var env struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		// event-stream framing: take the data line
		for _, ln := range strings.Split(body, "\n") {
			if d, ok := strings.CutPrefix(ln, "data: "); ok {
				_ = json.Unmarshal([]byte(d), &env)
			}
		}
	}
	var out []string
	for _, tl := range env.Result.Tools {
		out = append(out, tl.Name)
	}
	if len(out) < 10 {
		t.Fatalf("tools/list gave %d tools: %s", len(out), body)
	}
	return out
}

// Killed blocks every hosted tool before the admitter, the session lookup
// and any Glovo request.
func TestKilledBlocksEveryHostedToolBeforeAnyLookup(t *testing.T) {
	var admitted []string
	var mu sync.Mutex
	admit := func(_ context.Context, pid string) error {
		mu.Lock()
		defer mu.Unlock()
		admitted = append(admitted, pid)
		return nil
	}
	killed := true
	e := newMCPEnvWith(t, admit, func(d *mcpDeps) {
		d.Kill = func(context.Context) error {
			if killed {
				return tokenstore.ErrKilled
			}
			return nil
		}
	}, nil)
	sid := e.open(t, "tok-a")
	names := toolNames(t, e, "tok-a", sid)
	for _, name := range names {
		status, res, body := e.call(t, "tok-a", sid, name, map[string]any{})
		if status != 200 || res == nil || !res.IsError || resultText(res) != killedText {
			t.Fatalf("%s while killed: %d %q", name, status, body)
		}
	}
	if got := e.src.askedPIDs(); len(got) != 0 {
		t.Fatalf("session lookups while killed: %v", got)
	}
	if got := e.fake.seen(); len(got) != 0 {
		t.Fatalf("Glovo requests while killed: %v", got)
	}
	mu.Lock()
	if len(admitted) != 0 {
		t.Fatalf("admitter consulted while killed: %v", admitted)
	}
	mu.Unlock()
	if got := strings.Count(e.audit.String(), `"outcome":"refused"`); got != len(names) {
		t.Fatalf("want %d refused audit lines, got %d", len(names), got)
	}
	// Un-killed, the same call reaches the backend (the refusal was the
	// switch, not something else).
	killed = false
	if status, res, body := e.call(t, "tok-a", sid, probeTool, map[string]any{}); status != 200 || res == nil || res.IsError {
		t.Fatalf("after unkill: %d %s", status, body)
	}
	if len(e.src.askedPIDs()) == 0 {
		t.Fatal("unkilled call never reached the session lookup")
	}
}

// A control row that cannot be read means killed (the real KillSwitch), and
// the refusal text is fixed.
func TestUnreadableControlRowBlocksToolCalls(t *testing.T) {
	db := &ctlDB{err: errors.New("connection reset")}
	ks, err := tokenstore.NewKillSwitch(db)
	if err != nil {
		t.Fatal(err)
	}
	e := newMCPEnvWith(t, nil, func(d *mcpDeps) { d.Kill = ks.Check }, nil)
	sid := e.open(t, "tok-a")
	status, res, body := e.call(t, "tok-a", sid, probeTool, map[string]any{})
	if status != 200 || res == nil || !res.IsError || resultText(res) != killedText {
		t.Fatalf("unreadable row: %d %s", status, body)
	}
	if len(e.src.askedPIDs()) != 0 || len(e.fake.seen()) != 0 {
		t.Fatal("a lookup or a Glovo request happened with an unreadable control row")
	}
	// Recovery is not blocked by a cached failure.
	db.mu.Lock()
	db.err = nil
	db.mu.Unlock()
	if status, res, body := e.call(t, "tok-a", sid, probeTool, map[string]any{}); status != 200 || res == nil || res.IsError {
		t.Fatalf("after recovery: %d %s", status, body)
	}
}

// A control row read that outlives KillTimeout is a killed answer.
func TestSlowControlReadBlocksToolCalls(t *testing.T) {
	e := newMCPEnvWith(t, nil, func(d *mcpDeps) {
		d.KillTimeout = 50 * time.Millisecond
		d.Kill = func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}
	}, nil)
	sid := e.open(t, "tok-a")
	start := time.Now()
	status, res, _ := e.call(t, "tok-a", sid, probeTool, map[string]any{})
	if status != 200 || res == nil || !res.IsError || resultText(res) != killedText {
		t.Fatalf("slow read: %d %+v", status, res)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the kill read was not bounded")
	}
	if len(e.src.askedPIDs()) != 0 {
		t.Fatal("lookup after a timed out kill read")
	}
}

func TestMCPRoutesRequireAKillGate(t *testing.T) {
	_, err := newMCPRoutes(mcpDeps{
		Verifier: tokenVerifier{}, Clients: &fakeSource{}, Audit: slog.New(slog.NewTextHandler(io.Discard, nil)), Who: testWho,
	})
	if err == nil {
		t.Fatal("routes built without a kill gate")
	}
}

// The middleware itself: killed with a verified principal never calls next.
func TestKilledMiddlewareNeverCallsNext(t *testing.T) {
	called := false
	h := auditMiddleware(slog.New(slog.NewTextHandler(io.Discard, nil)), testWho,
		func(context.Context) error { return tokenstore.ErrKilled }, time.Second, nil, time.Second)(
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			called = true
			return mcp.NewToolResultText("x"), nil
		})
	ctx := authn.WithPrincipal(context.Background(), &authn.Principal{ID: "prn_A"})
	res, err := h(ctx, mcp.CallToolRequest{})
	if err != nil || called || res == nil || !res.IsError || resultText(res) != killedText {
		t.Fatalf("called=%v res=%+v err=%v", called, res, err)
	}
}

// --- Postgres end to end ----------------------------------------------

func pgControlDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TOKENSTORE_TEST_PG_DSN")
	if dsn == "" {
		if os.Getenv("TOKENSTORE_TEST_PG_REQUIRED") != "" {
			t.Fatal("TOKENSTORE_TEST_PG_REQUIRED is set but TOKENSTORE_TEST_PG_DSN is empty")
		}
		t.Skip("TOKENSTORE_TEST_PG_DSN not set")
	}
	schema := "ctl_" + strings.ToLower(strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", ""))
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatalf("postgres from the DSN unusable: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		_ = admin.Close()
	})
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = db.Close() })
	if err := tokenstore.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}

// The whole path on a real database: the store ships killed, an
// authenticated PUT unkills it durably (a new switch instance, as after a
// restart, agrees), a PUT kills it again, and a missing control row makes the
// PUT fail and tool calls refuse.
func TestControlFlipEndToEndOnPostgres(t *testing.T) {
	db := pgControlDB(t)
	ctx := context.Background()
	pg, err := tokenstore.NewPostgres(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	ks, _ := tokenstore.NewKillSwitch(pg)
	if err := ks.Check(ctx); !errors.Is(err, tokenstore.ErrKilled) {
		t.Fatalf("fresh store is not killed: %v", err)
	}
	e := newControlEnv(t, fakeServiceVerifier{}, ks)
	rec := e.do("PUT", allowedCred, "application/json", `{"killed":false}`, nil)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"killed":false}` {
		t.Fatalf("unkill: %d %q", rec.Code, rec.Body.String())
	}
	restarted, _ := tokenstore.NewKillSwitch(pg)
	if err := restarted.Check(ctx); err != nil {
		t.Fatalf("unkill not durable: %v", err)
	}
	if err := ks.Check(ctx); err != nil {
		t.Fatalf("this process still killed after the flip: %v", err)
	}
	rec = e.do("PUT", allowedCred, "application/json", `{"killed":true}`, nil)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"killed":true}` {
		t.Fatalf("kill: %d %q", rec.Code, rec.Body.String())
	}
	if err := ks.Check(ctx); !errors.Is(err, tokenstore.ErrKilled) {
		t.Fatalf("cached running outlived the kill: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM glovo_control`); err != nil {
		t.Fatal(err)
	}
	rec = e.do("PUT", allowedCred, "application/json", `{"killed":false}`, nil)
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), `"killed"`) {
		t.Fatalf("flip with no control row: %d %q", rec.Code, rec.Body.String())
	}
	if err := ks.Check(ctx); !errors.Is(err, tokenstore.ErrKilled) {
		t.Fatalf("missing row is not killed: %v", err)
	}
}

// c2App mounts the control route next to the link and MCP routes, and a nil
// control (not configured) mounts nothing.
func TestC2AppMountsControlOnlyWhenConfigured(t *testing.T) {
	h, err := linkflow.New(linkflow.Config{Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	l := slog.New(slog.NewTextHandler(io.Discard, nil))
	routes, err := newControlRoutes(fakeServiceVerifier{}, &recordingFlipper{}, l, l)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		ctl  *controlRoutes
		want int
	}{{routes, http.StatusUnauthorized}, {nil, http.StatusNotFound}} {
		mux := http.NewServeMux()
		(&c2App{link: h, control: c.ctl}).Register(mux)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("PUT", controlPath, strings.NewReader(`{"killed":false}`)))
		if rec.Code != c.want {
			t.Fatalf("control=%v: %d, want %d", c.ctl != nil, rec.Code, c.want)
		}
	}
}

// rowDB is a control row whose read-back can disagree with what was written
// (another writer got in), behind the real KillSwitch.
type rowDB struct {
	tokenstore.DB
	mu     sync.Mutex
	killed bool
	// readBack, when non-nil, is what ReadControl answers regardless of the row.
	readBack *bool
}

func (d *rowDB) SetKilled(_ context.Context, killed bool, _ string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.killed = killed
	return nil
}

func (d *rowDB) ReadControl(context.Context) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.readBack != nil {
		return *d.readBack, nil
	}
	return d.killed, nil
}

func (d *rowDB) InsertAudit(context.Context, tokenstore.AuditEvent) error { return nil }

// The handler over the real switch answers the state READ BACK from the
// row. When the stored state differs from the request, the answer is
// state_unknown (503), never the request echoed as a 200.
func TestControlReadBackDiffersFromRequestIsStateUnknown(t *testing.T) {
	stored := true
	d := &rowDB{killed: true, readBack: &stored}
	ks, err := tokenstore.NewKillSwitch(d)
	if err != nil {
		t.Fatal(err)
	}
	e := newControlEnv(t, fakeServiceVerifier{}, ks)
	rec := e.do("PUT", allowedCred, "application/json", `{"killed":false}`, nil)
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), `"killed"`) {
		t.Fatalf("mismatch answered as a state: %d %q", rec.Code, rec.Body.String())
	}
	// The agreeing case is a 200 carrying the stored value.
	d.readBack = nil
	rec = e.do("PUT", allowedCred, "application/json", `{"killed":false}`, nil)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"killed":false}` {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}

// While this process is Trip-latched, an un-kill request is answered with
// the effective state: 200 {"killed": true}, so the caller sees a mismatch
// with its request (state unknown), never a false "running".
func TestControlUnkillWhileLatchedAnswersKilled(t *testing.T) {
	d := &rowDB{killed: true}
	ks, _ := tokenstore.NewKillSwitch(d)
	_ = ks.Trip(context.Background(), "t")
	e := newControlEnv(t, fakeServiceVerifier{}, ks)
	rec := e.do("PUT", allowedCred, "application/json", `{"killed":false}`, nil)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"killed":true}` {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if err := ks.Check(context.Background()); !errors.Is(err, tokenstore.ErrKilled) {
		t.Fatalf("latch cleared: %v", err)
	}
}
