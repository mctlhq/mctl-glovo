package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/mctlhq/mctl-glovo/internal/authn"
	"github.com/mctlhq/mctl-glovo/internal/glovoapi"
	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

const testAuditKey = "0123456789abcdef0123456789abcdef-test-only"

var testWho = func() auditWho {
	w, err := newHMACWho([]byte(testAuditKey))
	if err != nil {
		panic(err)
	}
	return w
}()

const probeTool = "glovo_get_saved_locations" // makes one authenticated Glovo request

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// fakeGlovo records the access token of every request that reaches "Glovo".
type fakeGlovo struct {
	mu    sync.Mutex
	auth  []string
	delay time.Duration // answer each request after this long
	srv   *httptest.Server
}

func newFakeGlovo(t *testing.T) *fakeGlovo {
	t.Helper()
	f := &fakeGlovo{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		delay := f.delay
		f.mu.Unlock()
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGlovo) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auth...)
}

func marker(owner string) string {
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	pl := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":4102444800}`))
	return "Bearer " + hdr + "." + pl + "." + owner
}

func hostedClient(t *testing.T, base, owner string) *glovoapi.Client {
	t.Helper()
	noRefresh := func(context.Context, glovoapi.Session, func(context.Context, glovoapi.Session) (glovoapi.Session, error)) (glovoapi.Session, error) {
		return nil, errors.New("unexpected refresh")
	}
	c, err := glovoapi.New("",
		glovoapi.WithBaseURL(base),
		glovoapi.WithSession(glovoapi.Session{
			"accessToken":  marker(owner),
			"refreshToken": "refresh-" + owner,
			"customerId":   float64(1),
			"location":     map[string]any{"cityCode": "TST", "latitude": "1", "longitude": "2", "countryCode": "XX"},
		}),
		glovoapi.WithRefreshCoordinator(noRefresh),
		glovoapi.WithRequestGate(func(context.Context) error { return nil }),
		glovoapi.WithClock(func() time.Time { return time.Unix(1_800_000_000, 0) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// fakeSource is a ClientSource with one distinguishable client per pid.
type fakeSource struct {
	mu      sync.Mutex
	clients map[string]*glovoapi.Client
	asked   []string
}

func (s *fakeSource) Client(_ context.Context, pid tokenstore.PID) (*glovoapi.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, pid.String())
	c, ok := s.clients[pid.String()]
	if !ok {
		return nil, tokenstore.ErrNotLinked
	}
	return c, nil
}

func (s *fakeSource) askedPIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.asked...)
}

// tokenVerifier maps fixed bearer tokens to principals.
type tokenVerifier map[string]*authn.Principal

func (v tokenVerifier) Verify(_ context.Context, tok string) (*authn.Principal, error) {
	if p, ok := v[tok]; ok {
		return p, nil
	}
	return nil, &authn.Error{Reason: authn.ReasonSignature, Status: http.StatusUnauthorized}
}

type mcpEnv struct {
	ts    *httptest.Server
	fake  *fakeGlovo
	src   *fakeSource
	audit *syncBuf
}

func newMCPEnv(t *testing.T, admit callAdmitter) *mcpEnv {
	t.Helper()
	return newMCPEnvWith(t, admit, nil, nil)
}

// newMCPEnvWith is newMCPEnv with a tweak of the routes' inputs and a hook
// to build the test server (default: a plain httptest server).
func newMCPEnvWith(t *testing.T, admit callAdmitter, tweak func(*mcpDeps), start func(http.Handler) *httptest.Server) *mcpEnv {
	t.Helper()
	fake := newFakeGlovo(t)
	src := &fakeSource{clients: map[string]*glovoapi.Client{
		"prn_A": hostedClient(t, fake.srv.URL, "A"),
		"prn_B": hostedClient(t, fake.srv.URL, "B"),
	}}
	audit := &syncBuf{}
	deps := mcpDeps{
		Who: testWho,
		Verifier: tokenVerifier{
			"tok-a":   {ID: "prn_A"},
			"tok-b":   {ID: "prn_B"},
			"tok-bad": {ID: "not-a-principal-id"},
			"tok-nil": {},
		},
		Clients: src,
		Audit:   slog.New(slog.NewJSONHandler(audit, nil)),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Admit:   admit,
	}
	if tweak != nil {
		tweak(&deps)
	}
	routes, err := newMCPRoutes(deps)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	routes.Register(mux)
	var ts *httptest.Server
	if start != nil {
		ts = start(mux)
	} else {
		ts = httptest.NewServer(mux)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = routes.Shutdown(ctx)
		ts.Close()
	})
	return &mcpEnv{ts: ts, fake: fake, src: src, audit: audit}
}

// reply is a fully read response.
type reply struct {
	StatusCode int
	Header     http.Header
}

