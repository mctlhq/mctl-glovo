package childmcp

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// alertScript writes a script that records its argv to a file.
func alertScript(t *testing.T) (cmd, out string) {
	t.Helper()
	dir := t.TempDir()
	out = filepath.Join(dir, "alert.out")
	cmd = filepath.Join(dir, "alert.sh")
	script := "#!/bin/sh\nprintf '%s|%s' \"$#\" \"$1\" > \"" + out + "\"\n"
	if err := os.WriteFile(cmd, []byte(script), 0o700); err != nil { //nolint:gosec // test script must be executable
		t.Fatal(err)
	}
	return cmd, out
}

func keepaliveFor(h *harness, alertCmd string, logs io.Writer) *Keepalive {
	log := slog.New(slog.NewTextHandler(logs, nil))
	return &Keepalive{
		Interval: time.Hour,
		Call: func(ctx context.Context) (bool, error) {
			res, err := h.sup.Call(ctx, KeepaliveTool, nil)
			if err != nil {
				return false, err
			}
			return ResultOK(res.IsError, len(res.Content)), nil
		},
		Alert: ExecAlert(alertCmd, log),
		Log:   log,
	}
}

func TestKeepalive(t *testing.T) {
	for _, tc := range []struct {
		name, basket, outcome, alert string
		down                         bool
	}{
		{"success, no alert", "", "ok", "", false},
		{"auth failure alerts", "fail", "failed", AlertKeepaliveFailed, false},
		{"empty result is not ok", "empty", "failed", AlertKeepaliveFailed, false},
		{"child down alerts", "", "unavailable", AlertKeepaliveUnavailable, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startHarness(t, []string{"FAKE_BASKET=" + tc.basket}, func(c *Config) {
				c.MinBackoff, c.MaxBackoff = time.Hour, time.Hour
			})
			waitFor(t, "ready", h.ready)
			if tc.down {
				_, _ = h.call("crash", nil)
				waitFor(t, "down", func() bool { return !h.ready() })
			}
			cmd, out := alertScript(t)
			logs := &syncBuffer{}
			if got := keepaliveFor(h, cmd, logs).Once(context.Background()); got != tc.outcome {
				t.Fatalf("outcome %q, want %q", got, tc.outcome)
			}
			b, err := os.ReadFile(out) //nolint:gosec // test file
			if tc.alert == "" {
				if err == nil {
					t.Fatalf("alert ran on success: %q", b)
				}
			} else if string(b) != "1|"+tc.alert {
				t.Fatalf("alert argv = %q, %v", b, err)
			}
			l := logs.String()
			if !strings.Contains(l, "outcome="+tc.outcome) || strings.Contains(l, "baskets") || strings.Contains(l, "Run glovo_login") {
				t.Fatalf("keepalive log:\n%s", l)
			}
		})
	}
}

func TestWatchSessionStatOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	var changes atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go WatchSession(ctx, path, 20*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)), func() { changes.Add(1) })

	time.Sleep(100 * time.Millisecond)
	if changes.Load() != 0 {
		t.Fatal("change reported with no change")
	}
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "create seen", func() bool { return changes.Load() == 1 })
	time.Sleep(100 * time.Millisecond)
	if n := changes.Load(); n != 1 {
		t.Fatalf("one write reported %d times", n)
	}
	if err := os.WriteFile(path, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "rewrite seen", func() bool { return changes.Load() == 2 })
}

func pidOf(t *testing.T, h *harness) string {
	t.Helper()
	out, err := h.call("pid", nil)
	if err != nil || out.IsError {
		t.Fatalf("pid: %+v %v", out, err)
	}
	return out.Content[0].(mcp.TextContent).Text
}

func TestRestartReplacesChildAfterInFlight(t *testing.T) {
	// Longer than terminate's whole grace (3s to SIGTERM, 3s more to
	// SIGKILL): without the drain, the restart would kill this call.
	h := startHarness(t, []string{"FAKE_SLOW=7s"}, nil)
	waitFor(t, "ready", h.ready)
	before := pidOf(t, h)

	ch := make(chan error, 1)
	go func() {
		req := mcp.CallToolRequest{}
		req.Params.Name = "slow"
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		out, err := h.cl.CallTool(ctx, req)
		if err == nil && out.IsError {
			err = os.ErrInvalid
		}
		ch <- err
	}()
	time.Sleep(150 * time.Millisecond)
	h.sup.Restart()
	if err := <-ch; err != nil {
		t.Fatalf("in-flight call broken by restart: %v", err)
	}
	waitFor(t, "new child", func() bool {
		if !h.ready() {
			return false
		}
		out, err := h.call("pid", nil)
		return err == nil && !out.IsError && out.Content[0].(mcp.TextContent).Text != before
	})
}
