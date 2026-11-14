package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/mctlhq/mctl-glovo/internal/authn"
	"github.com/mctlhq/mctl-glovo/internal/childmcp"
)

const (
	resource = "https://glovo.mctl.ai/mcp"
	issuer   = "https://api.mctl.ai"
)

var fakeBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fakechild")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fakeBin = filepath.Join(dir, "fakechild")
	out, err := exec.Command("go", "build", "-o", fakeBin, "../childmcp/testdata/fakechild").CombinedOutput() //nolint:gosec // test helper build
	if err != nil {
		fmt.Fprintf(os.Stderr, "build fakechild: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// stubVerifier maps fixed tokens to outcomes.
type stubVerifier struct{}

func (stubVerifier) Verify(_ context.Context, tok string) (*authn.Principal, error) {
	switch tok {
	case "good":
		return &authn.Principal{ID: "prn_a"}, nil
	case "good-b":
		return &authn.Principal{ID: "prn_b"}, nil
	case "stranger":
		return nil, &authn.Error{Reason: authn.ReasonNotAllowed, Status: 403}
	case "jwks-down":
		return nil, &authn.Error{Reason: authn.ReasonJWKSDown, Status: 503}
	}
	return nil, &authn.Error{Reason: authn.ReasonSignature, Status: 401}
}

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

type env struct {
	ts    *httptest.Server
	sup   *childmcp.Supervisor
	audit *syncBuffer
}

func newEnv(t *testing.T, childEnv []string) *env {
	t.Helper()
	audit := &syncBuffer{}
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewMCPServer("test", slog.New(slog.NewJSONHandler(audit, nil)), 10*time.Second)
	sup := childmcp.New(childmcp.Config{
		Command: fakeBin, Env: childEnv, MinBackoff: 50 * time.Millisecond, MaxBackoff: 100 * time.Millisecond, Log: discard,
	}, srv)
	ctx, cancel := context.WithCancel(context.Background())
	go sup.Run(ctx)
	g, err := New(Config{
		Resource: resource, AuthorizationServer: issuer, Verifier: stubVerifier{},
		MCP: NewStreamable(srv, discard), Log: discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	g.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(func() {
		ts.Close()
		cancel()
		sup.Wait()
	})
	deadline := time.Now().Add(10 * time.Second)
	for sup.Ready(context.Background()) != nil {
		if time.Now().After(deadline) {
			t.Fatal("child never ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return &env{ts: ts, sup: sup, audit: audit}
}

// reply is a fully read response.
type reply struct {
	StatusCode int
	Header     http.Header
}

func do(t *testing.T, method, url string, hdr map[string]string, body string) (reply, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return reply{StatusCode: resp.StatusCode, Header: resp.Header}, string(b)
}

const wantChallenge = `Bearer resource_metadata="https://glovo.mctl.ai/.well-known/oauth-protected-resource"`

func TestMetadataExactBytes(t *testing.T) {
	e := newEnv(t, nil)
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		resp, body := do(t, "GET", e.ts.URL+path, nil, "")
		// The exact body mctl-api serves for glovo.mctl.ai today.
		want := `{"resource":"https://glovo.mctl.ai/mcp","authorization_servers":["https://api.mctl.ai"],"scopes_supported":[]}` + "\n"
		if resp.StatusCode != 200 || body != want {
			t.Fatalf("%s: %d %q", path, resp.StatusCode, body)
		}
		for k, v := range map[string]string{
			"Content-Type": "application/json", "Cache-Control": "public, max-age=300",
			"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY",
		} {
			if got := resp.Header.Get(k); got != v {
				t.Fatalf("%s: %s = %q, want %q", path, k, got, v)
			}
		}
	}
}

func TestChallengeExactBytes(t *testing.T) {
	e := newEnv(t, nil)
	for _, tc := range []struct {
		name, auth, method string
		status             int
		challenge, body    string
	}{
		{"no header", "", "POST", 401, wantChallenge, `{"error":"unauthenticated"}` + "\n"},
		{"no header GET", "", "GET", 401, wantChallenge, `{"error":"unauthenticated"}` + "\n"},
		{"no header DELETE", "", "DELETE", 401, wantChallenge, `{"error":"unauthenticated"}` + "\n"},
		{"empty bearer", "Bearer   ", "POST", 401, wantChallenge, `{"error":"unauthenticated"}` + "\n"},
		{"basic scheme", "Basic Zm9vOmJhcg==", "POST", 401, wantChallenge, `{"error":"unauthenticated"}` + "\n"},
		{"invalid token", "Bearer nope", "POST", 401,
			wantChallenge + `, error="invalid_token", error_description="the access token is not valid for this resource"`,
			`{"error":"invalid_token","error_description":"the access token is not valid for this resource"}` + "\n"},
		{"not allowlisted", "Bearer stranger", "POST", 403, "",
			`{"error":"access_denied","error_description":"this account is not allowed on this server"}` + "\n"},
		{"jwks down", "Bearer jwks-down", "POST", 503, "",
			`{"error":"temporarily_unavailable","error_description":"cannot verify tokens right now; try again later"}` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hdr := map[string]string{"Content-Type": "application/json"}
			if tc.auth != "" {
				hdr["Authorization"] = tc.auth
			}
			resp, body := do(t, tc.method, e.ts.URL+"/mcp", hdr, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
			if resp.StatusCode != tc.status || body != tc.body || resp.Header.Get("WWW-Authenticate") != tc.challenge {
				t.Fatalf("got %d %q challenge %q", resp.StatusCode, body, resp.Header.Get("WWW-Authenticate"))
			}
		})
	}
}

func TestQueryTokenRefused(t *testing.T) {
	e := newEnv(t, nil)
	resp, _ := do(t, "POST", e.ts.URL+"/mcp?access_token=good", map[string]string{"Authorization": "Bearer good"}, "{}")
	if resp.StatusCode != 401 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestHostGuard(t *testing.T) {
	e := newEnv(t, nil)
	req, _ := http.NewRequest("POST", e.ts.URL+"/mcp", strings.NewReader("{}"))
	req.Host = "evil.example"
	req.Header.Set("Authorization", "Bearer good")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func mcpClient(t *testing.T, e *env, token string, host string) *client.Client {
	t.Helper()
	opts := []transport.StreamableHTTPCOption{transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + token})}
	if host != "" {
		opts = append(opts, transport.WithHTTPBasicClient(&http.Client{Transport: hostRewrite{host}}))
	}
	c, err := client.NewStreamableHttpClient(e.ts.URL+"/mcp", opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	init := mcp.InitializeRequest{}
	init.Params.ProtocolVersion = mcp.LATEST_LEGACY_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "test", Version: "0"}
	if _, err := c.Initialize(context.Background(), init); err != nil {
		t.Fatal(err)
	}
	return c
}

type hostRewrite struct{ host string }

func (h hostRewrite) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Host = h.host
	return http.DefaultTransport.RoundTrip(r)
}

func callTool(c *client.Client, name string, args map[string]any) (*mcp.CallToolResult, error) {
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.CallTool(ctx, req)
}

func TestEndToEndProxyAndAudit(t *testing.T) {
	e := newEnv(t, nil)
	// Host as the tunnel sends it: the public host over loopback.
	c := mcpClient(t, e, "good", "glovo.mctl.ai")

	res, err := c.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for i := range res.Tools {
		names = append(names, res.Tools[i].Name)
	}
	got := strings.Join(names, ",")
	if strings.Contains(got, "checkout") || strings.Contains(got, "place_order") || !strings.Contains(got, "echo") {
		t.Fatalf("tools = %s", got)
	}

	secret := "personal-address-221B"
	out, err := callTool(c, "echo", map[string]any{"text": secret})
	if err != nil || out.IsError || out.Content[0].(mcp.TextContent).Text != secret {
		t.Fatalf("echo: %+v %v", out, err)
	}
	if out, err := callTool(c, "glovo_checkout", nil); err == nil && !out.IsError {
		t.Fatal("blocked tool callable over HTTP")
	}
	out, err = callTool(c, "crash", nil)
	if err == nil && !out.IsError {
		t.Fatalf("crash returned success: %+v", out)
	}

	logs := e.audit.String()
	if strings.Contains(logs, secret) || strings.Contains(logs, "Bearer") {
		t.Fatalf("audit log leaked call data:\n%s", logs)
	}
	for _, want := range []string{
		`"msg":"tool_call","principal":"prn_a","tool":"echo","outcome":"ok"`,
		`"tool":"crash","outcome":"unavailable"`,
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("audit log lacks %s:\n%s", want, logs)
		}
	}
	if !strings.Contains(logs, `"duration_ms":`) {
		t.Fatalf("no duration:\n%s", logs)
	}
}

func TestSessionBoundToPrincipal(t *testing.T) {
	e := newEnv(t, nil)
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	hdr := map[string]string{"Authorization": "Bearer good", "Content-Type": "application/json", "Accept": "application/json, text/event-stream"}
	resp, body := do(t, "POST", e.ts.URL+"/mcp", hdr, init)
	sid := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode != 200 || sid == "" {
		t.Fatalf("initialize: %d %q sid=%q", resp.StatusCode, body, sid)
	}
	list := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	hdr["Mcp-Session-Id"] = sid
	hdr["Mcp-Protocol-Version"] = "2025-06-18"
	if resp, body := do(t, "POST", e.ts.URL+"/mcp", hdr, list); resp.StatusCode != 200 {
		t.Fatalf("owner list: %d %s", resp.StatusCode, body)
	}
	hdr["Authorization"] = "Bearer good-b"
	if resp, _ := do(t, "POST", e.ts.URL+"/mcp", hdr, list); resp.StatusCode != 404 {
		t.Fatalf("other principal reused the session: %d", resp.StatusCode)
	}
}
