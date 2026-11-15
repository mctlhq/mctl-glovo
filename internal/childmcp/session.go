package childmcp

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"time"
)

// fileState is what the session watcher knows about the session file: only
// what os.Stat reports. The file is never opened, read or hashed here.
type fileState struct {
	exists bool
	size   int64
	mtime  int64 // UnixNano
}

func statFile(path string) (fileState, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fileState{}, nil
	}
	if err != nil {
		return fileState{}, err
	}
	return fileState{exists: true, size: fi.Size(), mtime: fi.ModTime().UnixNano()}, nil
}

// WatchSession calls onChange when path's size or mtime changes (including
// appearing or disappearing), once the new state has held for one more poll
// so a write in progress is not acted on. A failed stat is "could not
// observe": it is logged and never counted as a change.
func WatchSession(ctx context.Context, path string, every time.Duration, log *slog.Logger, onChange func()) {
	cur, err := statFile(path)
	known := err == nil
	if err != nil {
		log.Warn("session file stat failed", "error", err)
	}
	var pending *fileState
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		st, err := statFile(path)
		if err != nil {
			log.Warn("session file stat failed", "error", err)
			pending = nil
			continue
		}
		if !known {
			cur, known = st, true
			continue
		}
		if st == cur {
			pending = nil
			continue
		}
		if pending == nil || *pending != st {
			pending = &st
			continue
		}
		cur, pending = st, nil
		log.Info("session file changed: restarting the child", "exists", st.exists)
		onChange()
	}
}

// KeepaliveTool is the cheap glovo-skill call that needs a valid Glovo
// session: it goes through glovo-skill's ensureAuth, which refreshes an
// expiring access token and rotates the refresh token.
const KeepaliveTool = "glovo_get_basket"

// Alert messages passed to the alert command as its only argument. Fixed
// strings: no token, session or result content ever reaches the hook.
const (
	AlertKeepaliveFailed      = "glovo-home: Glovo session keepalive failed; re-login may be needed"
	AlertKeepaliveUnavailable = "glovo-home: Glovo session keepalive could not run; glovo-skill is unavailable"
)

// Keepalive runs fn every interval and reports a failure through alert.
// The outcome is ok only for a non-error result with content; an error, an
// isError result or an empty result is a failure ("could not observe" is
// never ok). The result itself is not logged.
type Keepalive struct {
	Interval time.Duration
	// Call runs the keepalive tool, Supervisor.Call in production.
	Call  func(ctx context.Context) (bool, error)
	Alert func(ctx context.Context, msg string)
	Log   *slog.Logger
	// Timeout bounds one keepalive call. Default 2m.
	Timeout time.Duration
}

// Run blocks until ctx is done. The first check runs after one interval.
func (k *Keepalive) Run(ctx context.Context) {
	if k.Interval <= 0 {
		return
	}
	t := time.NewTicker(k.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		k.Once(ctx)
	}
}

// Once runs one keepalive check and returns its outcome.
func (k *Keepalive) Once(ctx context.Context) string {
	timeout := k.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	ok, err := k.Call(cctx)
	outcome := "ok"
	msg := ""
	switch {
	case errors.Is(err, ErrUnavailable):
		outcome, msg = "unavailable", AlertKeepaliveUnavailable
	case err != nil:
		outcome, msg = "error", AlertKeepaliveFailed
	case !ok:
		outcome, msg = "failed", AlertKeepaliveFailed
	}
	k.Log.Info("session_keepalive", "tool", KeepaliveTool, "outcome", outcome, "duration_ms", time.Since(start).Milliseconds())
	if msg != "" && ctx.Err() == nil {
		k.Alert(ctx, msg)
	}
	return outcome
}

// ResultOK is the keepalive success rule for a tool result.
func ResultOK(isError bool, contentItems int) bool {
	return !isError && contentItems > 0
}

// ExecAlert returns an alert func that runs command with the message as its
// single argument: no shell, bounded to 30s, output discarded. An empty
// command only logs.
func ExecAlert(command string, log *slog.Logger) func(ctx context.Context, msg string) {
	return func(ctx context.Context, msg string) {
		if command == "" {
			log.Warn("alert not sent: GLOVO_HOME_ALERT_CMD is not set", "alert", msg)
			return
		}
		actx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		err := exec.CommandContext(actx, command, msg).Run() //nolint:gosec // operator-configured hook, fixed argv, no shell
		if err != nil {
			log.Error("alert command failed", "error", err)
			return
		}
		log.Info("alert sent")
	}
}