func (e *mcpEnv) post(t *testing.T, token, session, body string) (reply, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.ts.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return reply{StatusCode: resp.StatusCode, Header: resp.Header}, string(b)
}

// open runs initialize for token and returns the MCP session id.
func (e *mcpEnv) open(t *testing.T, token string) string {
	t.Helper()
	resp, body := e.post(t, token, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	id := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode != 200 || id == "" {
		t.Fatalf("initialize: %d %q", resp.StatusCode, body)
	}
	_, _ = e.post(t, token, id, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	return id
}

// call runs one tools/call and returns the HTTP status and the decoded
// result (nil when the response is a JSON-RPC error or not a result).
func (e *mcpEnv) call(t *testing.T, token, session, tool string, args map[string]any) (int, *mcp.CallToolResult, string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	resp, body := e.post(t, token, session, string(b))
	var env struct {
		Result *mcp.CallToolResult `json:"result"`
	}
	_ = json.Unmarshal([]byte(body), &env)
	return resp.StatusCode, env.Result, body
}

func resultText(r *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// The routes exist only behind GLOVO_C2_ENABLED: with the flag off, none of
// the MCP paths is mounted, and nothing was built to serve them.
func TestMCPRoutesAbsentWhenC2Off(t *testing.T) {
	app, closeFn, err := buildLink(context.Background(), c2Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	if app.mcp != nil || len(app.ready) != 0 {
		t.Fatalf("flag off built MCP parts: %+v", app)
	}
	mux := http.NewServeMux()
	app.Register(mux)
	for _, rq := range []struct{ method, path string }{
		{"POST", "/mcp"}, {"GET", "/mcp"}, {"DELETE", "/mcp"},
		{"GET", "/.well-known/oauth-protected-resource"},
		{"GET", "/.well-known/oauth-protected-resource/mcp"},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(rq.method, rq.path, strings.NewReader("{}")))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d with the flag off", rq.method, rq.path, rec.Code)
		}
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// With the flag on the routes exist and the landing page is not served here.
func TestMCPRoutesPresentWhenMounted(t *testing.T) {
	e := newMCPEnv(t, nil)
	resp, err := http.Get(e.ts.URL + "/.well-known/oauth-protected-resource")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("metadata = %d", resp.StatusCode)
	}
	if r, _ := e.post(t, "", "", `{}`); r.StatusCode != 401 {
		t.Fatalf("unauthenticated POST /mcp = %d, want 401", r.StatusCode)
	}
	root, err := http.Get(e.ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = root.Body.Close()
	if root.StatusCode != http.StatusNotFound {
		t.Fatalf("GET / = %d: the cluster service must not serve the landing page", root.StatusCode)
	}
}

// No verified principal: refused before any lookup or Glovo request.
func TestMCPNoPrincipalIsRefusedBeforeAnyLookup(t *testing.T) {
	e := newMCPEnv(t, nil)

	// 1. No token at all: the gateway answers 401 and nothing runs.
	if status, res, _ := e.call(t, "", "", probeTool, nil); status != 401 || res != nil {
		t.Fatalf("no token: %d %+v", status, res)
	}
	// 2. A verified token whose principal has no usable pid reaches the tool
	// layer and is refused there, still before any lookup.
	for _, tok := range []string{"tok-bad", "tok-nil"} {
		session := e.open(t, tok)
		_, res, body := e.call(t, tok, session, probeTool, nil)
		if res != nil && !res.IsError {
			t.Fatalf("%s: call succeeded: %s", tok, body)
		}
	}
	if asked := e.src.askedPIDs(); len(asked) != 0 {
		t.Fatalf("a lookup ran without a verified pid: %v", asked)
	}
	if n := len(e.fake.seen()); n != 0 {
		t.Fatalf("%d requests reached Glovo without a verified pid", n)
	}
	if got := strings.Count(e.audit.String(), `"outcome":"refused"`); got != 2 {
		t.Fatalf("want 2 refused audit lines, got %d:\n%s", got, e.audit.String())
	}
	// 3. The middleware itself, with no principal in the context.
	called := false
	h := auditMiddleware(slog.New(slog.NewTextHandler(io.Discard, nil)), testWho, nil, time.Second)(
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			called = true
			return mcp.NewToolResultText("x"), nil
		})
	req := mcp.CallToolRequest{}
	req.Params.Name = probeTool
	if res, err := h(context.Background(), req); err == nil || res != nil || called {
		t.Fatalf("no principal reached the tool: res=%v err=%v called=%v", res, err, called)
	}
}

// Two principals never share a session: each call runs on the caller's own
// client, a tool argument cannot steer it, and one principal cannot use the
// other's MCP session id.
func TestMCPTwoPrincipalsNeverShareASession(t *testing.T) {
	e := newMCPEnv(t, nil)
	sa, sb := e.open(t, "tok-a"), e.open(t, "tok-b")
	if sa == sb {
		t.Fatal("two principals got the same MCP session id")
	}
	steer := map[string]any{"pid": "prn_B", "principal": "prn_B", "user_id": "prn_B"}
	for _, c := range []struct {
		tok, session, owner string
		args                map[string]any
	}{{"tok-a", sa, "A", steer}, {"tok-b", sb, "B", nil}, {"tok-a", sa, "A", nil}} {
		before := len(e.fake.seen())
		status, res, body := e.call(t, c.tok, c.session, probeTool, c.args)
		if status != 200 || res == nil || res.IsError {
			t.Fatalf("%s: %d %s", c.owner, status, body)
		}
		got := e.fake.seen()[before:]
		if len(got) == 0 {
			t.Fatalf("%s: no request reached Glovo", c.owner)
		}
		for _, a := range got {
			if a != marker(c.owner) {
				t.Fatalf("%s: request carried the token of someone else: %q", c.owner, a)
			}
		}
	}
	if got := e.src.askedPIDs(); len(got) != 3 || got[0] != "prn_A" || got[1] != "prn_B" || got[2] != "prn_A" {
		t.Fatalf("lookups = %v, want [prn_A prn_B prn_A]", got)
	}

	// B presenting A's session id: refused, and nothing is looked up.
	asked, seen := len(e.src.askedPIDs()), len(e.fake.seen())
	status, res, _ := e.call(t, "tok-b", sa, probeTool, nil)
	if status != http.StatusNotFound || res != nil {
		t.Fatalf("B on A's session: %d %+v, want 404", status, res)
	}
	if len(e.src.askedPIDs()) != asked || len(e.fake.seen()) != seen {
		t.Fatal("a call on a foreign session id reached the backend")
	}
}

// One audit line per call: pseudonymous who, tool, outcome, duration. No raw
// pid, no arguments.
func TestMCPAuditLine(t *testing.T) {
	e := newMCPEnv(t, nil)
	sa := e.open(t, "tok-a")
	e.call(t, "tok-a", sa, probeTool, map[string]any{"query": "CANARY-ARGUMENT"})
	// Unlinked user: a refusal text, outcome error (not ok).
	e.src.mu.Lock()
	delete(e.src.clients, "prn_A")
	e.src.mu.Unlock()
	_, res, _ := e.call(t, "tok-a", sa, probeTool, nil)
	if res == nil || !res.IsError || !strings.Contains(resultText(res), "not linked") {
		t.Fatalf("unlinked: %+v", res)
	}

	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(e.audit.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("audit line %q: %v", l, err)
		}
		lines = append(lines, m)
	}
	if len(lines) != 2 {
		t.Fatalf("want 2 audit lines, got %d:\n%s", len(lines), e.audit.String())
	}
	if lines[0]["outcome"] != "ok" || lines[1]["outcome"] != "error" {
		t.Fatalf("outcomes: %v %v", lines[0]["outcome"], lines[1]["outcome"])
	}
	for _, m := range lines {
		if m["tool"] != probeTool || m["who"] != testWho("prn_A") || m["msg"] != "tool_call" {
			t.Fatalf("audit line %v", m)
		}
		if _, ok := m["duration_ms"]; !ok {
			t.Fatalf("no duration: %v", m)
		}
	}
	all := e.audit.String()
	for _, leak := range []string{"prn_A", "CANARY-ARGUMENT", "Bearer", "tok-a"} {
		if strings.Contains(all, leak) {
			t.Fatalf("audit log contains %q:\n%s", leak, all)
		}
	}
}

// SEAM(C2-3): a per-call decision that errors refuses the call before the
// backend, never lets it through.
func TestMCPCallAdmitterFailsClosed(t *testing.T) {
	deny := true
	e := newMCPEnv(t, func(context.Context, string) error {
		if deny {
			return errors.New("consent unreadable")
		}
		return nil
	})
	sa := e.open(t, "tok-a")
	if _, res, _ := e.call(t, "tok-a", sa, probeTool, nil); res != nil && !res.IsError {
		t.Fatal("call passed a refusing admitter")
	}
	if len(e.src.askedPIDs()) != 0 || len(e.fake.seen()) != 0 {
		t.Fatal("a refused call reached the backend")
	}
	if !strings.Contains(e.audit.String(), `"outcome":"refused"`) {
		t.Fatalf("no refused audit line:\n%s", e.audit.String())
	}
	deny = false
	if _, res, body := e.call(t, "tok-a", sa, probeTool, nil); res == nil || res.IsError {
		t.Fatalf("admitted call failed: %s", body)
	}
}
