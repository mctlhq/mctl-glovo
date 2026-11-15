package main

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/server"

	"github.com/mctlhq/mctl-glovo/internal/childmcp"
	"github.com/mctlhq/mctl-glovo/internal/config"
)

// A ReadTimeout or WriteTimeout would cut long tool calls and the GET /mcp
// event stream (net/http cancels the request context when the read
// deadline fires), so the server must set neither.
func TestServerHasNoWholeRequestTimeouts(t *testing.T) {
	srv := newHTTPServer(http.NewServeMux())
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
		t.Fatalf("ReadTimeout=%v WriteTimeout=%v, want 0", srv.ReadTimeout, srv.WriteTimeout)
	}
	if srv.ReadHeaderTimeout <= 0 {
		t.Fatal("ReadHeaderTimeout must bound slow headers")
	}
}

func TestNewBackendNativeServesToolsWithoutAChild(t *testing.T) {
	cfg := &config.Config{Backend: config.BackendNative, SessionPath: filepath.Join(t.TempDir(), "session.json")}
	srv := server.NewMCPServer("test", "0")
	b, err := newBackend(cfg, srv, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.(nativeBackend); !ok {
		t.Fatalf("backend = %T, want native", b)
	}
	if got := len(srv.ListTools()); got != len(childmcp.KnownTools) {
		t.Fatalf("%d tools installed, want %d", got, len(childmcp.KnownTools))
	}
	if err := b.Ready(context.Background()); err != nil {
		t.Fatalf("native not ready: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go b.Run(ctx)
	cancel()
	b.Wait()
}

func TestNewBackendSkillInstallsNothingBeforeTheChild(t *testing.T) {
	cfg := &config.Config{Backend: config.BackendSkill, Node: "/nonexistent/node", Server: "/nonexistent/server.mjs"}
	srv := server.NewMCPServer("test", "0")
	b, err := newBackend(cfg, srv, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := b.(skillBackend); !ok {
		t.Fatalf("backend = %T, want skill", b)
	}
	if got := len(srv.ListTools()); got != 0 {
		t.Fatalf("%d tools before the child ran", got)
	}
	if err := b.Ready(context.Background()); err == nil {
		t.Fatal("skill backend ready without a child")
	}
}

func TestNewBackendRefusesUnknown(t *testing.T) {
	if _, err := newBackend(&config.Config{Backend: "both"}, server.NewMCPServer("test", "0"), slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("unknown backend accepted")
	}
}
