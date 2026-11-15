// Package childmcp runs one long-lived stdio MCP server (glovo-skill) and
// mirrors its tools onto an mcp-go server.
//
// The child is spawned with explicit pipes; the supervisor owns cmd.Wait, so
// it sees every exit. A dead child is restarted with exponential backoff.
// While it is down, every call fails with ErrUnavailable; a call in flight
// when it dies fails with the transport error. Neither is ever turned into a
// success.
package childmcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// ErrUnavailable is returned for calls while no child is running.
var ErrUnavailable = errors.New("glovo backend is unavailable, try again shortly")

// Config configures a Supervisor.
type Config struct {
	// Command and Args start the child, e.g. node .../dist/server.mjs.
	Command string
	Args    []string
	// Env is the child's complete environment. Nothing else is inherited.
	Env []string
	// Stderr receives the child's stderr. nil discards it.
	Stderr io.Writer
	// Allowed are the exact tool names that may be mirrored; any other name
	// is refused and logged. nil means KnownTools. Blocked and ExtraBlocked
	// apply on top.
	Allowed []string
	// ExtraBlocked are exact tool names refused on top of Blocked.
	ExtraBlocked []string
	// InitTimeout bounds initialize + tools/list. Default 30s.
	InitTimeout time.Duration
	// MinBackoff and MaxBackoff bound restart delays. Defaults 1s and 30s.
	MinBackoff, MaxBackoff time.Duration
	Log                    *slog.Logger
}

type child struct {
	cmd    *exec.Cmd
	client *client.Client
	stdout *os.File
	stdin  *os.File
	// eof is closed when the transport's reader has hit EOF/error on stdout.
	eof    chan struct{}
	exited chan struct{}
	// draining is set (under Supervisor.mu) before a planned restart: new
	// calls are refused while in-flight ones finish. inflight counts calls
	// on this child; it is only incremented under Supervisor.mu's read lock
	// while draining is false.
	draining bool
	inflight atomic.Int64
}

// Supervisor owns the child process and the mirrored tool set.
type Supervisor struct {
	cfg     Config
	log     *slog.Logger
	srv     *server.MCPServer
	extra   map[string]bool
	allowed map[string]bool

	mu       sync.RWMutex
	cur      *child
	tools    []string // mirrored tool names, sorted
	loaded   bool
	stopping bool

	calls   sync.WaitGroup
	done    chan struct{}
	restart chan struct{}
}

// New returns a Supervisor that mirrors tools onto srv. Call Run to start.
func New(cfg Config, srv *server.MCPServer) *Supervisor {
	if cfg.InitTimeout == 0 {
		cfg.InitTimeout = 30 * time.Second
	}
	if cfg.MinBackoff == 0 {
		cfg.MinBackoff = time.Second
	}
	if cfg.MaxBackoff == 0 {
		cfg.MaxBackoff = 30 * time.Second
	}
	if cfg.Stderr == nil {
		cfg.Stderr = io.Discard
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	extra := map[string]bool{}
	for _, n := range cfg.ExtraBlocked {
		if n != "" {
			extra[n] = true
		}
	}
	names := cfg.Allowed
	if names == nil {
		names = KnownTools
	}
	allowed := map[string]bool{}
	for _, n := range names {
		if n != "" {
			allowed[n] = true
		}
	}
	return &Supervisor{cfg: cfg, log: log, srv: srv, extra: extra, allowed: allowed, done: make(chan struct{}), restart: make(chan struct{}, 1)}
}

// Ready reports nil only when a child is running and its tools are mirrored.
func (s *Supervisor) Ready(context.Context) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cur == nil || !s.loaded {
		return ErrUnavailable
	}
	return nil
}

// Tools returns the mirrored tool names.
func (s *Supervisor) Tools() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.tools...)
}

// Run starts the child and keeps it running until ctx is done. It returns
// after the last child has been stopped.
func (s *Supervisor) Run(ctx context.Context) {
	defer close(s.done)
	backoff := s.cfg.MinBackoff
	for {
		started := time.Now()
		c, err := s.start(ctx)
		if err != nil {
			s.log.Error("child start failed", "error", err)
		} else {
			select {
			case <-c.exited:
				s.markDown(c)
				s.log.Error("child exited", "exit", exitString(c.cmd), "uptime", time.Since(started).Round(time.Millisecond).String())
			case <-s.restart:
				s.log.Info("child restart requested")
				s.drainChild(ctx, c)
				s.terminate(c)
				if ctx.Err() != nil {
					return
				}
				backoff = s.cfg.MinBackoff
				continue
			case <-ctx.Done():
				s.stopChild(c)
				return
			}
			if time.Since(started) > time.Minute {
				backoff = s.cfg.MinBackoff
			}
		}
		if ctx.Err() != nil {
			return
		}
		s.log.Info("child restart scheduled", "in", backoff.String())
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff = min(backoff*2, s.cfg.MaxBackoff)
	}
}

