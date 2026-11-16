package tokenstore

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mctlhq/mctl-glovo/internal/glovoapi"
)

// A cap reserves at claim: concurrent callers share one window, so exactly
// limit of them reach Glovo, however they interleave. A check-then-act gate
// would let more than limit through.
func TestRequestCapReservesAtClaimConcurrently(t *testing.T) {
	ctx := context.Background()
	caps := DefaultCaps()
	caps.PIDRequestsPerMinute = 10
	m := newMemDB()
	h := newHarness(t, m, m, nil, caps)
	h.link(pidA, testSession("A1"))
	l, err := h.st.Get(ctx, h.pid(pidA))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := l.Client()

	const callers = 60
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		refused int
		other   []error
	)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := c.GetBaskets(ctx)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
			case errors.Is(err, ErrRateLimited):
				refused++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(other) != 0 {
		t.Fatalf("unexpected errors: %v", other)
	}
	if got := h.glovo.reads.Load(); got != 10 {
		t.Fatalf("upstream reads %d, want exactly the cap (10)", got)
	}
	if refused != callers-10 {
		t.Fatalf("refused %d, want %d", refused, callers-10)
	}
}

// The global cap is shared by every pid, and a pid refused by its own cap
// does not burn global quota.
func TestGlobalRequestCapBoundaryAcrossPids(t *testing.T) {
	ctx := context.Background()
	caps := DefaultCaps()
	caps.GlobalRequestsPerMinute = 6
	caps.PIDRequestsPerMinute = 4
	m := newMemDB()
	h := newHarness(t, m, m, nil, caps)
	h.link(pidA, testSession("A1"))
	h.link(pidB, testSession("B1"))
	la, err := h.st.Get(ctx, h.pid(pidA))
	if err != nil {
		t.Fatal(err)
	}
	lb, err := h.st.Get(ctx, h.pid(pidB))
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := la.Client()
	cb, _ := lb.Client()
	for i := 0; i < 4; i++ {
		if _, err := ca.GetBaskets(ctx); err != nil {
			t.Fatalf("A %d: %v", i, err)
		}
	}
	// A's own cap refuses; that must not consume global quota.
	for i := 0; i < 3; i++ {
		if _, err := ca.GetBaskets(ctx); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("A over its cap: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := cb.GetBaskets(ctx); err != nil {
			t.Fatalf("B %d (global has room): %v", i, err)
		}
	}
	if _, err := cb.GetBaskets(ctx); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("B past the global cap: %v", err)
	}
	if got := h.glovo.reads.Load(); got != 6 {
		t.Fatalf("upstream reads %d, want 6", got)
	}
}

// The window math at the boundary, including the daily window the harness
// cannot reach with a real clock.
func TestWindowBoundary(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	w := newWindow(2, 24*time.Hour)
	for i := 0; i < 2; i++ {
		if allowAll(now, capHit{w, "k"}) != -1 {
			t.Fatalf("hit %d refused", i)
		}
	}
	if allowAll(now.Add(23*time.Hour), capHit{w, "k"}) != 0 {
		t.Fatal("third hit inside the day allowed")
	}
	if allowAll(now.Add(24*time.Hour+time.Second), capHit{w, "k"}) != -1 {
		t.Fatal("hit after the window refused")
	}
	if allowAll(now, capHit{w, "other"}) != -1 {
		t.Fatal("another key shares the count")
	}
}

// A window that holds no limit is a refusal, never an allow: an unset or
// zero cap must not read as "uncapped", even for a key never seen.
func TestZeroLimitWindowRefuses(t *testing.T) {
	w := &window{limit: 0, period: time.Minute, hits: map[string][]time.Time{}}
	if allowAll(time.Now(), capHit{w, "fresh"}) != 0 {
		t.Fatal("zero-limit window allowed a request")
	}
}

// A zero Caps field is refused at construction (a partial config never runs).
func TestNewRefusesAnyZeroCap(t *testing.T) {
	ft := newFakeTransit(t)
	m := newMemDB()
	k, _ := NewKillSwitch(m)
	ok := func(context.Context, glovoapi.Session) error { return nil }
	for i := 0; i < 8; i++ {
		c := DefaultCaps()
		ptrs := []*int{&c.PIDRequestsPerMinute, &c.PIDRequestsPerDay, &c.GlobalRequestsPerMinute, &c.PIDDecryptsPerHour,
			&c.GlobalDecryptsPerHour, &c.LinkAttemptsPerIPHour, &c.LinkAttemptsGlobalHour, &c.RefreshFailuresInARow}
		*ptrs[i] = 0
		if _, err := New(Config{DB: m, Transit: ft.client(t), Kill: k, ValidateLink: ok, Caps: c}); err == nil {
			t.Fatalf("cap %d zero accepted", i)
		}
	}
}
