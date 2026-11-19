package main

import (
	"context"
	"strings"
	"time"
)

// connectWaitDefault is how long the first connection may keep failing with a
// transient class before the migrator gives up. A new pod's network policy is
// programmed a moment after the container starts (observed live on 2026-11-19:
// a throwaway pod got "refused" for its first ~2 seconds and was fine after),
// and the migrator dials in its first millisecond, so a single attempt fails
// every time. The wait is bounded and inside the overall -timeout.
const connectWaitDefault = 60 * time.Second

// retryable lists the classes of connectClass that can clear by themselves.
// Everything else (a refused login, a rejected certificate, a server without
// TLS, anything unclassified) is a real answer and ends the wait at once: waiting
// would only delay the same refusal, and a wrong password must not be hammered.
var retryable = map[string]bool{
	"connection refused":              true,
	"connection reset":                true,
	"host unreachable":                true,
	"timed out":                       true,
	"name resolution failed":          true,
	"connection closed by the server": true,
	"network error during dial":       true,
	"network error during read":       true,
	"network error during write":      true,
	"network error during connect":    true,
}

// isTransient reports whether a class may clear by itself.
func isTransient(class string) bool {
	if strings.HasPrefix(class, "server refused, SQLSTATE") {
		return false
	}
	return retryable[class]
}

// retryConnect calls attempt until it succeeds, fails with a class that cannot
// clear by itself, runs out of wait, or ctx ends. It returns the attempts made
// and the last error. The delay doubles from 250ms to 2s. sleep is injectable
// for tests and must return early when ctx ends.
func retryConnect(ctx context.Context, attempt func(context.Context) error, wait time.Duration, sleep func(context.Context, time.Duration)) (int, error) {
	deadline := time.Now().Add(wait)
	delay := 250 * time.Millisecond
	n := 0
	for {
		n++
		err := attempt(ctx)
		if err == nil {
			return n, nil
		}
		if ctx.Err() != nil || !isTransient(connectClass(err)) {
			return n, err
		}
		if time.Now().Add(delay).After(deadline) {
			return n, err
		}
		sleep(ctx, delay)
		if ctx.Err() != nil {
			return n, err
		}
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