// Restart asks Run to replace the child (for example after the session
// file changed). In-flight calls finish first; calls arriving meanwhile get
// ErrUnavailable. Requests coalesce: several before the restart run once.
func (s *Supervisor) Restart() {
	select {
	case s.restart <- struct{}{}:
	default:
	}
}

// Call runs one tool on the child for the gateway itself (the keepalive),
// through the same path and blocklist as a remote call.
func (s *Supervisor) Call(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error) {
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	return s.handler(name)(ctx, req)
}

// Wait waits for Run to return.
func (s *Supervisor) Wait() { <-s.done }

// Drain refuses new tool calls and waits until none is in flight, or ctx is
// done. Shutdown calls it before stopping the child.
func (s *Supervisor) Drain(ctx context.Context) error {
	// Set under the write lock: no handler can Add after this, so Wait
	// below never races a first Add.
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	ch := make(chan struct{})
	go func() { s.calls.Wait(); close(ch) }()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func exitString(cmd *exec.Cmd) string {
	if cmd.ProcessState == nil {
		return "unknown"
	}
	return cmd.ProcessState.String()
}

// eofReader closes eof once the wrapped reader returns an error, so the
// supervisor knows every byte the child wrote has been consumed.
type eofReader struct {
	r    io.Reader
	once sync.Once
	eof  chan struct{}
}

func (e *eofReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil {
		e.once.Do(func() { close(e.eof) })
	}
	return n, err
}

func (s *Supervisor) start(ctx context.Context) (*child, error) {
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		return nil, err
	}
	// Not CommandContext: shutdown closes stdin and signals the child
	// itself, so it can finish in-flight work.
	cmd := exec.Command(s.cfg.Command, s.cfg.Args...) //nolint:gosec // command comes from operator config
	// A nil Env would make exec inherit this process's environment.
	cmd.Env = append([]string{}, s.cfg.Env...)
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = s.cfg.Stderr
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{stdinR, stdinW, stdoutR, stdoutW} {
			_ = f.Close()
		}
		return nil, fmt.Errorf("start %s: %w", s.cfg.Command, err)
	}
	// The child holds its own copies now.
	_ = stdinR.Close()
	_ = stdoutW.Close()

	c := &child{cmd: cmd, stdout: stdoutR, stdin: stdinW, eof: make(chan struct{}), exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(c.exited)
	}()

	tr := transport.NewIO(&eofReader{r: stdoutR, eof: c.eof}, stdinW, nil)
	c.client = client.NewClient(tr,
		// glovo-skill speaks the initialize handshake; pinning a pre-2026-07-28
		// version skips the server/discover probe.
		client.WithProtocolVersion(mcp.LATEST_LEGACY_PROTOCOL_VERSION))
	if err := c.client.Start(ctx); err != nil {
		s.kill(c)
		return nil, fmt.Errorf("start transport: %w", err)
	}

	ictx, cancel := context.WithTimeout(ctx, s.cfg.InitTimeout)
	defer cancel()
	req := mcp.InitializeRequest{}
	req.Params.ProtocolVersion = mcp.LATEST_LEGACY_PROTOCOL_VERSION
	req.Params.ClientInfo = mcp.Implementation{Name: "mctl-glovo-home", Version: "1"}
	if _, err := c.client.Initialize(ictx, req); err != nil {
		s.kill(c)
		return nil, fmt.Errorf("initialize: %w", err)
	}

	s.mu.Lock()
	s.cur = c
	s.loaded = false
	s.mu.Unlock()

	c.client.OnNotification(func(n mcp.JSONRPCNotification) {
		if n.Method == string(mcp.MethodNotificationToolsListChanged) {
			go func() {
				lctx, cancel := context.WithTimeout(context.Background(), s.cfg.InitTimeout)
				defer cancel()
				if err := s.reload(lctx, c); err != nil {
					s.log.Error("tools reload failed", "error", err)
				}
			}()
		}
	})
	if err := s.reload(ictx, c); err != nil {
		s.mu.Lock()
		s.cur = nil
		s.mu.Unlock()
		s.kill(c)
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	s.log.Info("child ready", "pid", cmd.Process.Pid, "tools", len(s.Tools()))
	return c, nil
}

