package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func serve(t *testing.T, h *Handler, path string) int {
	t.Helper()
	mux := http.NewServeMux()
	h.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, http.NoBody))
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("%s Cache-Control = %q, want no-store", path, got)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("%s Content-Type = %q", path, got)
	}
	want := "ok\n"
	if rec.Code != http.StatusOK {
		want = "not ready\n"
	}
	if got := rec.Body.String(); got != want {
		t.Errorf("%s body = %q, want %q", path, got, want)
	}
	return rec.Code
}

func TestLivez(t *testing.T) {
	failing := func(context.Context) error { return errors.New("down") }
	if got := serve(t, New(time.Second, failing), "/livez"); got != http.StatusOK {
		t.Fatalf("livez = %d, want 200 even when a dependency is down", got)
	}
}

func TestReadyz(t *testing.T) {
	ok := func(context.Context) error { return nil }
	down := func(context.Context) error { return errors.New("down") }
	slow := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	cases := []struct {
		name   string
		checks []Checker
		want   int
	}{
		{"no checks", nil, http.StatusOK},
		{"all healthy", []Checker{ok, ok}, http.StatusOK},
		{"one down", []Checker{ok, down}, http.StatusServiceUnavailable},
		// A check that could not finish is not a pass.
		{"timeout", []Checker{slow}, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := serve(t, New(20*time.Millisecond, tc.checks...), "/readyz"); got != tc.want {
				t.Fatalf("readyz = %d, want %d", got, tc.want)
			}
		})
	}
}
