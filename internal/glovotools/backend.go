// Package glovotools serves glovo-skill 0.2.1's MCP tools natively in Go,
// composed from glovoapi and glovologic, so glovo-home can run without the
// Node child (GLOVO_HOME_BACKEND=native).
//
// The tool names, titles, descriptions and input schemas are glovo-skill's
// own (tooldefs.json, generated from the upstream server), and every result
// is shaped like the upstream one. The session file is the same file
// glovo-skill uses: it is re-read on every call, as the upstream server
// builds a fresh client per call, and token refreshes rotate it atomically
// at mode 0600.
//
// There is no checkout, payment or order tool, and New refuses to serve one:
// every name must be a childmcp.KnownTools entry and pass childmcp.Blocked.
// glovo_login is served but refuses; a new session comes from
// scripts/glovo-relogin.sh.
package glovotools

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/mctlhq/mctl-glovo/internal/authn"
	"github.com/mctlhq/mctl-glovo/internal/childmcp"
	"github.com/mctlhq/mctl-glovo/internal/glovoapi"
	"github.com/mctlhq/mctl-glovo/internal/glovologic"
	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

//go:embed tooldefs.json
var toolDefsJSON []byte

//go:embed shopping-guide.md
var shoppingGuide string

// LoginRefusal is the glovo_login result in the native backend. Signing in
// needs a browser on the owner's machine, which a gateway call must never
// start.
const LoginRefusal = "glovo_login is not available through glovo-home: it would open a browser on the host. " +
	"Sign in on the host with scripts/glovo-relogin.sh, which installs the new session and touches the reload marker."

// Unsupported lists the tools that are served (same name and schema as
// upstream) but not ported, with the reason. Calling one returns an isError
// result carrying the reason.
var Unsupported = map[string]string{
	"glovo_login": LoginRefusal,
}

// Config configures a Backend.
type Config struct {
	// SessionPath is glovo-skill's session file. The history cache lives
	// next to it, as upstream.
	SessionPath string
	// ClientOptions are passed to glovoapi.New (egress, base URL, test
	// hooks).
	ClientOptions []glovoapi.Option
	// GoogleAPIKey enables Google Places evidence in glovo_get_suggestions,
	// as GOOGLE_MAPS_API_KEY does upstream. GoogleProvider replaces the
	// Places client (tests).
	GoogleAPIKey   string
	GoogleProvider glovologic.GoogleProvider
	// Now is the history-cache clock; nil is time.Now.
	Now func() time.Time
	// Sleep replaces glovologic's real timer (tests).
	Sleep glovologic.Sleep
	// ExtraBlocked are exact tool names refused on top of childmcp.Blocked.
	ExtraBlocked []string
	// Clients switches the backend to hosted mode (C2-2): there is no
	// session file, and every call runs on the client Clients returns for
	// the pid of the verified principal in the call's context. Exactly one
	// of SessionPath and Clients must be set. In hosted mode ClientOptions
	// must be empty (the source builds the clients) and the history cache is
	// off: its file is shared by whoever uses the directory, which is the
	// wrong shape for a multi-user process.
	Clients ClientSource
}

// ClientSource resolves the hosted Glovo client of one pid.
// tokenstore.Sessions implements it.
//
// The pid is a tokenstore.PID, whose only constructor from outside that
// package is PIDFrom over a verified principal: a tool argument cannot
// name another user's session.
type ClientSource interface {
	Client(ctx context.Context, pid tokenstore.PID) (*glovoapi.Client, error)
}

type handler func(ctx context.Context, c *glovoapi.Client, a args) (any, error)