// reload lists the child's tools and replaces the mirrored set.
func (s *Supervisor) reload(ctx context.Context, c *child) error {
	res, err := c.client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return err
	}
	var mirrored []server.ServerTool
	var names []string
	for i := range res.Tools {
		t := res.Tools[i]
		if t.Name == "" {
			continue
		}
		if Blocked(t.Name) || s.extra[t.Name] {
			s.log.Warn("tool refused: not mirrored", "tool", t.Name)
			continue
		}
		if !s.allowed[t.Name] {
			s.log.Warn("tool refused: not on the allowlist (GLOVO_HOME_EXTRA_TOOLS)", "tool", t.Name)
			continue
		}
		mirrored = append(mirrored, server.ServerTool{Tool: t, Handler: s.handler(t.Name)})
		names = append(names, t.Name)
	}
	sort.Strings(names)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != c {
		return errors.New("child replaced during reload")
	}
	s.srv.SetTools(mirrored...)
	s.tools = names
	s.loaded = true
	return nil
}

// handler forwards a call verbatim to the current child.
func (s *Supervisor) handler(name string) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if Blocked(name) || s.extra[name] || !s.allowed[name] {
			return nil, fmt.Errorf("tool %q is not available through this gateway", name)
		}
		s.mu.RLock()
		c, stopping := s.cur, s.stopping
		ok := c != nil && !stopping && !c.draining
		if ok {
			s.calls.Add(1)
			c.inflight.Add(1)
		}
		s.mu.RUnlock()
		if !ok {
			return nil, ErrUnavailable
		}
		defer s.calls.Done()
		defer c.inflight.Add(-1)

		fwd := mcp.CallToolRequest{}
		fwd.Params.Name = name
		fwd.Params.Arguments = req.Params.Arguments
		fwd.Params.Meta = req.Params.Meta
		res, err := c.client.CallTool(ctx, fwd)
		if err != nil {
			if errors.Is(err, transport.ErrTransportClosed) {
				return nil, fmt.Errorf("%w: the backend stopped during the call", ErrUnavailable)
			}
			return nil, err
		}
		if res == nil {
			return nil, errors.New("glovo backend returned no result")
		}
		return res, nil
	}
}

func (s *Supervisor) markDown(c *child) {
	s.mu.Lock()
	if s.cur == c {
		s.cur = nil
		s.loaded = false
	}
	s.mu.Unlock()
	// Let the transport consume what the child wrote before exiting, then
	// release it: in-flight calls get their response or ErrTransportClosed.
	select {
	case <-c.eof:
	case <-time.After(2 * time.Second):
	}
	// client.Close closes the stdin writer (mcp-go Stdio.Close); closing
	// it here as well keeps that explicit. A second Close is a harmless
	// error.
	_ = c.client.Close()
	_ = c.stdin.Close()
	_ = c.stdout.Close()
}

// kill stops a child that never became current.
func (s *Supervisor) kill(c *child) {
	_ = c.client.Close()
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	<-c.exited
	_ = c.stdout.Close()
}

// drainChild refuses new calls on c and waits for its in-flight calls (each
// bounded by the gateway's call timeout), or for ctx.
func (s *Supervisor) drainChild(ctx context.Context, c *child) {
	s.mu.Lock()
	c.draining = true
	s.mu.Unlock()
	for c.inflight.Load() > 0 {
		select {
		case <-ctx.Done():
			return
		case <-c.exited:
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// stopChild ends the current child for shutdown.
func (s *Supervisor) stopChild(c *child) {
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	s.terminate(c)
}

// terminate closes stdin (the MCP stdio shutdown signal), then sends
// SIGTERM, then SIGKILL.
func (s *Supervisor) terminate(c *child) {
	_ = c.stdin.Close()
	select {
	case <-c.exited:
	case <-time.After(3 * time.Second):
		_ = c.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-c.exited:
		case <-time.After(3 * time.Second):
			_ = c.cmd.Process.Kill()
			<-c.exited
		}
	}
	s.markDown(c)
	s.log.Info("child stopped", "exit", exitString(c.cmd))
}
