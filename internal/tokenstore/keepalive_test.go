package tokenstore

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mctlhq/mctl-glovo/internal/glovoapi"
)

// kaClock is the fake clock and sleeper of a Keepalive: sleeping advances it.
type kaClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *kaClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *kaClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func (c *kaClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.advance(d)
	return nil
}

// listCounter counts the work-list reads that reach the database.
type listCounter struct {
	DB
	lists atomic.Int64
}

func (l *listCounter) ListLinks(ctx context.Context, after string, limit int) ([]LinkRow, error) {
	l.lists.Add(1)
	return l.DB.ListLinks(ctx, after, limit)
}

// expiredSession is a linked session whose access token is past its life, so
// the first authenticated read has to refresh it.
func expiredSession(tag string) glovoapi.Session {
	s := testSession(tag)
	s["createdAt"] = json.Number(strconv.FormatInt(time.Now().Add(-2*time.Hour).UnixMilli(), 10))
	s["expiresIn"] = json.Number("1200")
	return s
}

type kaEnv struct {
	h        *harness
	m        *memDB
	lc       *listCounter
	k        *Keepalive
	clk      *kaClock
	sessions *Sessions
}

func newKAEnv(t *testing.T, caps Caps, interval time.Duration, read func(context.Context, *glovoapi.Client) error) *kaEnv {
	t.Helper()
	m := newMemDB()
	lc := &listCounter{DB: m}
	h := newHarness(t, lc, m, nil, caps)
	sessions, err := NewSessions(h.st, 0)
	if err != nil {
		t.Fatal(err)
	}
	k, err := NewKeepalive(KeepaliveConfig{
		Store: h.st, Sessions: sessions, Interval: interval, Read: read,
		Logger: slog.New(slog.NewJSONHandler(h.logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	clk := &kaClock{t: time.Now()}
	k.now, k.sleep = clk.now, clk.sleep
	k.rand = func() float64 { return 0 } // every new pid is due at once
	return &kaEnv{h: h, m: m, lc: lc, k: k, clk: clk, sessions: sessions}
}

func (e *kaEnv) auditCount(event, outcome, reason string) int {
	n := 0
	evs := e.m.events(event)
	for i := range evs {
		a := &evs[i]
		if (outcome == "" || a.Outcome == outcome) && (reason == "" || a.Reason == reason) {
			n++
		}
	}
	return n
}

func (e *kaEnv) state(pid string) string { return e.h.row(pid).State }

func TestKeepaliveRejectsIntervalOutsideBounds(t *testing.T) {
	m := newMemDB()
	h := newHarness(t, m, m, nil, Caps{})
	s, _ := NewSessions(h.st, 0)
	for _, d := range []time.Duration{0, -time.Hour, time.Minute, 59 * time.Minute, 24*time.Hour + time.Second, 48 * time.Hour} {
		if _, err := NewKeepalive(KeepaliveConfig{Store: h.st, Sessions: s, Interval: d}); err == nil {
			t.Errorf("interval %s accepted", d)
		}
	}
	for _, d := range []time.Duration{time.Hour, 24 * time.Hour} {
		if _, err := NewKeepalive(KeepaliveConfig{Store: h.st, Sessions: s, Interval: d}); err != nil {
			t.Errorf("interval %s refused: %v", d, err)
		}
	}
	if _, err := NewKeepalive(KeepaliveConfig{Sessions: s, Interval: time.Hour}); err == nil {
		t.Error("nil store accepted")
	}
	if _, err := NewKeepalive(KeepaliveConfig{Store: h.st, Interval: time.Hour}); err == nil {
		t.Error("nil sessions accepted")
	}
}

// While killed, or while the kill row cannot be read, or after a trip
// latched it, the keepalive reads no work list, decrypts nothing and sends
// nothing to Glovo.
func TestKeepaliveKilledMakesNoCallAndNoDecrypt(t *testing.T) {
	cases := map[string]func(e *kaEnv){
		"killed": func(e *kaEnv) {
			if err := e.m.SetKilled(context.Background(), true, "test"); err != nil {
				t.Fatal(err)
			}
		},
		"unreadable": func(e *kaEnv) { e.m.mu.Lock(); e.m.noCtl = true; e.m.mu.Unlock() },
		"latched":    func(e *kaEnv) { _ = e.h.kill.Trip(context.Background(), "test") },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			e := newKAEnv(t, Caps{}, time.Hour, nil)
			e.h.link(pidA, expiredSession("A1"))
			e.h.link(pidB, expiredSession("B1"))
			vault0 := e.h.transit.vaultCalls()
			lists0 := e.lc.lists.Load()
			setup(e)
			e.h.kill.invalidate() // the 5 s "running" cache is not what is under test
			e.clk.advance(2 * time.Hour)
			e.k.round(context.Background())
			e.k.round(context.Background())
			if n := e.lc.lists.Load() - lists0; n != 0 {
				t.Errorf("the work list was read %d times while killed", n)
			}
			if r, f := e.h.glovo.reads.Load(), e.h.glovo.refreshes.Load(); r != 0 || f != 0 {
				t.Errorf("glovo saw %d reads and %d refreshes while killed", r, f)
			}
			if d := e.h.transit.vaultCalls() - vault0; d != 0 {
				t.Errorf("vault saw %d calls while killed", d)
			}
			if n := e.auditCount("decrypt", "", ""); n != 0 {
				t.Errorf("%d decrypt audit rows while killed", n)
			}
		})
	}
}

// A kill that lands while a round is running stops the pids still to do.
func TestKeepaliveKillMidRoundStopsTheRest(t *testing.T) {
	var e *kaEnv
	reads := 0
	e = newKAEnv(t, Caps{}, time.Hour, func(ctx context.Context, c *glovoapi.Client) error {
		reads++
		_, err := c.GetBaskets(ctx)
		_ = e.m.SetKilled(context.Background(), true, "test")
		e.h.kill.invalidate()
		return err
	})
	e.h.link(pidA, testSession("A1"))
	e.h.link(pidB, testSession("B1"))
	e.k.round(context.Background())
	if reads != 1 {
		t.Fatalf("%d pids were read; the kill should have stopped the round after the first", reads)
	}
}

// Un-killed: one read per active pid, then nothing until the interval is up,
// and a suspended pid is not read.
func TestKeepaliveOneReadPerPidPerInterval(t *testing.T) {
	e := newKAEnv(t, Caps{}, 4*time.Hour, nil)
	e.h.link(pidA, testSession("A1"))
	e.h.link(pidB, testSession("B1"))
	pidC := "prn_cccccccccccccccccccccccccc"
	e.h.link(pidC, testSession("C1"))
	if err := e.m.SetState(context.Background(), pidC, e.h.row(pidC).LinkID, StateSuspended); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	e.k.round(ctx)
	if got := e.h.glovo.reads.Load(); got != 2 {
		t.Fatalf("first round: %d reads, want 2 (one per active pid)", got)
	}
	e.k.round(ctx)
	e.clk.advance(3 * time.Hour)
	e.k.round(ctx)
	if got := e.h.glovo.reads.Load(); got != 2 {
		t.Fatalf("inside the interval: %d reads, want still 2", got)
	}
	e.clk.advance(90 * time.Minute) // past 4h * 1.1
	e.k.round(ctx)
	if got := e.h.glovo.reads.Load(); got != 4 {
		t.Fatalf("after the interval: %d reads, want 4", got)
	}
	if !strings.Contains(e.h.logs.String(), `"outcome":"ok"`) {
		t.Error("no keepalive ok line")
	}
}

// The refresh the read triggers goes through the store's coordinator: the
// row lease and fence, the audit rows and the persisted rotation.
func TestKeepaliveRefreshRotatesThroughTheCoordinator(t *testing.T) {
	e := newKAEnv(t, Caps{}, time.Hour, nil)
	e.h.link(pidA, expiredSession("A1"))
	v0 := e.h.row(pidA).Version
	e.k.round(context.Background())
	if got := e.h.glovo.refreshes.Load(); got != 1 {
		t.Fatalf("%d refreshes at Glovo, want 1", got)
	}
	if got := e.auditCount("refresh", outcomeOK, "rotated"); got != 1 {
		t.Fatalf("%d refresh/ok audit rows, want 1", got)
	}
	row := e.h.row(pidA)
	if row.Version == v0 {
		t.Fatal("the rotation was not persisted behind the fence")
	}
	if rt, _ := e.h.stored(pidA)["refreshToken"].(string); rt == "refresh-A1" {
		t.Fatal("the stored refresh token was not rotated")
	}
	if row.LeaseHolder != "" && row.LeaseUntil.After(time.Now()) {
		t.Fatal("the row lease was left held")
	}
	if e.auditCount("decrypt", outcomeOK, "keepalive") == 0 {
		t.Fatal("no decrypt audit row with reason keepalive")
	}
}

// A keepalive on one cache and a tool call on another (a second replica)
// refresh the same pid at the same moment: Glovo sees one refresh, both
// reads succeed, nothing is marked dead.
func TestKeepaliveAndToolCallRefreshOnce(t *testing.T) {
	e := newKAEnv(t, Caps{}, time.Hour, nil)
	e.h.link(pidA, expiredSession("A1"))
	e.h.glovo.delay = 150 * time.Millisecond
	other, _ := NewSessions(e.h.st, 0)
	var wg sync.WaitGroup
	var callErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		c, err := other.Client(context.Background(), e.h.pid(pidA))
		if err == nil {
			_, err = c.GetBaskets(context.Background())
		}
		callErr = err
	}()
	e.k.round(context.Background())
	wg.Wait()
	if callErr != nil {
		t.Fatalf("the tool call failed: %v", callErr)
	}
	if got := e.h.glovo.refreshes.Load(); got != 1 {
		t.Fatalf("%d refreshes at Glovo, want exactly 1", got)
	}
	if s := e.state(pidA); s != StateActive {
		t.Fatalf("state %s", s)
	}
	if strings.Contains(e.h.logs.String(), `"outcome":"needs_relink"`) {
		t.Fatal("a needs_relink line for a healthy session")
	}
}

// A transient failure is retried with backoff and never marks the session
// dead; once Glovo answers again the read succeeds.
func TestKeepaliveTransientErrorRetriesAndDoesNotRelink(t *testing.T) {
	e := newKAEnv(t, Caps{}, 4*time.Hour, nil)
	e.h.link(pidA, expiredSession("A1"))
	e.h.glovo.status.Store(503)
	ctx := context.Background()
	e.k.round(ctx)
	if s := e.state(pidA); s != StateActive {
		t.Fatalf("state %s after a 503", s)
	}
	logs := e.h.logs.String()
	if !strings.Contains(logs, `"outcome":"transient_error"`) || strings.Contains(logs, `"outcome":"needs_relink"`) {
		t.Fatalf("log: %s", logs)
	}
	st := e.k.sched[pidA]
	if st == nil || st.failures != 1 {
		t.Fatalf("schedule %+v", st)
	}
	wait := st.next.Sub(e.clk.now())
	if wait < 3*time.Minute || wait > 7*time.Minute {
		t.Fatalf("first retry in %s, want about 5m", wait)
	}
	e.k.round(ctx) // too early
	if got := e.h.glovo.refreshes.Load(); got != 1 {
		t.Fatalf("retried too early: %d refreshes", got)
	}
	e.h.glovo.status.Store(0)
	e.clk.advance(8 * time.Minute)
	e.k.round(ctx)
	if s := e.k.sched[pidA]; s == nil || s.failures != 0 {
		t.Fatalf("not recovered: %+v", s)
	}
	if e.state(pidA) != StateActive {
		t.Fatal("not active after recovery")
	}
}

// Backoff grows and is capped at half the interval.
func TestKeepaliveBackoffIsCapped(t *testing.T) {
	e := newKAEnv(t, Caps{}, time.Hour, nil)
	st := &kaState{}
	l := ActiveLink{LinkID: "gl_x"}
	var last time.Duration
	for i := 0; i < 12; i++ {
		e.k.fail(context.Background(), l, st, "transient_error", "other")
		last = st.next.Sub(e.clk.now())
	}
	// half of 1h is 30m, jittered by at most +-20 percent
	if last > 36*time.Minute || last < 24*time.Minute {
		t.Fatalf("capped backoff %s", last)
	}
}

// Glovo refusing the refresh chain: needs_relink in the store, a distinct
// greppable keepalive line, the existing refresh audit row, and no further
// calls for that pid.
func TestKeepaliveRefusedChainIsNeedsRelink(t *testing.T) {
	e := newKAEnv(t, Caps{}, time.Hour, nil)
	e.h.link(pidA, expiredSession("A1"))
	e.h.glovo.status.Store(401)
	ctx := context.Background()
	e.k.round(ctx)
	if s := e.state(pidA); s != StateNeedsRelink {
		t.Fatalf("state %s, want needs_relink", s)
	}
	logs := e.h.logs.String()
	if !strings.Contains(logs, `"event":"keepalive"`) || !strings.Contains(logs, `"outcome":"needs_relink"`) || !strings.Contains(logs, "GlovoSessionNeedsRelink") || !strings.Contains(logs, `"reason":"glovo_rejected"`) {
		t.Fatalf("no distinct keepalive needs_relink line: %s", logs)
	}
	if e.auditCount("refresh", outcomeError, "glovo_rejected") != 1 {
		t.Fatal("no refresh/glovo_rejected audit row")
	}
	if e.k.sched[pidA] != nil {
		t.Fatal("a dead pid stays scheduled")
	}
	r := e.h.glovo.refreshes.Load()
	e.clk.advance(3 * time.Hour)
	e.k.round(ctx)
	if e.h.glovo.refreshes.Load() != r {
		t.Fatal("a pid in needs_relink was refreshed again")
	}
}

// Repeated refresh failures that are not a rejection of the chain reach
// needs_relink through the store's counter; the keepalive line says so, so
// an alert on a dead chain (reason=glovo_rejected) is not fed by it.
func TestKeepaliveRepeatedRefusalsAreDistinguishedFromARefusedChain(t *testing.T) {
	e := newKAEnv(t, Caps{}, 4*time.Hour, nil)
	e.h.link(pidA, expiredSession("A1"))
	e.h.glovo.noAccess.Store(true) // 200 without an access token: counted, not a rejection
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		e.k.round(ctx)
		e.clk.advance(time.Hour)
	}
	if s := e.state(pidA); s != StateNeedsRelink {
		t.Fatalf("state %s, want needs_relink after 3 counted failures", s)
	}
	logs := e.h.logs.String()
	if !strings.Contains(logs, `"outcome":"needs_relink","reason":"refresh_failures"`) {
		t.Fatalf("the line does not carry the cause: %s", logs)
	}
	if strings.Contains(logs, `"reason":"glovo_rejected"`) {
		t.Fatal("repeated refusals logged as a refused chain")
	}
}

