// Command glovo-home is the owner-only MCP gateway for https://glovo.mctl.ai/mcp
// on the owner's own machine.
//
// It verifies mctl-api access tokens (EdDSA, exact issuer and audience, a pid
// allowlist) and serves glovo-skill's tools from one backend, chosen by
// GLOVO_HOME_BACKEND: "skill" (the default) proxies MCP to one long-lived
// glovo-skill child over stdio and mirrors its tools; "native" serves the
// same tools from Go (internal/glovotools) with no child. Only one backend
// runs. Neither adds a tool; checkout, payment and order tools are refused
// either way.
//
// Configuration is environment only; see deploy/mac-mini/README.md.
package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/mctlhq/mctl-glovo/internal/authn"
	"github.com/mctlhq/mctl-glovo/internal/childmcp"
	"github.com/mctlhq/mctl-glovo/internal/config"
	"github.com/mctlhq/mctl-glovo/internal/gateway"
	"github.com/mctlhq/mctl-glovo/internal/glovotools"
	"github.com/mctlhq/mctl-glovo/internal/health"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("exit", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load(os.Getenv, os.Environ())
	if err != nil {
		return err
	}
	if len(cfg.AllowedPrincipals) == 0 {
		log.Warn("GLOVO_HOME_ALLOWED_PRINCIPALS is empty: every request will be refused")
	}

	verifier, err := authn.New(authn.Config{
		Issuer: config.Issuer, Resource: config.Resource, JWKSURL: config.JWKSURL,
		AllowedPrincipals: cfg.AllowedPrincipals,
	})
	if err != nil {
		return err
	}
	startCtx, cancelStart := context.WithTimeout(context.Background(), 10*time.Second)
	if err := verifier.Refresh(startCtx); err != nil {
		// Not fatal: readiness stays false and requests get 503 until a
		// refresh succeeds.
		log.Warn("initial jwks fetch failed", "error", err)
	}
	cancelStart()

	audit := log.With("log", "audit")
	mcpSrv := gateway.NewMCPServer(version, audit, cfg.CallTimeout)
	sup, err := newBackend(cfg, mcpSrv, log)
	if err != nil {
		return err
	}

	streamable := gateway.NewStreamable(mcpSrv, log.With("component", "mcp"))
	gw, err := gateway.New(gateway.Config{
		Resource: config.Resource, AuthorizationServer: config.Issuer,
		Verifier: verifier, MCP: streamable, Log: log.With("component", "http"),
	})
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	health.New(2*time.Second, sup.Ready, verifier.Ready).Register(mux)
	gw.Register(mux)

	srv := newHTTPServer(mux)
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}

	childCtx, stopChild := context.WithCancel(context.Background())
	defer stopChild()
	go sup.Run(childCtx)

	// Session hot-reload: scripts/glovo-relogin.sh touches the reload
	// marker after installing a new session. The skill backend restarts the
	// child; the native backend re-reads the session (it also re-reads it on
	// every call). The session file itself is not watched: it is rewritten
	// on every token refresh.
	if cfg.ReloadMarker != "" {
		go childmcp.WatchSession(childCtx, cfg.ReloadMarker, 5*time.Second, log.With("component", "session"), sup.Reload)
	}
	if cfg.Keepalive > 0 {
		klog := log.With("component", "keepalive")
		ka := &childmcp.Keepalive{
			Interval: cfg.Keepalive,
			Call: func(ctx context.Context) (bool, error) {
				res, err := sup.Call(ctx, childmcp.KeepaliveTool, nil)
				if err != nil {
					return false, err
				}
				return childmcp.ResultOK(res.IsError, len(res.Content)), nil
			},
			Alert: childmcp.ExecAlert(cfg.AlertCmd, klog),
			Log:   klog,
		}
		go ka.Run(childCtx)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", ln.Addr().String(), "version", version, "allowed_principals", len(cfg.AllowedPrincipals))
		errc <- srv.Serve(ln)
	}()

	select {
	case err := <-errc:
		stopChild()
		sup.Wait()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	// A second signal during a hung shutdown kills the process.
	stop()
	log.Info("shutting down")

	shutdown, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	// 1. Stop accepting connections; idle ones close, active ones continue.
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- srv.Shutdown(shutdown) }()
	// 2. Refuse new tool calls and let in-flight ones finish.
	if err := sup.Drain(shutdown); err != nil {
		log.Warn("in-flight tool calls did not finish", "error", err)
	}
	// 3. End the MCP sessions, which closes the long-lived GET streams that
	//    would otherwise hold srv.Shutdown until its deadline.
	if err := streamable.Shutdown(shutdown); err != nil {
		log.Warn("mcp shutdown", "error", err)
	}
	err = <-shutdownDone
	// 4. Stop the child.
	stopChild()
	sup.Wait()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("stopped")
	return nil
}

// backend is the tool source behind the gateway: the glovo-skill child or
// the native Go tools.
type backend interface {
	Run(ctx context.Context)
	Wait()
	Ready(ctx context.Context) error
	Call(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error)
	Reload()
	Drain(ctx context.Context) error
}

type skillBackend struct{ *childmcp.Supervisor }

// Reload restarts the child so it reads the new session.
func (s skillBackend) Reload() { s.Restart() }

type nativeBackend struct {
	*glovotools.Backend
	done chan struct{}
}

func (n nativeBackend) Run(ctx context.Context) {
	<-ctx.Done()
	close(n.done)
}

func (n nativeBackend) Wait() { <-n.done }

// newBackend builds the one backend cfg selects and registers its tools on
// srv: the native tools at once, the child's once it is running. The other
// backend is never constructed.
func newBackend(cfg *config.Config, srv *server.MCPServer, log *slog.Logger) (backend, error) {
	switch cfg.Backend {
	case config.BackendNative:
		if len(cfg.ExtraTools) > 0 {
			log.Warn("GLOVO_HOME_EXTRA_TOOLS is ignored by the native backend: it serves only the tools it implements")
		}
		b, err := glovotools.New(glovotools.Config{
			SessionPath:  cfg.SessionPath,
			GoogleAPIKey: cfg.GoogleMapsAPIKey,
			ExtraBlocked: cfg.ExtraBlocked,
		})
		if err != nil {
			return nil, err
		}
		b.Install(srv)
		log.Info("backend", "backend", config.BackendNative, "tools", len(b.Names()))
		return nativeBackend{Backend: b, done: make(chan struct{})}, nil
	case config.BackendSkill:
		stderr := io.Writer(io.Discard)
		if cfg.ChildStderr {
			stderr = os.Stderr
		}
		log.Info("backend", "backend", config.BackendSkill)
		return skillBackend{childmcp.New(childmcp.Config{
			Command: cfg.Node, Args: []string{cfg.Server}, Env: cfg.ChildEnv, Stderr: stderr,
			Allowed:      append(append([]string{}, childmcp.KnownTools...), cfg.ExtraTools...),
			ExtraBlocked: cfg.ExtraBlocked, Log: log.With("component", "child"),
		}, srv)}, nil
	default:
		return nil, errors.New("unknown backend " + cfg.Backend)
	}
}

// newHTTPServer returns the listener-facing server.
func newHTTPServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		// No ReadTimeout or WriteTimeout: either one would cut a tool call
		// longer than the timeout (net/http cancels the request context
		// when the read deadline fires) and the long-lived GET /mcp event
		// stream. Headers are bounded by ReadHeaderTimeout, POST bodies by
		// the gateway's 1 MiB MaxBytesReader, tool calls by
		// GLOVO_HOME_CALL_TIMEOUT.
		IdleTimeout: 120 * time.Second,
	}
}
