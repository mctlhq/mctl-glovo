// Package health serves the liveness and readiness probes.
//
// Readiness is driven by a Checker: the service reports ready only when every
// dependency it needs could be observed as healthy. A check that could not run
// is a failure, never a pass.
package health

import (
	"context"
	"net/http"
	"time"
)

// Checker reports whether one dependency is usable. A nil error means it was
// observed healthy; any error, including a timeout, means not ready.
type Checker func(ctx context.Context) error

// Handler serves /livez and /readyz.
type Handler struct {
	checks  []Checker
	timeout time.Duration
}

// New returns a Handler that runs every check on /readyz, each bounded by
// timeout. With no checks, /readyz is ready.
func New(timeout time.Duration, checks ...Checker) *Handler {
	return &Handler{checks: checks, timeout: timeout}
}

// Register mounts the probes on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET /readyz", h.ready)
}

func (h *Handler) ready(w http.ResponseWriter, r *http.Request) {
	for _, check := range h.checks {
		ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
		err := check(ctx)
		cancel()
		if err != nil {
			write(w, http.StatusServiceUnavailable, "not ready")
			return
		}
	}
	write(w, http.StatusOK, "ok")
}

func write(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body + "\n"))
}