// A failed work-list read is a failed read: nothing is dropped from the
// schedule and no pid is concluded gone.
func TestKeepaliveListFailureKeepsTheSchedule(t *testing.T) {
	e := newKAEnv(t, Caps{}, time.Hour, nil)
	e.h.link(pidA, testSession("A1"))
	ctx := context.Background()
	e.k.round(ctx)
	if e.k.sched[pidA] == nil {
		t.Fatal("not scheduled")
	}
	e.h.db.set("ListLinks", 1)
	e.k.round(ctx)
	if e.k.sched[pidA] == nil {
		t.Fatal("a failed listing emptied the schedule")
	}
	if !strings.Contains(e.h.logs.String(), `"outcome":"list_failed"`) {
		t.Fatal("no list_failed line")
	}
}

// ListActive errors on a failing page instead of returning a shorter list.
func TestListActiveFailsWholeOnAPageError(t *testing.T) {
	m := newMemDB()
	h := newHarness(t, m, m, nil, Caps{})
	h.link(pidA, testSession("A1"))
	h.db.set("ListLinks", 1)
	if got, err := h.st.ListActive(context.Background()); err == nil || got != nil {
		t.Fatalf("got %v, %v", got, err)
	}
}

// The keepalive cannot suspend a pid or trip the global latch, even with the
// real caps at their tightest and many pids due at once, and it leaves the
// tool calls' decrypt budget untouched.
func TestKeepaliveDoesNotConsumeDecryptCaps(t *testing.T) {
	caps := DefaultCaps()
	caps.PIDDecryptsPerHour = 2
	caps.GlobalDecryptsPerHour = 3
	e := newKAEnv(t, caps, time.Hour, nil)
	const n = 10
	var pids []string
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("prn_k%025d", i)
		pids = append(pids, p)
		e.h.link(p, expiredSession(fmt.Sprintf("K%d", i)))
	}
	e.k.round(context.Background())
	if got := e.h.glovo.reads.Load(); got != n {
		t.Fatalf("%d reads, want %d", got, n)
	}
	if err := e.h.kill.Check(context.Background()); err != nil {
		t.Fatalf("the keepalive tripped the kill switch: %v", err)
	}
	for _, p := range pids {
		if s := e.state(p); s != StateActive {
			t.Fatalf("a pid is %s after a keepalive", s)
		}
	}
	for _, ev := range []string{"cap_exceeded", "suspend", "kill_on"} {
		if got := len(e.m.events(ev)); got != 0 {
			t.Fatalf("%d %s events from the keepalive alone", got, ev)
		}
	}
	if got := e.auditCount("decrypt", outcomeOK, "keepalive"); got < n {
		t.Fatalf("only %d keepalive decrypt rows for %d pids", got, n)
	}
	// The tool calls' budget (2 per pid, 3 overall) is intact.
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := e.h.st.Get(ctx, e.h.pid(pids[0])); err != nil {
			t.Fatalf("tool-call decrypt %d refused after the keepalive: %v", i, err)
		}
	}
	if _, err := e.h.st.Get(ctx, e.h.pid(pids[1])); err != nil {
		t.Fatalf("third tool-call decrypt refused after the keepalive: %v", err)
	}
}

