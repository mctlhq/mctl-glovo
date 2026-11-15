package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/mctlhq/mctl-glovo/internal/authn"
	"github.com/mctlhq/mctl-glovo/internal/childmcp"
)

// Instructions repeat glovo-skill's own server instructions.
const Instructions = `Use Glovo read-only tools before mutating the real basket.

Do not checkout or pay. Basket tools only prepare the user's real Glovo basket for later human review.
If auth is missing or expired, call glovo_login and let the user sign in in the browser.`

// NewMCPServer returns the MCP server the child's tools are mirrored onto.
// Every tool call writes one audit line: principal, tool, outcome and
// duration. Arguments and results carry personal data and are never logged.
func NewMCPServer(version string, audit *slog.Logger, callTimeout time.Duration) *server.MCPServer {
	return server.NewMCPServer("mctl-glovo-home", version,
		server.WithToolCapabilities(true),
		server.WithInstructions(Instructions),
		server.WithRecovery(),
		server.WithToolHandlerMiddleware(auditMiddleware(audit, callTimeout)),
	)
}

func auditMiddleware(audit *slog.Logger, callTimeout time.Duration) server.ToolHandlerMiddleware {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (res *mcp.CallToolResult, err error) {
			start := time.Now()
			principal := ""
			if p := authn.FromContext(ctx); p != nil {
				principal = p.ID
			}
			if callTimeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, callTimeout)
				defer cancel()
			}
			defer func() {
				outcome := "ok"
				switch {
				case errors.Is(err, childmcp.ErrUnavailable):
					outcome = "unavailable"
				case errors.Is(err, context.DeadlineExceeded):
					outcome = "timeout"
				case err != nil:
					outcome = "error"
				case res == nil:
					outcome = "error"
				case res.IsError:
					outcome = "tool_error"
				}
				audit.Info("tool_call",
					"principal", principal,
					"tool", req.Params.Name,
					"outcome", outcome,
					"duration_ms", time.Since(start).Milliseconds(),
				)
			}()
			if principal == "" {
				// Unreachable behind authenticate; refuse rather than run an
				// unattributed call.
				return nil, errors.New("unauthenticated")
			}
			return next(ctx, req)
		}
	}
}

// NewStreamable wraps srv in the Streamable HTTP transport. Sessions are
// stateful (GET streams carry tools/list_changed). The library's
// DNS-rebinding guard is replaced by hostGuard.
func NewStreamable(srv *server.MCPServer, log *slog.Logger) *server.StreamableHTTPServer {
	return server.NewStreamableHTTPServer(srv,
		server.WithStateful(true),
		server.WithSessionIdleTTL(sessionIdle),
		server.WithDisableLocalhostProtection(true),
		server.WithHeartbeatInterval(30*time.Second),
		server.WithStreamableHTTPLogger(log),
		server.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			if p := authn.FromContext(r.Context()); p != nil {
				return authn.WithPrincipal(ctx, p)
			}
			return ctx
		}),
	)
}
