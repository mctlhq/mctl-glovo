// Command mctl-glovo serves the Glovo Basket MCP for glovo.mctl.ai.
//
// It serves the health probes and, behind GLOVO_C2_ENABLED (off by
// default), the account-link flow of slice C2-1b (internal/linkflow) and the
// hosted MCP routes of slice C2-2 (/mcp and the RFC 9728 metadata). The
// mctl-api join client arrives in a later change (see the migration plan in
// README.md).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mctlhq/mctl-glovo/internal/health"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("exit", "error", err)
		os.Exit(1)
	}
}

const (
	// serverReadTimeout and serverWriteTimeout are the whole-request limits
	// of the link flow and the probes. The MCP routes lift them per request
	// (noDeadline).
	serverReadTimeout  = 15 * time.Second
	serverWriteTimeout = 30 * time.Second
)

// newServer is the one place the HTTP server is configured. ReadHeaderTimeout
// stays for slowloris protection on every route; the read and write timeouts
// are lifted per request on the MCP routes by noDeadline.
func newServer(addr string, h http.Handler, read, write time.Duration) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       read,
		WriteTimeout:      write,
		IdleTimeout:       60 * time.Second,
	}
}

func run(log *slog.Logger) error {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	c2, err := loadC2(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	app, closeLink, err := buildLink(ctx, c2, log)
	if err != nil {
		return err
	}
	defer closeLink()
	log.Info("c2 link flow and mcp routes", "enabled", c2.enabled)

	mux := http.NewServeMux()
	health.New(2*time.Second, app.ready...).Register(mux)
	app.Register(mux)

	srv := newServer(addr, mux, serverReadTimeout, serverWriteTimeout)

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	// Restore default signal handling: a second SIGINT/SIGTERM during a hung
	// shutdown kills the process instead of being swallowed.
	stop()

	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Stop accepting connections while the MCP surface refuses new tool
	// calls, waits for the ones in flight and ends its sessions: the open
	// GET streams would otherwise hold srv.Shutdown until its deadline.
	done := make(chan error, 1)
	go func() { done <- srv.Shutdown(shutdown) }()
	mcpErr := app.Shutdown(shutdown)
	return errors.Join(<-done, mcpErr)
}