// Many pids at the minimum interval over a simulated day, with the store's
// own windows running on the simulated clock: never a suspension or a kill.
func TestKeepaliveAtMinimumIntervalNeverSelfTrips(t *testing.T) {
	caps := DefaultCaps()
	e := newKAEnv(t, caps, time.Hour, nil)
	e.h.st.now = e.clk.now
	const n = 20
	for i := 0; i < n; i++ {
		e.h.link(fmt.Sprintf("prn_m%025d", i), testSession(fmt.Sprintf("M%d", i)))
	}
	e.k.rand = func() float64 { return 0.5 }
	ctx := context.Background()
	for step := 0; step < 24*60; step++ { // every minute for a day
		if step%5 == 0 {
			e.sessions.DropAll() // each read decrypts afresh: the worst case
		}
		e.k.round(ctx)
		e.clk.advance(time.Minute)
	}
	if err := e.h.kill.Check(ctx); err != nil {
		t.Fatalf("kill switch tripped: %v", err)
	}
	for _, ev := range []string{"suspend", "kill_on"} {
		if got := len(e.m.events(ev)); got != 0 {
			t.Fatalf("%d %s events", got, ev)
		}
	}
	if e.h.glovo.reads.Load() < 24*n/2 {
		t.Fatalf("only %d reads in a day for %d pids", e.h.glovo.reads.Load(), n)
	}
}