type toolDef struct {
	Name        string          `json:"name"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Execution   *struct {
		TaskSupport string `json:"taskSupport"`
	} `json:"execution"`
}

type tool struct {
	def     toolDef
	schema  *schema
	handler handler
}

// Backend is the native tool set. It is safe for concurrent use.
type Backend struct {
	cfg    Config
	client *glovoapi.Client
	tools  []*tool
	byName map[string]*tool

	mu       sync.Mutex
	draining bool
	inflight sync.WaitGroup
}

// New builds the backend. It fails if a tool definition cannot be checked
// by the validator, if a served name is not a known glovo-skill tool or is
// blocked, or if a tool has no handler.
func New(cfg Config) (*Backend, error) {
	var c *glovoapi.Client
	switch {
	case cfg.Clients != nil && cfg.SessionPath != "":
		return nil, errors.New("glovotools: SessionPath and Clients are mutually exclusive")
	case cfg.Clients != nil:
		if len(cfg.ClientOptions) > 0 {
			return nil, errors.New("glovotools: ClientOptions do not apply in hosted mode")
		}
	case cfg.SessionPath == "":
		return nil, errors.New("glovotools: SessionPath or Clients is required")
	default:
		var err error
		c, err = glovoapi.New(cfg.SessionPath, cfg.ClientOptions...)
		if err != nil {
			return nil, err
		}
	}
	var defs []toolDef
	if err := json.Unmarshal(toolDefsJSON, &defs); err != nil {
		return nil, fmt.Errorf("glovotools: tool definitions: %w", err)
	}
	known := map[string]bool{}
	for _, n := range childmcp.KnownTools {
		known[n] = true
	}
	extra := map[string]bool{}
	for _, n := range cfg.ExtraBlocked {
		extra[n] = true
	}
	b := &Backend{cfg: cfg, client: c, byName: map[string]*tool{}}
	hs := handlers(b)
	for _, d := range defs {
		if childmcp.Blocked(d.Name) || !known[d.Name] {
			return nil, fmt.Errorf("glovotools: refusing to serve tool %q", d.Name)
		}
		if extra[d.Name] {
			continue
		}
		s, err := parseSchema(d.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("glovotools: %s schema: %w", d.Name, err)
		}
		h, ok := hs[d.Name]
		if !ok {
			if reason, unsupported := Unsupported[d.Name]; unsupported {
				h = refuse(reason)
			} else {
				return nil, fmt.Errorf("glovotools: no handler for %q", d.Name)
			}
		}
		t := &tool{def: d, schema: s, handler: h}
		b.tools = append(b.tools, t)
		b.byName[d.Name] = t
	}
	for name := range hs {
		if _, ok := b.byName[name]; !ok && !extra[name] {
			return nil, fmt.Errorf("glovotools: handler %q has no tool definition", name)
		}
	}
	return b, nil
}

func refuse(reason string) handler {
	return func(context.Context, *glovoapi.Client, args) (any, error) {
		return nil, refusal(reason)
	}
}

// refusal is returned verbatim as the isError text.
type refusal string

func (r refusal) Error() string { return string(r) }

// Names returns the served tool names in upstream order.
func (b *Backend) Names() []string {
	out := make([]string, len(b.tools))
	for i, t := range b.tools {
		out[i] = t.def.Name
	}
	return out
}

// ServerTools returns the tools for server.MCPServer.SetTools.
func (b *Backend) ServerTools() []server.ServerTool {
	out := make([]server.ServerTool, 0, len(b.tools))
	for _, t := range b.tools {
		mt := mcp.NewToolWithRawSchema(t.def.Name, t.def.Description, t.def.InputSchema)
		mt.Title = t.def.Title
		if t.def.Execution != nil {
			mt.Execution = &mcp.ToolExecution{TaskSupport: mcp.TaskSupport(t.def.Execution.TaskSupport)}
		}
		name := t.def.Name
		out = append(out, server.ServerTool{Tool: mt, Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return b.Call(ctx, name, req.GetArguments())
		}})
	}
	return out
}

// Install replaces srv's tools with the native ones.
func (b *Backend) Install(srv *server.MCPServer) { srv.SetTools(b.ServerTools()...) }

// Call runs one tool. A tool failure is an isError result, like upstream;
// an error return means the call itself could not run (unknown tool,
// draining, cancelled or timed out).
func (b *Backend) Call(ctx context.Context, name string, rawArgs map[string]any) (*mcp.CallToolResult, error) {
	t, ok := b.byName[name]
	if !ok {
		return nil, fmt.Errorf("tool %q not found", name)
	}
	b.mu.Lock()
	if b.draining {
		b.mu.Unlock()
		return nil, childmcp.ErrUnavailable
	}
	b.inflight.Add(1)
	b.mu.Unlock()
	defer b.inflight.Done()

	// Hosted mode: the caller is the verified principal, nobody else. This
	// runs before argument validation, so an unauthenticated call learns
	// nothing about the tools.
	var pid tokenstore.PID
	if b.cfg.Clients != nil {
		pid = tokenstore.PIDFrom(authn.FromContext(ctx))
		if pid == (tokenstore.PID{}) {
			return nil, errUnauthenticated
		}
	}

	argv := map[string]any{}
	for k, v := range rawArgs {
		argv[k] = v
	}
	if issues := t.schema.validate(argv, nil); len(issues) > 0 {
		text, err := stringify(issues)
		if err != nil {
			return nil, err
		}
		return mcp.NewToolResultError("MCP error -32602: Input validation error: Invalid arguments for tool " + name + ": " + text), nil
	}
	// A fresh read of the session file per call, as upstream's getClient().
	// A file that cannot be read leaves the client signed out; the tool then
	// fails with the upstream auth message, and glovo_auth_status reports
	// session_error.
	client := b.client
	if b.cfg.Clients != nil {
		var err error
		client, err = b.cfg.Clients.Client(ctx, pid)
		if err != nil {
			return hostedRefusal(ctx, err)
		}
		if client == nil {
			return nil, childmcp.ErrUnavailable
		}
	} else {
		_ = b.client.Reload()
	}
	data, err := t.handler(ctx, client, args(argv))
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, errors.Join(cerr, err)
		}
		if b.cfg.Clients != nil {
			// Store sentinels also surface from inside the handler (request
			// gate, refresh coordinator): same answers as at lookup time.
			if res, ok := hostedState(err); ok {
				return res, nil
			}
			if isStoreFailure(err) {
				return nil, childmcp.ErrUnavailable
			}
		}
		return mcp.NewToolResultError(errorText(err)), nil
	}
	if s, ok := data.(textResult); ok {
		return mcp.NewToolResultText(string(s)), nil
	}
	text, err := stringify(data)
	if err != nil {
		return nil, fmt.Errorf("encode %s result: %w", name, err)
	}
	return mcp.NewToolResultText(text), nil
}

// textResult is returned as is, not JSON encoded.
type textResult string

// Ready reports the backend usable. The native backend has no child to
// wait for; Glovo itself is checked by the keepalive, not by readiness.
func (b *Backend) Ready(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.draining {
		return childmcp.ErrUnavailable
	}
	return nil
}

// Reload re-reads the session file now. Every call re-reads it anyway; this
// is what the reload marker triggers.
func (b *Backend) Reload() {
	if b.client != nil {
		_ = b.client.Reload()
	}
}

// Drain refuses new calls and waits for the ones in flight.
func (b *Backend) Drain(ctx context.Context) error {
	b.mu.Lock()
	b.draining = true
	b.mu.Unlock()
	done := make(chan struct{})
	go func() { b.inflight.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *Backend) historyCache() glovologic.HistoryCache {
	if b.cfg.Clients != nil {
		return glovologic.HistoryCache{Now: b.cfg.Now} // empty Dir: cache off
	}
	return glovologic.HistoryCache{Dir: filepath.Dir(b.cfg.SessionPath), Now: b.cfg.Now}
}
