package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/mctlhq/mctl-glovo/internal/authn"
	"github.com/mctlhq/mctl-glovo/internal/config"
	"github.com/mctlhq/mctl-glovo/internal/gateway"
	"github.com/mctlhq/mctl-glovo/internal/glovotools"
	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

// hostedInstructions replace glovo-home's: a hosted account is linked at the
// link page, there is no browser login to start.
const hostedInstructions = `Use Glovo read-only tools before mutating the real basket.

Do not checkout or pay. Basket tools only prepare the user's real Glovo basket for later human review.
If a tool says your Glovo account is not linked or expired, link it again at https://glovo.mctl.ai/link.`

// hostedCallTimeout bounds one tool call. The HTTP server has no write
// timeout on the MCP routes (see noDeadline), so this is the bound.
const hostedCallTimeout = 3 * time.Minute

// auditWho turns a verified principal id into what the audit line records.
// It is a seam: the masked MCTL identity helper (slice build-infra,
// tokenstore.Config.MaskIdentity) needs mctl-api and can replace it later.
// The default is a one-way short digest: stable enough to follow one user
// through the log, not an id anyone can paste into another system.
type auditWho func(pid string) string

// hashWho is the default auditWho.
func hashWho(pid string) string {
	sum := sha256.Sum256([]byte(pid))
	return "pid-" + hex.EncodeToString(sum[:6])
}

// callAdmitter is the seam of the mctl-api join/consent client (slice C2-3):
// a per-call access decision for the verified pid. Slice C2-2 leaves it nil,
// so the verified principal alone decides (the token verifier has already
// applied the allowlist and, in consent mode, the consent check). When set,
// any error refuses the call before the backend, so a decision that could
// not be read is a refusal, never a pass.
type callAdmitter func(ctx context.Context, pid string) error

// mcpDeps are the inputs of the hosted MCP surface.
type mcpDeps struct {
	Verifier gateway.Verifier
	Clients  glovotools.ClientSource
	// Audit receives one line per tool call. Nil is refused: a call must not
	// run unaudited.
	Audit   *slog.Logger
	Log     *slog.Logger
	Who     auditWho
	Admit   callAdmitter
	Version string
}

// mcpRoutes is the hosted MCP surface. A nil *mcpRoutes is the flag-off
// state: it registers nothing, so every MCP path is a 404.
type mcpRoutes struct {
	gw         *gateway.Gateway
	backend    *glovotools.Backend
	streamable *server.StreamableHTTPServer
}

func newMCPRoutes(d mcpDeps) (*mcpRoutes, error) {
	if d.Verifier == nil || d.Clients == nil || d.Audit == nil {
		return nil, errors.New("mcp: verifier, clients and audit logger are required")
	}
	if d.Who == nil {
		d.Who = hashWho
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	version := d.Version
	if version == "" {
		version = "dev"
	}
	backend, err := glovotools.New(glovotools.Config{Clients: d.Clients})
	if err != nil {
		return nil, err
	}
	srv := server.NewMCPServer("mctl-glovo", version,
		server.WithToolCapabilities(true),
		server.WithInstructions(hostedInstructions),
		server.WithRecovery(),
		server.WithToolHandlerMiddleware(auditMiddleware(d.Audit, d.Who, d.Admit, hostedCallTimeout)),
	)
	backend.Install(srv)
	streamable := gateway.NewStreamable(srv, log.With("component", "mcp"))
	gw, err := gateway.New(gateway.Config{
		Resource:            config.Resource,
		AuthorizationServer: config.Issuer,
		Verifier:            d.Verifier,
		MCP:                 noDeadline(streamable),
		Log:                 log.With("component", "http"),
		SkipLanding:         true,
	})
	if err != nil {
		return nil, err
	}
	return &mcpRoutes{gw: gw, backend: backend, streamable: streamable}, nil
}

// Register mounts /mcp and the RFC 9728 metadata.
func (m *mcpRoutes) Register(mux *http.ServeMux) {
	if m != nil {
		m.gw.Register(mux)
	}
}

// Shutdown refuses new tool calls, waits for the ones in flight and ends the
// MCP sessions, which closes the long-lived GET streams.
func (m *mcpRoutes) Shutdown(ctx context.Context) error {
	if m == nil {
		return nil
	}
	err := m.backend.Drain(ctx)
	return errors.Join(err, m.streamable.Shutdown(ctx))
}

// noDeadline lifts the server's read and write deadlines for the MCP
// routes: they would cut a GET event stream and any tool call longer than
// them. The body of a POST keeps a read deadline (the gateway also caps it
// at 1 MiB), and tool calls are bounded by hostedCallTimeout. Where the
// ResponseWriter cannot set deadlines the server's own stay in force.
func noDeadline(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		_ = rc.SetWriteDeadline(time.Time{})
		if r.Method == http.MethodPost {
			_ = rc.SetReadDeadline(time.Now().Add(30 * time.Second))
		} else {
			_ = rc.SetReadDeadline(time.Time{})
		}
		next.ServeHTTP(w, r)
	})
}

// auditMiddleware refuses a call with no usable verified principal (and one
// the admitter refuses) before any lookup, and writes one audit line per
// call: who, tool, outcome (ok, refused or error) and duration. Arguments
// and results are never logged: they carry personal data.
//
// refused: no verified principal, or the admitter said no. error: the call
// could not run (unavailable, timeout) or the tool returned an isError
// result; the result text, which for a hosted refusal is a fixed sentence,
// is deliberately not parsed here.
func auditMiddleware(audit *slog.Logger, who auditWho, admit callAdmitter, callTimeout time.Duration) server.ToolHandlerMiddleware {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (res *mcp.CallToolResult, err error) {
			start := time.Now()
			label := "none"
			outcome := "error"
			p := authn.FromContext(ctx)
			pid := tokenstore.PIDFrom(p)
			if pid != (tokenstore.PID{}) {
				label = who(pid.String())
			}
			defer func() {
				audit.Info("tool_call",
					"who", label,
					"tool", req.Params.Name,
					"outcome", outcome,
					"duration_ms", time.Since(start).Milliseconds(),
				)
			}()
			if pid == (tokenstore.PID{}) {
				outcome = "refused"
				return nil, errors.New("unauthenticated")
			}
			if admit != nil {
				if aerr := admit(ctx, pid.String()); aerr != nil {
					outcome = "refused"
					return nil, errors.New("access refused")
				}
			}
			if callTimeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, callTimeout)
				defer cancel()
			}
			res, err = next(ctx, req)
			if err == nil && res != nil && !res.IsError {
				outcome = "ok"
			}
			return res, err
		}
	}
}
