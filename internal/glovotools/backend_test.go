package glovotools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/mctlhq/mctl-glovo/internal/childmcp"
)

func caseByName(t *testing.T, s *spec, name string) caseSpec {
	t.Helper()
	for _, c := range s.Cases {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no case %q", name)
	return caseSpec{}
}

// tools/list must be glovo-skill's own: name, title, description, input
// schema and execution, nothing added.
func TestToolsListMatchesUpstream(t *testing.T) {
	h := newHarness(t, loadSpec(t), caseSpec{})
	srv := server.NewMCPServer("test", "0")
	h.backend.Install(srv)
	resp := srv.HandleMessage(context.Background(), json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, tl := range got.Result.Tools {
		// mcp-go always writes an annotations object; upstream has none.
		// An empty one carries nothing; a filled one would be an addition.
		if a, ok := tl["annotations"].(map[string]any); ok && len(a) == 0 {
			delete(tl, "annotations")
		}
	}
	raw, err := os.ReadFile("testdata/golden/tools.json")
	if err != nil {
		t.Fatal(err)
	}
	var want []map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	byName := func(ts []map[string]any) map[string]map[string]any {
		m := map[string]map[string]any{}
		for _, x := range ts {
			m[x["name"].(string)] = x
		}
		return m
	}
	if !reflect.DeepEqual(byName(got.Result.Tools), byName(want)) {
		t.Fatalf("tools/list differs from upstream:\n got %s\nwant %s", pretty(got.Result.Tools), pretty(want))
	}
}

// Every served name is a known glovo-skill tool and passes Blocked, and the
// native set is exactly the allowlist.
func TestEveryNativeNamePassesBlocked(t *testing.T) {
	h := newHarness(t, loadSpec(t), caseSpec{})
	names := h.backend.Names()
	for _, n := range names {
		if childmcp.Blocked(n) {
			t.Errorf("%s is blocked", n)
		}
	}
	want := append([]string(nil), childmcp.KnownTools...)
	got := append([]string(nil), names...)
	sort.Strings(want)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("native tools = %v, want KnownTools %v", got, want)
	}
}

func withToolDefs(t *testing.T, defs string) {
	t.Helper()
	old := toolDefsJSON
	toolDefsJSON = []byte(defs)
	t.Cleanup(func() { toolDefsJSON = old })
}

func TestNewRefusesUnsafeOrUnknownTools(t *testing.T) {
	for name, tc := range map[string]struct{ defs, want string }{
		// A blocked or unknown name must be refused by the name check
		// itself, not only later because it happens to have no handler.
		"blocked":        {`[{"name":"glovo_checkout","inputSchema":{"type":"object","properties":{}}}]`, "refusing to serve"},
		"unknown":        {`[{"name":"glovo_new_read_tool","inputSchema":{"type":"object","properties":{}}}]`, "refusing to serve"},
		"schema keyword": {`[{"name":"glovo_auth_status","inputSchema":{"type":"object","properties":{},"additionalProperties":false}}]`, "schema"},
	} {
		t.Run(name, func(t *testing.T) {
			withToolDefs(t, tc.defs)
			_, err := New(Config{SessionPath: filepath.Join(t.TempDir(), "session.json")})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New = %v, want a %q refusal", err, tc.want)
			}
		})
	}
}

