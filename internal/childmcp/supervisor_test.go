package childmcp

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

var fakeBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fakechild")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fakeBin = filepath.Join(dir, "fakechild")
	out, err := exec.Command("go", "build", "-o", fakeBin, "./testdata/fakechild").CombinedOutput() //nolint:gosec // test helper build
	if err != nil {
		fmt.Fprintf(os.Stderr, "build fakechild: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type harness struct {
	sup    *Supervisor
	srv    *server.MCPServer
	cl     *client.Client
	logs   *syncBuffer
	cancel context.CancelFunc
}

func startHarness(t *testing.T, env []string, tweak func(*Config)) *harness {
	t.Helper()
	logs := &syncBuffer{}
	srv := server.NewMCPServer("glovo-home-test", "0", server.WithToolCapabilities(true))
	cfg := Config{
		Command: fakeBin, Env: env, MinBackoff: 50 * time.Millisecond, MaxBackoff: 200 * time.Millisecond,
		InitTimeout: 10 * time.Second, Log: slog.New(slog.NewTextHandler(logs, nil)),
	}
	if tweak != nil {
		tweak(&cfg)
	}
	sup := New(cfg, srv)
	ctx, cancel := context.WithCancel(context.Background())
	go sup.Run(ctx)
	h := &harness{sup: sup, srv: srv, logs: logs, cancel: cancel}
	t.Cleanup(func() {
		cancel()
		sup.Wait()
	})
	cl, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Start(ctx); err != nil {
		t.Fatal(err)
	}
	init := mcp.InitializeRequest{}
	init.Params.ProtocolVersion = mcp.LATEST_LEGACY_PROTOCOL_VERSION
	if _, err := cl.Initialize(ctx, init); err != nil {
		t.Fatal(err)
	}
	h.cl = cl
	return h
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (h *harness) ready() bool { return h.sup.Ready(context.Background()) == nil }

func (h *harness) call(name string, args map[string]any) (*mcp.CallToolResult, error) {
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return h.cl.CallTool(ctx, req)
}

func (h *harness) listed(t *testing.T) []string {
	t.Helper()
	res, err := h.cl.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for i := range res.Tools {
		names = append(names, res.Tools[i].Name)
	}
	slices.Sort(names)
	return names
}

func TestMirrorsToolsAndPassesCallsThrough(t *testing.T) {
	h := startHarness(t, []string{"FAKE_MARK=passed"}, nil)
	waitFor(t, "ready", h.ready)

	want := []string{"add_tool", "crash", "echo", "rpc_error", "slow", "tool_error"}
	if got := h.listed(t); !slices.Equal(got, want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}
	// The input schema is the child's, not a rebuilt one.
	res, _ := h.cl.ListTools(context.Background(), mcp.ListToolsRequest{})
	for i := range res.Tools {
		tl := &res.Tools[i]
		if tl.Name == "echo" {
			if tl.Description != "echo text" || !slices.Contains(tl.InputSchema.Required, "text") {
				t.Fatalf("echo schema not mirrored: %+v", tl)
			}
		}
	}

	out, err := h.call("echo", map[string]any{"text": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if out.IsError || len(out.Content) != 1 || out.Content[0].(mcp.TextContent).Text != "hello" {
		t.Fatalf("echo result = %+v", out)
	}
	sc, _ := out.StructuredContent.(map[string]any)
	if sc["echo"] != "hello" || sc["env"] != "passed" {
		t.Fatalf("structured content = %#v (env passthrough)", out.StructuredContent)
	}

	out, err = h.call("tool_error", nil)
	if err != nil || !out.IsError || out.Content[0].(mcp.TextContent).Text != "upstream said no" {
		t.Fatalf("tool_error = %+v, %v", out, err)
	}

	out, err = h.call("rpc_error", nil)
	if err == nil && (out == nil || !out.IsError) {
		t.Fatalf("rpc_error became a success: %+v", out)
	}
}

func TestBlockedToolsNeverMirrored(t *testing.T) {
	h := startHarness(t, nil, func(c *Config) { c.ExtraBlocked = []string{"slow"} })
	waitFor(t, "ready", h.ready)
	got := h.listed(t)
	for _, n := range []string{"glovo_checkout", "glovo_place_order", "slow"} {
		if slices.Contains(got, n) {
			t.Fatalf("%s mirrored: %v", n, got)
		}
		if out, err := h.call(n, nil); err == nil && (out == nil || !out.IsError) {
			t.Fatalf("call to blocked %s succeeded: %+v", n, out)
		}
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "tool=glovo_checkout") || !strings.Contains(logs, "tool=glovo_place_order") {
		t.Fatalf("refusals not logged:\n%s", logs)
	}
}

func TestChildCrashIsAnErrorThenRestarts(t *testing.T) {
	h := startHarness(t, nil, nil)
	waitFor(t, "ready", h.ready)

	out, err := h.call("crash", nil)
	if err == nil && (out == nil || !out.IsError) {
		t.Fatalf("call that crashed the child returned success: %+v", out)
	}
	waitFor(t, "restart", h.ready)
	out, err = h.call("echo", map[string]any{"text": "again"})
	if err != nil || out.IsError {
		t.Fatalf("after restart: %+v, %v", out, err)
	}
}

func TestInFlightCallFailsWhenChildDies(t *testing.T) {
	h := startHarness(t, []string{"FAKE_SLOW=3s"}, nil)
	waitFor(t, "ready", h.ready)

	type result struct {
		out *mcp.CallToolResult
		err error
	}
	ch := make(chan result, 1)
	go func() {
		out, err := h.call("slow", nil)
		ch <- result{out, err}
	}()
	time.Sleep(300 * time.Millisecond)
	_, _ = h.call("crash", nil)
	r := <-ch
	if r.err == nil && (r.out == nil || !r.out.IsError) {
		t.Fatalf("in-flight call survived a crash as success: %+v", r.out)
	}
}

func TestDownChildRefusesCalls(t *testing.T) {
	// The child exits at once every time: never ready, every call refused.
	h := startHarness(t, []string{"FAKE_EXIT_AT_START=1"}, nil)
	waitFor(t, "a failed start", func() bool { return strings.Contains(h.logs.String(), "child start failed") })
	if h.ready() {
		t.Fatal("ready without a child")
	}
	if len(h.listed(t)) != 0 {
		t.Fatal("tools listed without a child")
	}

	// Tools mirrored from an earlier child stay listed while it is down,
	// but calls fail with ErrUnavailable.
	h2 := startHarness(t, nil, func(c *Config) { c.MinBackoff = time.Hour; c.MaxBackoff = time.Hour })
	waitFor(t, "ready", h2.ready)
	_, _ = h2.call("crash", nil)
	waitFor(t, "down", func() bool { return !h2.ready() })
	out, err := h2.call("echo", map[string]any{"text": "x"})
	if err == nil && (out == nil || !out.IsError) {
		t.Fatalf("call while down succeeded: %+v", out)
	}
	msg := fmt.Sprint(err)
	if out != nil && len(out.Content) > 0 {
		msg += out.Content[0].(mcp.TextContent).Text
	}
	if !strings.Contains(msg, "unavailable") {
		t.Fatalf("down error = %q", msg)
	}
}

func TestListChangedReloads(t *testing.T) {
	h := startHarness(t, nil, nil)
	waitFor(t, "ready", h.ready)
	if _, err := h.call("add_tool", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "late_tool mirrored", func() bool { return slices.Contains(h.sup.Tools(), "late_tool") })
	out, err := h.call("late_tool", nil)
	if err != nil || out.IsError {
		t.Fatalf("late_tool: %+v %v", out, err)
	}
}

func TestDrainWaitsForInFlight(t *testing.T) {
	h := startHarness(t, []string{"FAKE_SLOW=700ms"}, nil)
	waitFor(t, "ready", h.ready)
	ch := make(chan error, 1)
	go func() {
		out, err := h.call("slow", nil)
		if err == nil && out.IsError {
			err = fmt.Errorf("tool error: %v", out.Content)
		}
		ch <- err
	}()
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	if err := h.sup.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Fatal("drain returned before the in-flight call finished")
	}
	if err := <-ch; err != nil {
		t.Fatalf("in-flight call during drain: %v", err)
	}
	if out, err := h.call("echo", map[string]any{"text": "x"}); err == nil && !out.IsError {
		t.Fatal("call accepted after drain")
	}
}

func TestChildEnvIsExplicit(t *testing.T) {
	// FAKE_MARK is set here, in the parent, and not in Config.Env: the
	// child must not see it.
	t.Setenv("FAKE_MARK", "leaked-from-parent")
	h := startHarness(t, []string{}, nil)
	waitFor(t, "ready", h.ready)
	out, err := h.call("echo", map[string]any{"text": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if sc, _ := out.StructuredContent.(map[string]any); sc["env"] != "" {
		t.Fatalf("env leaked: %#v", sc)
	}
}

func TestBlocked(t *testing.T) {
	// Every tool glovo-skill 0.2.1 ships must pass.
	for _, n := range []string{
		"glovo_get_shopping_guide", "glovo_auth_status", "glovo_get_location", "glovo_set_location",
		"glovo_get_saved_locations", "glovo_search_locations", "glovo_select_location", "glovo_login",
		"glovo_browse_stores", "glovo_get_suggestions", "glovo_get_store", "glovo_get_store_menu",
		"glovo_browse_store_catalog", "glovo_get_store_recommendations", "glovo_get_store_order_options",
		"glovo_search_store_items", "glovo_get_product", "glovo_get_basket", "glovo_get_purchase_history",
		"glovo_get_order_items", "glovo_preview_reorder", "glovo_plan_reorder", "glovo_get_order_stats",
		"glovo_analyze_order_history", "glovo_add_to_basket", "glovo_set_quantity", "glovo_remove_from_basket",
	} {
		if Blocked(n) {
			t.Errorf("%s blocked", n)
		}
	}
	for _, n := range []string{
		"glovo_checkout", "checkout", "glovo_pay", "glovo_payment_methods", "glovo_place_order",
		"glovo_submit_order", "glovo_confirm_basket", "glovo_create_order", "glovo_reorder",
		"glovo_purchase", "glovo_buy_now", "GlovoCheckout", "glovo-place-order", "glovo_charge_card",
	} {
		if !Blocked(n) {
			t.Errorf("%s not blocked", n)
		}
	}
}
