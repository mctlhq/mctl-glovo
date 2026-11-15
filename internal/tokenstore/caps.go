package tokenstore

import (
	"sync"
	"time"
)

// Caps are the limits of design section 6. Counters are in-process, which
// is correct for the single replica of C2-1/C2-2; more replicas need
// shared counters first (a C2-2 item).
type Caps struct {
	PIDRequestsPerMinute    int // Glovo requests per pid
	PIDRequestsPerDay       int
	GlobalRequestsPerMinute int // Glovo requests, all pids
	PIDDecryptsPerHour      int // breach suspends the pid
	GlobalDecryptsPerHour   int // breach trips the runtime kill switch
	LinkAttemptsPerIPHour   int
	LinkAttemptsGlobalHour  int
	RefreshFailuresInARow   int // reaching it sets needs_relink
}

// DefaultCaps are the proposals of section 6 for owner-only operation.
func DefaultCaps() Caps {
	return Caps{
		PIDRequestsPerMinute:    60,
		PIDRequestsPerDay:       2000,
		GlobalRequestsPerMinute: 120,
		PIDDecryptsPerHour:      30,
		GlobalDecryptsPerHour:   100,
		LinkAttemptsPerIPHour:   10,
		LinkAttemptsGlobalHour:  50,
		RefreshFailuresInARow:   3,
	}
}

// valid refuses a zero or negative cap: "no limit" is not a configuration.
func (c Caps) valid() bool {
	for _, v := range []int{c.PIDRequestsPerMinute, c.PIDRequestsPerDay, c.GlobalRequestsPerMinute,
		c.PIDDecryptsPerHour, c.GlobalDecryptsPerHour, c.LinkAttemptsPerIPHour,
		c.LinkAttemptsGlobalHour, c.RefreshFailuresInARow} {
		if v <= 0 {
			return false
		}
	}
	return true
}

// window is a sliding-log counter: it keeps at most limit timestamps per
// key, so memory is bounded by the cap itself.
type window struct {
	mu     sync.Mutex
	limit  int
	period time.Duration
	hits   map[string][]time.Time
}

func newWindow(limit int, period time.Duration) *window {
	return &window{limit: limit, period: period, hits: map[string][]time.Time{}}
}

// allow records a hit for key at now and reports whether it is within the
// cap. A refused hit is not recorded.
// pruneLocked drops hits older than the window and reports whether one
// more fits. w.mu must be held.
func (w *window) pruneLocked(key string, now time.Time) bool {
	cut := now.Add(-w.period)
	h := w.hits[key]
	i := 0
	for i < len(h) && !h[i].After(cut) {
		i++
	}
	w.hits[key] = h[i:]
	return len(w.hits[key]) < w.limit
}

// capHit names one window and the key counted in it.
type capHit struct {
	w   *window
	key string
}

// allowAll checks every window first and records the hit in all of them
// only when all pass, so a request refused by one cap is never counted
// against another (a capped pid must not burn the global quota). It
// returns -1 on success, or the index of the first window that refused.
// Callers always pass windows in the same order, so the locks cannot
// deadlock.
func allowAll(now time.Time, hits ...capHit) int {
	for _, h := range hits {
		h.w.mu.Lock()
	}
	defer func() {
		for _, h := range hits {
			h.w.mu.Unlock()
		}
	}()
	for i, h := range hits {
		if !h.w.pruneLocked(h.key, now) {
			return i
		}
	}
	for _, h := range hits {
		h.w.hits[h.key] = append(h.w.hits[h.key], now)
	}
	return -1
}

// limiter groups the windows.
type limiter struct {
	pidReqMin, pidReqDay, globalReqMin *window
	pidDecrypt, globalDecrypt          *window
	linkIP, linkGlobal                 *window
}

func newLimiter(c Caps) *limiter {
	return &limiter{
		pidReqMin:     newWindow(c.PIDRequestsPerMinute, time.Minute),
		pidReqDay:     newWindow(c.PIDRequestsPerDay, 24*time.Hour),
		globalReqMin:  newWindow(c.GlobalRequestsPerMinute, time.Minute),
		pidDecrypt:    newWindow(c.PIDDecryptsPerHour, time.Hour),
		globalDecrypt: newWindow(c.GlobalDecryptsPerHour, time.Hour),
		linkIP:        newWindow(c.LinkAttemptsPerIPHour, time.Hour),
		linkGlobal:    newWindow(c.LinkAttemptsGlobalHour, time.Hour),
	}
}
