package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/mctlhq/mctl-glovo/internal/authn"
)

// startProdServer serves h through newServer, the production server
// construction, with shortened read and write timeouts.
func startProdServer(read, write time.Duration) func(http.Handler) *httptest.Server {
	return func(h http.Handler) *httptest.Server {
		ts := httptest.NewUnstartedServer(h)
		ts.Config = newServer("", h, read, write)
		ts.Start()
		return ts
	}
}

// A tool call that outlives the server's read timeout (and the POST body
// deadline) completes: the deadline is cleared once the body is read, so
// net/http's background read cannot cancel the request context.
func TestMCPCallOutlivesServerReadTimeout(t *testing.T) {
	const (
		short = 200 * time.Millisecond
		slow  = 600 * time.Millisecond
	)
	e := newMCPEnvWith(t, nil, func(d *mcpDeps) { d.BodyReadTimeout = short },
		startProdServer(short, short))
	e.fake.mu.Lock()
	e.fake.delay = slow
	e.fake.mu.Unlock()
	sa := e.open(t, "tok-a")

	start := time.Now()
	status, res, body := e.call(t, "tok-a", sa, probeTool, nil)
	if took := time.Since(start); took < slow {
		t.Fatalf("call returned after %v, before the slow backend answered", took)
	}
	if status != 200 || res == nil || res.IsError {
		t.Fatalf("slow call did not complete: %d %s", status, body)
	}
	if !strings.Contains(e.audit.String(), `"outcome":"ok"`) {
		t.Fatalf("audit: %s", e.audit.String())
	}
}

// A POST body that stalls past the body deadline is still cut.
func TestMCPSlowBodyIsCut(t *testing.T) {
	e := newMCPEnvWith(t, nil, func(d *mcpDeps) { d.BodyReadTimeout = 150 * time.Millisecond },
		startProdServer(time.Minute, time.Minute))
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/mcp", pr)
	req.Header.Set("Authorization", "Bearer tok-a")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	go func() { _, _ = pw.Write([]byte(`{"jsonrpc"`)) }() // never finishes
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("a stalled POST body was not cut by the body deadline")
	}
}

// hostedCallTimeout (here injected short) bounds a tool call.
func TestMCPCallTimeoutBoundsACall(t *testing.T) {
	e := newMCPEnvWith(t, nil, func(d *mcpDeps) { d.CallTimeout = 100 * time.Millisecond }, nil)
	e.fake.mu.Lock()
	e.fake.delay = 2 * time.Second
	e.fake.mu.Unlock()
	sa := e.open(t, "tok-a")
	start := time.Now()
	_, res, body := e.call(t, "tok-a", sa, probeTool, nil)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("call ran %v past a 100ms call timeout", took)
	}
	if res != nil && !res.IsError {
		t.Fatalf("timed-out call reported success: %s", body)
	}
	if !strings.Contains(e.audit.String(), `"outcome":"error"`) {
		t.Fatalf("audit: %s", e.audit.String())
	}
}

// The middleware itself puts the deadline on the context, and 0 means none.
func TestAuditMiddlewareCallTimeout(t *testing.T) {
	ctx := authn.WithPrincipal(context.Background(), &authn.Principal{ID: "prn_A"})
	run := func(timeout time.Duration) (hasDeadline bool) {
		h := auditMiddleware(slog.New(slog.NewTextHandler(io.Discard, nil)), testWho, notKilled, time.Second, nil, timeout)(
			func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				_, hasDeadline = ctx.Deadline()
				return mcp.NewToolResultText("x"), nil
			})
		req := mcp.CallToolRequest{}
		req.Params.Name = probeTool
		if _, err := h(ctx, req); err != nil {
			t.Fatal(err)
		}
		return hasDeadline
	}
	if !run(time.Minute) {
		t.Fatal("no deadline with a call timeout set")
	}
	if run(0) {
		t.Fatal("deadline set with no call timeout")
	}
}

func TestHMACWho(t *testing.T) {
	if _, err := newHMACWho([]byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
	if _, err := newHMACWho(nil); err == nil {
		t.Fatal("nil key accepted")
	}
	a, _ := newHMACWho([]byte(strings.Repeat("a", 32)))
	b, _ := newHMACWho([]byte(strings.Repeat("b", 32)))
	first, second := a("prn_X"), a("prn_X")
	if first != second {
		t.Fatal("not stable")
	}
	if a("prn_X") == a("prn_Y") || a("prn_X") == b("prn_X") {
		t.Fatal("digest does not depend on pid and key")
	}
	if !strings.HasPrefix(a("prn_X"), "pid-") || strings.Contains(a("prn_X"), "prn_X") {
		t.Fatalf("shape: %s", a("prn_X"))
	}
	// no unkeyed default: routes without Who are refused
	if _, err := newMCPRoutes(mcpDeps{}); err == nil {
		t.Fatal("routes built without Who")
	}
}

func TestC2AuditKeyRequired(t *testing.T) {
	for _, k := range []string{"", "tooshort", strings.Repeat("x", minAuditKeyLen-1)} {
		m := fullEnv()
		m["GLOVO_C2_AUDIT_KEY"] = k
		_, err := loadC2(env(m))
		if err == nil || !strings.Contains(err.Error(), "GLOVO_C2_AUDIT_KEY") {
			t.Errorf("key %d bytes accepted: %v", len(k), err)
		}
		if err != nil && k != "" && strings.Contains(err.Error(), k) {
			t.Error("error leaks the key")
		}
	}
	if c, err := loadC2(env(fullEnv())); err != nil || len(c.auditKey) < minAuditKeyLen {
		t.Fatalf("%v", err)
	}
}

// The GET event stream has no body: net/http's background read is already
// running when the handler starts, so a deadline left armed (the server's
// ReadTimeout) cancels the request context and ends the stream. It must
// stay open past the server's read timeout.
func TestMCPEventStreamOutlivesServerReadTimeout(t *testing.T) {
	const short = 200 * time.Millisecond
	e := newMCPEnvWith(t, nil, func(d *mcpDeps) { d.BodyReadTimeout = short },
		startProdServer(short, short))
	sa := e.open(t, "tok-a")
	req, _ := http.NewRequest(http.MethodGet, e.ts.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer tok-a")
	req.Header.Set("Mcp-Session-Id", sa)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("GET stream: %d", resp.StatusCode)
	}
	ended := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, resp.Body)
		ended <- err
	}()
	select {
	case err := <-ended:
		t.Fatalf("event stream ended before the 600ms mark: %v", err)
	case <-time.After(3 * short):
	}
}