func TestExtraBlockedIsNotServed(t *testing.T) {
	b, err := New(Config{SessionPath: filepath.Join(t.TempDir(), "session.json"), ExtraBlocked: []string{"glovo_add_to_basket"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range b.Names() {
		if n == "glovo_add_to_basket" {
			t.Fatal("an extra-blocked tool is served")
		}
	}
	if _, err := b.Call(context.Background(), "glovo_add_to_basket", map[string]any{}); err == nil {
		t.Fatal("an extra-blocked tool can be called")
	}
}

// glovo_login is listed with upstream's schema but refuses: it must never
// open a browser, and it touches neither the network nor the session.
func TestLoginRefuses(t *testing.T) {
	s := loadSpec(t)
	c := caseByName(t, s, "get_basket_store")
	h := newHarness(t, s, c)
	before := readState(t, h.session)
	for _, a := range []map[string]any{{}, {"timeout_ms": 1000.0}} {
		res, err := h.backend.Call(context.Background(), "glovo_login", a)
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError || resultText(res) != LoginRefusal {
			t.Fatalf("glovo_login = %v %q, want the refusal", res.IsError, resultText(res))
		}
		if !strings.Contains(LoginRefusal, "scripts/glovo-relogin.sh") {
			t.Fatal("the refusal must point to scripts/glovo-relogin.sh")
		}
	}
	if n := len(h.fake.requests()); n != 0 {
		t.Fatalf("glovo_login sent %d requests", n)
	}
	if after := readState(t, h.session); !reflect.DeepEqual(before, after) {
		t.Fatal("glovo_login changed the session file")
	}
}

func TestUnsupportedToolsReturnErrors(t *testing.T) {
	if got := keys(Unsupported); !reflect.DeepEqual(got, []string{"glovo_login"}) {
		t.Fatalf("Unsupported = %v", got)
	}
	h := newHarness(t, loadSpec(t), caseSpec{})
	for name, reason := range Unsupported {
		res, err := h.backend.Call(context.Background(), name, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError || resultText(res) != reason {
			t.Fatalf("%s = %v %q", name, res.IsError, resultText(res))
		}
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// A failed read is an error result, and it is not remembered: the same
// call succeeds once Glovo answers again.
func TestErrorsAreNotCached(t *testing.T) {
	s := loadSpec(t)
	c := caseByName(t, s, "get_basket_store")
	ok := c.Routes[0].Replies[0]
	c.Routes[0].Replies = []reply{{Status: 500, JSON: json.RawMessage(`{"error":{"exceptionName":"TEST_FAILURE"}}`)}, ok}
	h := newHarness(t, s, c)
	first, err := h.backend.Call(context.Background(), c.Tool, c.Args)
	if err != nil || !first.IsError {
		t.Fatalf("first call = %v, %v; want an isError result", first, err)
	}
	second, err := h.backend.Call(context.Background(), c.Tool, c.Args)
	if err != nil || second.IsError {
		t.Fatalf("second call = %q, %v; want success", resultText(second), err)
	}
}

// The session file is re-read on every call, so a session installed by
// scripts/glovo-relogin.sh is used at once.
func TestSessionIsReadPerCall(t *testing.T) {
	s := loadSpec(t)
	h := newHarness(t, s, caseSpec{Session: json.RawMessage("null")})
	call := func() map[string]any {
		res, err := h.backend.Call(context.Background(), "glovo_auth_status", nil)
		if err != nil || res.IsError {
			t.Fatalf("auth status: %v %v", err, res)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(resultText(res)), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if call()["signed_in"] != false {
		t.Fatal("signed in without a session file")
	}
	writeFile(t, h.session, s.Sessions["default"])
	if call()["signed_in"] != true {
		t.Fatal("a newly installed session was not read")
	}
}

// A refresh rotates the session file atomically at 0600 and leaves no
// temporary file behind; concurrent expired calls refresh once.
func TestRefreshRotatesSessionOnce(t *testing.T) {
	s := loadSpec(t)
	c := caseByName(t, s, "get_basket_refresh")
	h := newHarness(t, s, c)
	errc := make(chan error, 4)
	for range 4 {
		go func() {
			res, err := h.backend.Call(context.Background(), c.Tool, c.Args)
			if err == nil && res.IsError {
				err = errors.New(resultText(res))
			}
			errc <- err
		}()
	}
	for range 4 {
		if err := <-errc; err != nil {
			t.Fatal(err)
		}
	}
	refreshes := 0
	for _, r := range h.fake.requests() {
		if r.URI == "/oauth/refresh" {
			refreshes++
		}
	}
	if refreshes != 1 {
		t.Fatalf("%d refreshes, want 1", refreshes)
	}
	st := readState(t, h.session)
	if st.Mode != "600" || st.JSON.(map[string]any)["refreshToken"] != "refresh-test-new" {
		t.Fatalf("session after refresh: %s", pretty(st))
	}
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "session.json" {
			t.Errorf("left behind %s", e.Name())
		}
	}
}

func TestDrainRefusesNewCalls(t *testing.T) {
	h := newHarness(t, loadSpec(t), caseSpec{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.backend.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.backend.Call(ctx, "glovo_auth_status", nil); !errors.Is(err, childmcp.ErrUnavailable) {
		t.Fatalf("call after drain: %v", err)
	}
	if err := h.backend.Ready(ctx); err == nil {
		t.Fatal("ready after drain")
	}
}

// A cancelled call is a call error (the audit line says timeout), not an
// isError result.
func TestCancelledCallIsAnError(t *testing.T) {
	s := loadSpec(t)
	c := caseByName(t, s, "get_basket_store")
	h := newHarness(t, s, c)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.backend.Call(ctx, c.Tool, c.Args); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