// The keepalive's own budget only defers it: no kill, no suspension.
func TestKeepaliveBudgetDefersAndNeverEscalates(t *testing.T) {
	e := newKAEnv(t, DefaultCaps(), time.Hour, nil)
	e.h.st.kaDecrypt = newWindow(3, time.Hour)
	for i := 0; i < 5; i++ {
		e.h.link(fmt.Sprintf("prn_b%025d", i), testSession(fmt.Sprintf("B%d", i)))
	}
	e.k.round(context.Background())
	if e.auditCount("cap_exceeded", outcomeRefused, "keepalive_decrypt_cap") == 0 {
		t.Fatal("the budget never refused")
	}
	if err := e.h.kill.Check(context.Background()); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if len(e.m.events("suspend")) != 0 || len(e.m.events("kill_on")) != 0 {
		t.Fatal("budget breach escalated")
	}
	if !strings.Contains(e.h.logs.String(), `"reason":"rate_limited"`) {
		t.Fatal("deferred pid not logged as transient rate_limited")
	}
	if got := e.h.glovo.reads.Load(); got != 3 {
		t.Fatalf("%d reads with a budget of 3 decrypts", got)
	}
}

func TestKeepaliveJitterBounds(t *testing.T) {
	e := newKAEnv(t, Caps{}, 10*time.Hour, nil)
	for _, r := range []float64{0, 0.25, 0.5, 0.999999} {
		e.k.rand = func() float64 { return r }
		got := e.k.jittered(10*time.Hour, keepaliveJitter)
		if got < 9*time.Hour || got > 11*time.Hour {
			t.Errorf("rand %v: %s outside [9h, 11h]", r, got)
		}
	}
	e.k.rand = func() float64 { return 0 }
	if got := e.k.jittered(10*time.Hour, keepaliveJitter); got != 9*time.Hour {
		t.Errorf("lower edge %s", got)
	}
	// Two pids with different draws do not stay in step.
	e.h.link(pidA, testSession("A1"))
	e.h.link(pidB, testSession("B1"))
	draws := []float64{0.1, 0.9}
	i := 0
	e.k.rand = func() float64 { v := draws[i%2]; i++; return v }
	e.k.round(context.Background())
	a, b := e.k.sched[pidA], e.k.sched[pidB]
	if a == nil || b == nil || a.next.Equal(b.next) {
		t.Fatalf("pids synchronised: %+v %+v", a, b)
	}
	for _, st := range []*kaState{a, b} {
		if d := st.next.Sub(e.clk.now()); d < 0 || d >= 10*time.Hour {
			t.Errorf("initial slot %s outside [0, interval)", d)
		}
	}
}

func TestKeepaliveSteadyStateScheduleIsJittered(t *testing.T) {
	e := newKAEnv(t, Caps{}, 10*time.Hour, nil)
	e.h.link(pidA, testSession("A1"))
	e.k.round(context.Background())
	d := e.k.sched[pidA].next.Sub(e.clk.now())
	if d < 9*time.Hour || d > 11*time.Hour {
		t.Fatalf("next read in %s", d)
	}
}

// Run waits before its first round, and stops on context, closing Done.
func TestKeepaliveRunWaitsAndStopsOnContext(t *testing.T) {
	e := newKAEnv(t, Caps{}, time.Hour, nil)
	e.h.link(pidA, testSession("A1"))
	ctx, cancel := context.WithCancel(context.Background())
	sleeps := 0
	e.k.sleep = func(ctx context.Context, d time.Duration) error {
		sleeps++
		if sleeps == 1 {
			if e.lc.lists.Load() != 0 || e.h.glovo.reads.Load() != 0 {
				t.Error("acted before the first tick")
			}
			return nil
		}
		cancel()
		return ctx.Err()
	}
	done := make(chan struct{})
	go func() { e.k.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	select {
	case <-e.k.Done():
	default:
		t.Fatal("Done not closed")
	}
	if e.h.glovo.reads.Load() != 1 {
		t.Fatalf("%d reads: the first round after the tick did not run", e.h.glovo.reads.Load())
	}
}

func TestKeepaliveStopsMidRoundOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	e := newKAEnv(t, Caps{}, time.Hour, func(c context.Context, cl *glovoapi.Client) error {
		cancel()
		return context.Canceled
	})
	e.h.link(pidA, testSession("A1"))
	e.h.link(pidB, testSession("B1"))
	e.k.round(ctx)
	if e.k.sched[pidA] != nil && e.k.sched[pidA].failures != 0 {
		t.Fatal("shutdown counted as a failure")
	}
	if e.h.glovo.reads.Load() > 1 {
		t.Fatal("kept reading after shutdown")
	}
}

// Logs and audit carry the link id, never the principal id or a token.
func TestKeepaliveLogsNeverCarryPidOrTokens(t *testing.T) {
	e := newKAEnv(t, Caps{}, time.Hour, nil)
	e.h.link(pidA, expiredSession("A1"))
	e.k.round(context.Background())
	e.h.glovo.status.Store(401)
	e.sessions.DropAll()
	e.clk.advance(3 * time.Hour)
	e.h.link(pidB, expiredSession("B1"))
	e.k.round(context.Background())
	logs := e.h.logs.String()
	for _, bad := range []string{pidA, pidB, "access-", "refresh-", "urn:device"} {
		if strings.Contains(logs, bad) {
			t.Errorf("log carries %q", bad)
		}
	}
}
