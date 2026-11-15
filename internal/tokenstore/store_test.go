package tokenstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mctlhq/mctl-glovo/internal/authn"
	"github.com/mctlhq/mctl-glovo/internal/glovoapi"
)

func tokens(c *glovoapi.Client) (string, string) {
	s := c.Session()
	a, _ := s["accessToken"].(string)
	r, _ := s["refreshToken"].(string)
	return a, r
}

// 1. A/B isolation, including unlink and relink of B (design 3.3, §9.5).
func TestPIDIsolationIncludingRelink(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		ctx := context.Background()
		h.link(pidA, testSession("A1"))
		h.link(pidB, testSession("B1"))
		sessions, err := NewSessions(h.st, 0)
		if err != nil {
			t.Fatal(err)
		}
		a, b := h.pid(pidA), h.pid(pidB)

		ca, err := sessions.Client(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		cb, err := sessions.Client(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		if at, _ := tokens(ca); at != "access-A1" {
			t.Fatalf("A got %q", at)
		}
		if bt, _ := tokens(cb); bt != "access-B1" {
			t.Fatalf("B got %q", bt)
		}
		if _, err := ca.GetBaskets(ctx); err != nil {
			t.Fatalf("A basket read: %v", err)
		}

		// B unlinks: B is refused at once, A is untouched.
		if err := h.st.Unlink(ctx, b, "user"); err != nil {
			t.Fatal(err)
		}
		if _, err := sessions.Client(ctx, b); !errors.Is(err, ErrNotLinked) {
			t.Fatalf("B after unlink: %v", err)
		}
		if ca2, err := sessions.Client(ctx, a); err != nil || ca2 != ca {
			t.Fatalf("A after B's unlink: %v, same client %v", err, ca2 == ca)
		}

		// B relinks with a new session: B sees the new one only, A still
		// sees its own.
		h.link(pidB, testSession("B2"))
		cb2, err := sessions.Client(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		if bt, _ := tokens(cb2); bt != "access-B2" {
			t.Fatalf("B after relink got %q", bt)
		}
		if at, _ := tokens(ca); at != "access-A1" {
			t.Fatalf("A after B's relink got %q", at)
		}

		// Relink without unlink, while B's client is cached: dropped.
		h.link(pidB, testSession("B3"))
		cb3, err := sessions.Client(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		if cb3 == cb2 {
			t.Fatal("cached client of the replaced link was reused")
		}
		if bt, _ := tokens(cb3); bt != "access-B3" {
			t.Fatalf("B after second relink got %q", bt)
		}
	})
}

// PIDs come only from verified principals.
func TestPIDFrom(t *testing.T) {
	if PIDFrom(nil).valid() {
		t.Fatal("nil principal gave a PID")
	}
	for _, bad := range []string{"", "prn_", "user_x", "prn_a|b", "prn_" + strings.Repeat("a", 121)} {
		if PIDFrom(&authn.Principal{ID: bad}).valid() {
			t.Fatalf("%q gave a PID", bad)
		}
	}
	if !PIDFrom(&authn.Principal{ID: pidA}).valid() {
		t.Fatal("valid pid refused")
	}
	h := memHarness(t, Caps{})
	if _, err := h.st.Get(context.Background(), PID{}); !errors.Is(err, ErrInvalidPID) {
		t.Fatalf("zero PID: %v", err)
	}
}

// 2. Swapping two rows' ciphertexts makes both decrypts fail (T2).
func TestCiphertextSwapFailsToDecrypt(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		ctx := context.Background()
		h.link(pidA, testSession("A1"))
		h.link(pidB, testSession("B1"))
		h.swap(pidA, pidB)
		for _, p := range []string{pidA, pidB} {
			if _, err := h.st.Get(ctx, h.pid(p)); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("%s after swap: %v", p, err)
			}
		}
		if h.mem != nil && len(h.mem.events("integrity_error")) != 2 {
			t.Fatalf("integrity_error events: %d", len(h.mem.events("integrity_error")))
		}
	})
}

// An old ciphertext of the same pid (an earlier link restored from a
// backup) does not decrypt under the current link either.
func TestOldLinkCiphertextFailsToDecrypt(t *testing.T) {
	h := memHarness(t, Caps{})
	ctx := context.Background()
	h.link(pidA, testSession("A1"))
	old := h.row(pidA).Ciphertext
	h.link(pidA, testSession("A2"))
	h.mem.mu.Lock()
	h.mem.links[pidA].Ciphertext = old
	h.mem.mu.Unlock()
	if _, err := h.st.Get(ctx, h.pid(pidA)); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("old link ciphertext: %v", err)
	}
}

// 3. Two replicas refreshing one pid at once: exactly one Glovo refresh,
// and both end on the same rotated session.
func TestConcurrentRefreshOneUpstream(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		ctx := context.Background()
		h.link(pidA, testSession("A1"))
		h.glovo.delay = 300 * time.Millisecond
		a := h.pid(pidA)

		// Two leases with their own clients: two replicas.
		var clients []*glovoapi.Client
		for i := 0; i < 2; i++ {
			l, err := h.st.Get(ctx, a)
			if err != nil {
				t.Fatal(err)
			}
			c, err := l.Client()
			if err != nil {
				t.Fatal(err)
			}
			clients = append(clients, c)
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for i, c := range clients {
			wg.Add(1)
			go func(i int, c *glovoapi.Client) {
				defer wg.Done()
				<-start
				errs[i] = c.Refresh(ctx)
			}(i, c)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("replica %d: %v", i, err)
			}
		}
		if n := h.glovo.refreshes.Load(); n != 1 {
			t.Fatalf("upstream refreshes: %d, want 1", n)
		}
		a0, r0 := tokens(clients[0])
		a1, r1 := tokens(clients[1])
		if a0 != a1 || r0 != r1 || a0 == "access-A1" {
			t.Fatalf("replicas diverged: %s/%s vs %s/%s", a0, r0, a1, r1)
		}
		if got := h.stored(pidA)["refreshToken"]; got != r0 {
			t.Fatalf("stored refresh token %v, clients have %s", got, r0)
		}
		if r := h.row(pidA); r.LeaseHolder != "" {
			t.Fatalf("lease left behind: %q", r.LeaseHolder)
		}
	})
}

// 4. Killed means zero Vault calls, on every path that could decrypt.
func TestKilledMeansZeroDecrypts(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		ctx := context.Background()
		h.link(pidA, testSession("A1"))
		sessions, _ := NewSessions(h.st, 0)
		c, err := sessions.Client(ctx, h.pid(pidA))
		if err != nil {
			t.Fatal(err)
		}
		before := h.transit.vaultCalls()
		readsBefore := h.glovo.reads.Load()

		if err := h.raw.SetKilled(ctx, true, "test"); err != nil {
			t.Fatal(err)
		}
		h.kill.mu.Lock()
		h.kill.okUntil = time.Time{} // skip the 5 s cache in the test
		h.kill.mu.Unlock()

		a := h.pid(pidA)
		if _, err := h.st.Get(ctx, a); !errors.Is(err, ErrKilled) {
			t.Fatalf("Get: %v", err)
		}
		if err := h.st.Update(ctx, a, func(_ context.Context, s glovoapi.Session) (glovoapi.Session, error) { return s, nil }); !errors.Is(err, ErrKilled) {
			t.Fatalf("Update: %v", err)
		}
		if _, err := sessions.Client(ctx, a); !errors.Is(err, ErrKilled) {
			t.Fatalf("Sessions.Client: %v", err)
		}
		if sessions.Len() != 0 {
			t.Fatal("clients kept after kill")
		}
		if _, err := h.st.Rewrap(ctx); !errors.Is(err, ErrKilled) {
			t.Fatalf("Rewrap: %v", err)
		}
		if _, _, err := h.st.CreateLinkCode(ctx, a, "consent-v1"); !errors.Is(err, ErrKilled) {
			t.Fatalf("CreateLinkCode: %v", err)
		}
		if err := h.st.Link(ctx, strings.Repeat("A", 43), testSession("X")); !errors.Is(err, ErrKilled) {
			t.Fatalf("Link: %v", err)
		}
		// A client handed out before the kill is refused per request.
		if _, err := c.GetBaskets(ctx); !errors.Is(err, ErrKilled) {
			t.Fatalf("cached client request: %v", err)
		}
		if err := c.Refresh(ctx); !errors.Is(err, ErrKilled) {
			t.Fatalf("cached client refresh: %v", err)
		}
		if n := h.transit.vaultCalls() - before; n != 0 {
			t.Fatalf("vault calls while killed: %d", n)
		}
		if n := h.glovo.reads.Load() - readsBefore; n != 0 {
			t.Fatalf("glovo reads while killed: %d", n)
		}
		// Unlink still works, without Vault.
		if err := h.st.Unlink(ctx, a, "user"); err != nil {
			t.Fatalf("Unlink while killed: %v", err)
		}
		if h.transit.vaultCalls()-before != 0 {
			t.Fatal("unlink called vault")
		}
	})
}

// 5. A control row that cannot be read means killed, and the failure is
// not cached.
func TestUnreadableControlRowMeansKilled(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		ctx := context.Background()
		h.link(pidA, testSession("A1"))
		before := h.transit.decrypts.Load()
		h.kill.mu.Lock()
		h.kill.okUntil = time.Time{}
		h.kill.mu.Unlock()

		h.db.set("ReadControl", -1)
		a := h.pid(pidA)
		if _, err := h.st.Get(ctx, a); !errors.Is(err, ErrKilled) {
			t.Fatalf("Get with unreadable control: %v", err)
		}
		st, err := h.st.Status(ctx, a)
		if err != nil || !st.Killed {
			t.Fatalf("Status: %+v, %v", st, err)
		}
		if h.transit.decrypts.Load() != before {
			t.Fatal("decrypt with unreadable control row")
		}
		// The error is not cached: the next call reads again and runs.
		h.db.set("ReadControl", 0)
		if _, err := h.st.Get(ctx, a); err != nil {
			t.Fatalf("Get after recovery: %v", err)
		}
	})
	// A missing control row (memDB) is killed too.
	m := newMemDB()
	h := newHarness(t, m, m, nil, Caps{})
	m.mu.Lock()
	m.noCtl = true
	m.mu.Unlock()
	h.kill.okUntil = time.Time{}
	if err := h.kill.Check(context.Background()); !errors.Is(err, ErrKilled) {
		t.Fatalf("missing control row: %v", err)
	}
}

func TestKillSwitchCachesOnlyRunning(t *testing.T) {
	m := newMemDB()
	k, _ := NewKillSwitch(m)
	now := time.Unix(1000, 0)
	k.now = func() time.Time { return now }
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_ = k.Check(ctx)
	}
	if k.readCalls != 1 {
		t.Fatalf("running answer not cached: %d reads", k.readCalls)
	}
	now = now.Add(MaxKillCache)
	_ = k.Check(ctx)
	if k.readCalls != 2 {
		t.Fatalf("cache outlived %v", MaxKillCache)
	}
	m.killed = true
	now = now.Add(MaxKillCache)
	for i := 0; i < 3; i++ {
		if err := k.Check(ctx); !errors.Is(err, ErrKilled) {
			t.Fatal("not killed")
		}
	}
	if k.readCalls != 5 {
		t.Fatalf("killed answer cached: %d reads", k.readCalls)
	}
}

func TestStoreNeedsKillSwitch(t *testing.T) {
	ft := newFakeTransit(t)
	m := newMemDB()
	ok := func(context.Context, glovoapi.Session) error { return nil }
	if _, err := New(Config{DB: m, Transit: ft.client(t), ValidateLink: ok}); err == nil {
		t.Fatal("store built without a kill switch")
	}
	var nilKill *KillSwitch
	if err := nilKill.Check(context.Background()); !errors.Is(err, ErrKilled) {
		t.Fatal("nil kill switch is not killed")
	}
	if _, err := NewKillSwitch(nil); err == nil {
		t.Fatal("kill switch without a DB")
	}
	k, _ := NewKillSwitch(m)
	if _, err := New(Config{DB: m, Transit: ft.client(t), Kill: k}); err == nil {
		t.Fatal("store built without a link validator")
	}
	if _, err := New(Config{DB: m, Transit: ft.client(t), Kill: k, ValidateLink: ok, Caps: Caps{PIDRequestsPerMinute: 1}}); err == nil {
		t.Fatal("partial caps accepted")
	}
}

// 6. Caps.
func TestCaps(t *testing.T) {
	ctx := context.Background()
	small := DefaultCaps()
	small.PIDDecryptsPerHour = 3
	small.GlobalDecryptsPerHour = 5
	small.PIDRequestsPerMinute = 4
	small.LinkAttemptsPerIPHour = 2

	t.Run("pid decrypt cap suspends", func(t *testing.T) {
		m := newMemDB()
		h := newHarness(t, m, m, nil, small)
		h.link(pidA, testSession("A1"))
		a := h.pid(pidA)
		for i := 0; i < 3; i++ {
			if _, err := h.st.Get(ctx, a); err != nil {
				t.Fatalf("decrypt %d: %v", i, err)
			}
		}
		if _, err := h.st.Get(ctx, a); !errors.Is(err, ErrSuspended) {
			t.Fatalf("4th decrypt: %v", err)
		}
		if st := h.row(pidA).State; st != StateSuspended {
			t.Fatalf("state %s", st)
		}
		if h.transit.decrypts.Load() != 3 {
			t.Fatalf("decrypts %d", h.transit.decrypts.Load())
		}
	})

	t.Run("global decrypt cap kills", func(t *testing.T) {
		m := newMemDB()
		h := newHarness(t, m, m, nil, small)
		h.link(pidA, testSession("A1"))
		h.link(pidB, testSession("B1"))
		for i := 0; i < 5; i++ {
			p := pidA
			if i%2 == 1 {
				p = pidB
			}
			if _, err := h.st.Get(ctx, h.pid(p)); err != nil {
				t.Fatalf("decrypt %d: %v", i, err)
			}
		}
		if _, err := h.st.Get(ctx, h.pid(pidB)); !errors.Is(err, ErrKilled) {
			t.Fatalf("6th decrypt: %v", err)
		}
		if killed, _ := m.ReadControl(ctx); !killed {
			t.Fatal("control row not killed")
		}
		if _, err := h.st.Get(ctx, h.pid(pidA)); !errors.Is(err, ErrKilled) {
			t.Fatalf("after trip: %v", err)
		}
	})

	t.Run("request cap", func(t *testing.T) {
		m := newMemDB()
		h := newHarness(t, m, m, nil, small)
		h.link(pidA, testSession("A1"))
		l, err := h.st.Get(ctx, h.pid(pidA))
		if err != nil {
			t.Fatal(err)
		}
		c, _ := l.Client()
		for i := 0; i < 4; i++ {
			if _, err := c.GetBaskets(ctx); err != nil {
				t.Fatalf("request %d: %v", i, err)
			}
		}
		if _, err := c.GetBaskets(ctx); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("5th request: %v", err)
		}
		if h.glovo.reads.Load() != 4 {
			t.Fatalf("upstream reads %d", h.glovo.reads.Load())
		}
	})

	t.Run("refused request not counted against other caps", func(t *testing.T) {
		caps := DefaultCaps()
		caps.PIDRequestsPerMinute = 2
		caps.GlobalRequestsPerMinute = 5
		m := newMemDB()
		h := newHarness(t, m, m, nil, caps)
		h.link(pidA, testSession("A1"))
		h.link(pidB, testSession("B1"))
		la, _ := h.st.Get(ctx, h.pid(pidA))
		lb, _ := h.st.Get(ctx, h.pid(pidB))
		ca, _ := la.Client()
		cb, _ := lb.Client()
		for i := 0; i < 10; i++ {
			_, _ = ca.GetBaskets(ctx) // 2 pass, 8 refused by A's own cap
		}
		// The global window holds only A's 2 real requests, so B still
		// gets its own 2.
		for i := 0; i < 2; i++ {
			if _, err := cb.GetBaskets(ctx); err != nil {
				t.Fatalf("pid B starved by pid A's refused requests: %v", err)
			}
		}
	})

	t.Run("link attempts", func(t *testing.T) {
		h := memHarness(t, small)
		for i := 0; i < 2; i++ {
			if err := h.st.LinkAttempt("192.0.2.1"); err != nil {
				t.Fatal(err)
			}
		}
		if err := h.st.LinkAttempt("192.0.2.1"); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("3rd attempt: %v", err)
		}
		if err := h.st.LinkAttempt("192.0.2.2"); err != nil {
			t.Fatalf("other ip: %v", err)
		}
	})

	t.Run("refresh failures", func(t *testing.T) {
		m := newMemDB()
		h := newHarness(t, m, m, nil, Caps{})
		h.link(pidA, testSession("A1"))
		a := h.pid(pidA)
		// A refresh answer glovoapi refuses (no access token) is counted;
		// the third in a row means relink.
		h.glovo.noAccess.Store(true)
		for i := 0; i < 2; i++ {
			l, _ := h.st.Get(ctx, a)
			c, _ := l.Client()
			if err := c.Refresh(ctx); err == nil {
				t.Fatal("refresh succeeded")
			}
			if st := h.row(pidA).State; st != StateActive {
				t.Fatalf("after %d failures: %s", i+1, st)
			}
		}
		l, _ := h.st.Get(ctx, a)
		c, _ := l.Client()
		if err := c.Refresh(ctx); !errors.Is(err, ErrNeedsRelink) {
			t.Fatalf("3rd failure: %v", err)
		}
		if st := h.row(pidA).State; st != StateNeedsRelink {
			t.Fatalf("state %s", st)
		}
		h.glovo.noAccess.Store(false)
		// 5xx and 429 are transient: no count.
		h.link(pidB, testSession("B1"))
		h.glovo.status.Store(429)
		lb0, _ := h.st.Get(ctx, h.pid(pidB))
		cb0, _ := lb0.Client()
		_ = cb0.Refresh(ctx)
		h.glovo.status.Store(503)
		lb, _ := h.st.Get(ctx, h.pid(pidB))
		cb, _ := lb.Client()
		_ = cb.Refresh(ctx)
		if r := h.row(pidB); r.RefreshFailures != 0 || r.State != StateActive {
			t.Fatalf("5xx counted: %+v", r)
		}
		// A rejection of the refresh token (400 invalid_grant, 401, 403)
		// is immediate.
		for _, st := range []int64{400, 401, 403} {
			h.link(pidB, testSession("B"+strconv.FormatInt(st, 10)))
			h.glovo.status.Store(st)
			lr, _ := h.st.Get(ctx, h.pid(pidB))
			cr, _ := lr.Client()
			if err := cr.Refresh(ctx); !errors.Is(err, ErrNeedsRelink) {
				t.Fatalf("%d: %v", st, err)
			}
		}
	})
}

// 7. No session byte in logs, errors or renderings, on every path.
func TestRedactionMarkerScan(t *testing.T) {
	m := newMemDB()
	h := newHarness(t, m, m, nil, Caps{})
	ctx := WithRequestID(context.Background(), "req-1")
	const mk = "ZQXJ"
	sess := glovoapi.Session{
		"accessToken":  "access-" + mk + "-ACCESS",
		"refreshToken": "refresh-" + mk + "-REFRESH",
		"deviceUrn":    "urn:" + mk + "-DEVICE",
		"customerId":   mk + "-CUSTOMER",
		"location":     map[string]any{"text": mk + "-ADDRESS"},
		"createdAt":    json.Number(fmt.Sprint(time.Now().UnixMilli())),
		"expiresIn":    json.Number("3600"),
	}
	var errs []error
	keep := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	h.link(pidA, sess)
	h.link(pidB, testSession("B1"))
	a := h.pid(pidA)

	l, err := h.st.Get(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	var rendered []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%T %v"} {
		rendered = append(rendered, fmt.Sprintf(verb, l), fmt.Sprintf(verb, []any{l}), fmt.Sprintf(verb, struct{ L *Lease }{l}))
	}
	j, _ := json.Marshal(map[string]any{"lease": l})
	rendered = append(rendered, string(j))
	h.st.log.Info("lease", "lease", l, "any", slog.AnyValue(l))

	c, _ := l.Client()
	_, err = c.GetBaskets(ctx)
	keep(err)
	// Glovo errors that echo the refresh token. A refresh error is always
	// one of this package's fixed-text errors, never glovoapi's own.
	fixed := []error{ErrNeedsRelink, ErrUnavailable, ErrSessionRefused, ErrKilled, ErrRateLimited, ErrSuperseded, ErrBusy}
	for _, st := range []int64{400, 401, 403, 429, 500, 503} {
		h.glovo.status.Store(st)
		err := c.Refresh(ctx)
		keep(err)
		ok := false
		for _, f := range fixed {
			ok = ok || err == f
		}
		if !ok {
			t.Errorf("refresh %d: error is not a fixed-text sentinel: %T", st, err)
		}
		_ = h.raw.SetState(ctx, pidA, h.row(pidA).LinkID, StateActive)
	}
	h.glovo.status.Store(0)
	// Vault errors that echo the request body.
	h.transit.fail500.Store(true)
	_, err = h.st.Get(ctx, a)
	keep(err)
	keep(h.st.Link(ctx, "x", sess))
	code, _, _ := h.st.CreateLinkCode(ctx, a, "consent-v1")
	keep(h.st.Link(ctx, code, sess))
	h.transit.fail500.Store(false)
	// Integrity failure (Vault 400 echo).
	h.link(pidA, sess)
	h.swap = func(x, y string) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.links[x].Ciphertext, m.links[y].Ciphertext = m.links[y].Ciphertext, m.links[x].Ciphertext
	}
	h.swap(pidA, pidB)
	_, err = h.st.Get(ctx, a)
	keep(err)
	h.swap(pidA, pidB)
	// Unconfirmed write.
	h.glovo.issue(sess)
	l2, err := h.st.Get(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := l2.Client()
	h.db.set("CommitLease", -1)
	h.db.set("FindWrite", -1)
	h.st.sleep = func(context.Context, time.Duration) error { return nil }
	keep(c2.Refresh(ctx))
	h.db.set("CommitLease", 0)
	h.db.set("FindWrite", 0)
	// Session refused by shape.
	bad := glovoapi.Session{"accessToken": mk + "-ACCESS", "refreshToken": 7}
	code, _, _ = h.st.CreateLinkCode(ctx, a, "consent-v1")
	keep(h.st.Link(ctx, code, bad))
	if len(errs) < 8 {
		t.Fatalf("only %d error paths exercised", len(errs))
	}

	hay := h.logs.String() + "\n" + strings.Join(rendered, "\n")
	for _, e := range errs {
		hay += "\n" + e.Error() + fmt.Sprintf(" %+v %#v", e, e)
	}
	if strings.Contains(hay, mk) {
		i := strings.Index(hay, mk)
		t.Fatalf("session marker leaked: ...%s...", hay[max(0, i-80):min(len(hay), i+40)])
	}
	if strings.Contains(hay, "vault:v") {
		t.Fatal("ciphertext leaked into logs or errors")
	}
	if !strings.Contains(hay, redacted) {
		t.Fatal("redaction marker missing: the renderings were not exercised")
	}
}

// 8. Version is monotonic across refresh, relink, unlink + relink.
func TestVersionMonotonicAcrossRelink(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		ctx := context.Background()
		a := h.pid(pidA)
		var seen []int64
		h.link(pidA, testSession("A1"))
		seen = append(seen, h.row(pidA).Version)
		l, _ := h.st.Get(ctx, a)
		c, _ := l.Client()
		if err := c.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
		seen = append(seen, h.row(pidA).Version)
		if l.Version() != seen[1] {
			t.Fatalf("lease version %d, row %d", l.Version(), seen[1])
		}
		h.link(pidA, testSession("A2"))
		seen = append(seen, h.row(pidA).Version)
		if err := h.st.Unlink(ctx, a, "user"); err != nil {
			t.Fatal(err)
		}
		h.link(pidA, testSession("A3"))
		seen = append(seen, h.row(pidA).Version)
		for i := 1; i < len(seen); i++ {
			if seen[i] <= seen[i-1] {
				t.Fatalf("version not monotonic: %v", seen)
			}
		}
	})
}

// Even if a DB reset the version on relink, a cached client of the old
// link is dropped, because link_id is checked too (agy's P1 on PR #14).
func TestCacheChecksLinkIDNotOnlyVersion(t *testing.T) {
	m := newMemDB()
	h := newHarness(t, m, m, nil, Caps{})
	ctx := context.Background()
	a := h.pid(pidA)
	h.link(pidA, testSession("A1"))
	sessions, _ := NewSessions(h.st, 0)
	c1, err := sessions.Client(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	v := h.row(pidA).Version
	h.link(pidA, testSession("A2"))
	m.mu.Lock()
	m.links[pidA].Version = v // a buggy reset
	m.mu.Unlock()
	c2, err := sessions.Client(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if c2 == c1 {
		t.Fatal("client of the old link reused after relink")
	}
	if at, _ := tokens(c2); at != "access-A2" {
		t.Fatalf("got %s", at)
	}
}

// A refresh racing a relink loses the fence and does not overwrite the
// new link.
func TestRefreshLosesFenceToRelink(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		ctx := context.Background()
		a := h.pid(pidA)
		h.link(pidA, testSession("A1"))
		l, _ := h.st.Get(ctx, a)
		relinked := false
		err := h.st.Update(ctx, a, func(ctx context.Context, s glovoapi.Session) (glovoapi.Session, error) {
			if !relinked {
				relinked = true
				h.link(pidA, testSession("A2"))
			}
			s["accessToken"] = "access-stale"
			return s, nil
		})
		if !errors.Is(err, ErrSuperseded) {
			t.Fatalf("update across relink: %v", err)
		}
		if got := h.stored(pidA)["accessToken"]; got != "access-A2" {
			t.Fatalf("relink overwritten: %v", got)
		}
		if h.mem != nil && len(h.mem.events("persist_discarded")) != 1 {
			t.Fatal("discarded tokens not audited")
		}
		// The old lease's client is refused as not linked.
		c, _ := l.Client()
		if err := c.Refresh(ctx); !errors.Is(err, ErrNotLinked) {
			t.Fatalf("old lease refresh: %v", err)
		}
	})
}

// The fence is (link_id, version), not the lease token: a refresh whose
// lease expired and was taken over still lands when nobody else wrote.
func TestFenceIsLinkAndVersionNotLeaseToken(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		ctx := context.Background()
		a := h.pid(pidA)
		h.link(pidA, testSession("A1"))
		h.st.leaseTTL = 20 * time.Millisecond
		err := h.st.Update(ctx, a, func(ctx context.Context, s glovoapi.Session) (glovoapi.Session, error) {
			time.Sleep(60 * time.Millisecond)
			if _, err := h.raw.AcquireLease(ctx, pidA, "", "glh_takeover", time.Minute); err != nil {
				return nil, fmt.Errorf("takeover: %w", err)
			}
			s["accessToken"] = "access-late"
			return s, nil
		})
		if err != nil {
			t.Fatalf("write after lease expiry: %v", err)
		}
		if got := h.stored(pidA)["accessToken"]; got != "access-late" {
			t.Fatalf("stored %v", got)
		}
	})
}

// A rotated session whose write is unconfirmed is pinned: it is neither
// served nor evicted, and it lands once Postgres answers again (2.5).
func TestUnpersistedRefreshPinnedRefusedThenWritten(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		ctx := context.Background()
		a := h.pid(pidA)
		h.link(pidA, testSession("A1"))
		h.st.sleep = func(context.Context, time.Duration) error { return nil }
		sessions, _ := NewSessions(h.st, 0)
		c, err := sessions.Client(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		h.db.set("CommitLease", -1)
		h.db.set("FindWrite", -1)
		if err := c.Refresh(ctx); err != nil {
			t.Fatalf("refresh with unconfirmed write: %v", err)
		}
		at, _ := tokens(c)
		if at == "access-A1" {
			t.Fatal("client did not keep the rotated session")
		}
		if got := h.stored(pidA)["accessToken"]; got != "access-A1" {
			t.Fatalf("row changed: %v", got)
		}
		// Still failing: refused, not served, and not evicted.
		if _, err := sessions.Client(ctx, a); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("pinned client served: %v", err)
		}
		if _, err := c.GetBaskets(ctx); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("pinned client request served: %v", err)
		}
		sessions.idle = time.Nanosecond
		sessions.Sweep(ctx)
		if sessions.Len() != 1 {
			t.Fatal("pinned client evicted")
		}
		sessions.idle = time.Hour
		// Recovered: the write lands, and the same client serves again.
		h.db.set("CommitLease", 0)
		h.db.set("FindWrite", 0)
		c2, err := sessions.Client(ctx, a)
		if err != nil || c2 != c {
			t.Fatalf("after recovery: %v (same client %v)", err, c2 == c)
		}
		if got := h.stored(pidA)["accessToken"]; got != at {
			t.Fatalf("pending write not stored: %v", got)
		}
		if _, err := c.GetBaskets(ctx); err != nil {
			t.Fatalf("request after recovery: %v", err)
		}
	})
}

// A pinned client whose link_id saw another writer is discarded, not
// written over it (2.5).
func TestPinnedDiscardedWhenAnotherWriterWins(t *testing.T) {
	m := newMemDB()
	h := newHarness(t, m, m, nil, Caps{})
	ctx := context.Background()
	a := h.pid(pidA)
	h.link(pidA, testSession("A1"))
	h.st.sleep = func(context.Context, time.Duration) error { return nil }
	sessions, _ := NewSessions(h.st, 0)
	c, err := sessions.Client(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	h.db.set("CommitLease", -1)
	h.db.set("FindWrite", -1)
	if err := c.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.links[pidA].Version += 1000 // another writer on the same link_id
	m.mu.Unlock()
	h.db.set("CommitLease", 0)
	h.db.set("FindWrite", 0)
	if _, err := sessions.Client(ctx, a); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("pinned client after a competing write: %v", err)
	}
	if sessions.Len() != 0 {
		t.Fatal("superseded pinned client kept")
	}
	if got := h.stored(pidA)["accessToken"]; got != "access-A1" {
		t.Fatalf("competing write overwritten: %v", got)
	}
	if len(m.events("persist_discarded")) != 1 {
		t.Fatal("discard not audited")
	}
}

// A commit whose answer was lost is recognised by its write id.
func TestCommitOutcomeUnknownResolved(t *testing.T) {
	m := newMemDB()
	h := newHarness(t, m, m, nil, Caps{})
	ctx := context.Background()
	h.link(pidA, testSession("A1"))
	h.st.sleep = func(context.Context, time.Duration) error { return nil }
	l, _ := h.st.Get(ctx, h.pid(pidA))
	c, _ := l.Client()
	h.db.set("CommitLeaseLost", 1)
	if err := c.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if l.Unpersisted() {
		t.Fatal("landed write treated as unpersisted")
	}
	at, _ := tokens(c)
	if got := h.stored(pidA)["accessToken"]; got != at {
		t.Fatalf("stored %v, client %s", got, at)
	}
}

func TestDecryptNeedsAuditRow(t *testing.T) {
	m := newMemDB()
	h := newHarness(t, m, m, nil, Caps{})
	h.link(pidA, testSession("A1"))
	before := h.transit.decrypts.Load()
	h.db.set("InsertAudit", 1)
	if _, err := h.st.Get(context.Background(), h.pid(pidA)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("decrypt without audit row: %v", err)
	}
	if h.transit.decrypts.Load() != before {
		t.Fatal("decrypt ran without its audit row")
	}
}

func TestReadFailureIsUnavailableNotNotLinked(t *testing.T) {
	m := newMemDB()
	h := newHarness(t, m, m, nil, Caps{})
	ctx := context.Background()
	a := h.pid(pidA)
	if _, err := h.st.Get(ctx, a); !errors.Is(err, ErrNotLinked) {
		t.Fatalf("absent: %v", err)
	}
	h.db.set("ReadLink", 1)
	if _, err := h.st.Get(ctx, a); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrNotLinked) {
		t.Fatalf("failed read: %v", err)
	}
	h.db.set("ReadLink", 1)
	if _, err := h.st.Status(ctx, a); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("status failed read: %v", err)
	}
	h.link(pidA, testSession("A1"))
	sessions, _ := NewSessions(h.st, 0)
	if _, err := sessions.Client(ctx, a); err != nil {
		t.Fatal(err)
	}
	h.db.set("ReadLink", 1)
	if _, err := sessions.Client(ctx, a); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("cache served on failed read: %v", err)
	}
	h.db.set("ListLinks", 1)
	if _, err := h.st.Rewrap(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("rewrap with failed listing: %v", err)
	}
}

func TestLinkCodeFlow(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		ctx := context.Background()
		a := h.pid(pidA)
		code, exp, err := h.st.CreateLinkCode(ctx, a, "consent-v1")
		if err != nil || time.Until(exp) > linkCodeTTL || time.Until(exp) < linkCodeTTL-time.Minute {
			t.Fatalf("code: %v %v", exp, err)
		}
		if masked, err := h.st.PreviewLink(ctx, code); err != nil || masked != "a***@e***.com" {
			t.Fatalf("preview: %q %v", masked, err)
		}
		// A second code invalidates the first.
		code2, _, _ := h.st.CreateLinkCode(ctx, a, "consent-v1")
		if err := h.st.Link(ctx, code, testSession("A1")); !errors.Is(err, ErrCodeRefused) {
			t.Fatalf("superseded code: %v", err)
		}
		if err := h.st.Link(ctx, code2, testSession("A1")); err != nil {
			t.Fatal(err)
		}
		if err := h.st.Link(ctx, code2, testSession("A1")); !errors.Is(err, ErrCodeRefused) {
			t.Fatalf("reused code: %v", err)
		}
		for _, bad := range []string{"", "short", strings.Repeat("A", 44)} {
			if _, err := h.st.PreviewLink(ctx, bad); !errors.Is(err, ErrCodeRefused) {
				t.Fatalf("preview %q: %v", bad, err)
			}
		}
		// A failed validation still burns the code.
		code3, _, _ := h.st.CreateLinkCode(ctx, a, "consent-v1")
		if err := h.st.Link(ctx, code3, glovoapi.Session{"accessToken": "x"}); !errors.Is(err, ErrSessionRefused) {
			t.Fatalf("bad session: %v", err)
		}
		if err := h.st.Link(ctx, code3, testSession("A2")); !errors.Is(err, ErrCodeRefused) {
			t.Fatalf("burned code reused: %v", err)
		}
	})
}

func TestEraseIsPseudonymisedInAudit(t *testing.T) {
	m := newMemDB()
	h := newHarness(t, m, m, nil, Caps{})
	ctx := context.Background()
	a := h.pid(pidA)
	h.link(pidA, testSession("A1"))
	if _, err := h.st.Get(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.st.CreateLinkCode(ctx, a, "v1"); err != nil { // a live code beside the used one
		t.Fatal(err)
	}
	if m.codeRows(pidA) != 2 {
		t.Fatalf("setup: %d code rows", m.codeRows(pidA))
	}
	if err := h.st.Erase(ctx, a); err != nil {
		t.Fatal(err)
	}
	if n := m.codeRows(pidA); n != 0 {
		t.Fatalf("erasure left %d code rows with the pid", n)
	}
	if _, err := h.st.Get(ctx, a); !errors.Is(err, ErrNotLinked) {
		t.Fatalf("after erase: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.subjects {
		if s.pid == pidA {
			t.Fatal("pid survives in the audit subjects")
		}
	}
	if len(m.audit) == 0 || m.audit[0].subject == "" {
		t.Fatal("audit rows lost their subject")
	}
	erasures := 0
	for i := range m.audit {
		if m.audit[i].Event == "erasure" {
			erasures++
		}
	}
	if erasures != 1 {
		t.Fatalf("erasure events: %d", erasures)
	}
	if strings.Contains(h.logs.String(), pidA) {
		t.Fatal("pid in log lines")
	}
}

func TestRewrapSweep(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		ctx := context.Background()
		h.link(pidA, testSession("A1"))
		h.link(pidB, testSession("B1"))
		h.transit.rotate()
		rep, err := h.st.Rewrap(ctx)
		if err != nil || rep.Rewrapped != 2 {
			t.Fatalf("rewrap: %+v %v", rep, err)
		}
		for _, p := range []string{pidA, pidB} {
			if r := h.row(p); r.KeyVersion != 2 || !strings.HasPrefix(r.Ciphertext, "vault:v2:") {
				t.Fatalf("%s not rewrapped: %d", p, r.KeyVersion)
			}
			if _, err := h.st.Get(ctx, h.pid(p)); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// A sweep with the key unchanged writes nothing: the version stays, a
// cached client stays, and no extra decrypt is spent.
func TestRewrapNoOpWhenKeyUnchanged(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		ctx := context.Background()
		a := h.pid(pidA)
		h.link(pidA, testSession("A1"))
		sessions, _ := NewSessions(h.st, 0)
		c1, err := sessions.Client(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		v := h.row(pidA).Version
		decrypts := h.transit.decrypts.Load()
		for i := 0; i < 3; i++ {
			rep, err := h.st.Rewrap(ctx)
			if err != nil || rep.Rewrapped != 0 || rep.Current != 1 {
				t.Fatalf("sweep %d: %+v %v", i, rep, err)
			}
		}
		if got := h.row(pidA).Version; got != v {
			t.Fatalf("version moved without a key change: %d -> %d", v, got)
		}
		c2, err := sessions.Client(ctx, a)
		if err != nil || c2 != c1 {
			t.Fatalf("cached client evicted by a no-op sweep: %v", err)
		}
		if h.transit.decrypts.Load() != decrypts {
			t.Fatal("no-op sweep cost a decrypt")
		}
	})
}

func TestKeyVersionMismatchIsIntegrity(t *testing.T) {
	m := newMemDB()
	h := newHarness(t, m, m, nil, Caps{})
	h.link(pidA, testSession("A1"))
	m.mu.Lock()
	m.links[pidA].KeyVersion = 7
	m.mu.Unlock()
	before := h.transit.decrypts.Load()
	if _, err := h.st.Get(context.Background(), h.pid(pidA)); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("mismatch: %v", err)
	}
	if h.transit.decrypts.Load() != before {
		t.Fatal("decrypted a mismatched row")
	}
}

func TestHostedClientNeverUsesAFile(t *testing.T) {
	rc := func(ctx context.Context, held glovoapi.Session, ex func(context.Context, glovoapi.Session) (glovoapi.Session, error)) (glovoapi.Session, error) {
		return held, nil
	}
	if _, err := glovoapi.New("/tmp/should-not-be-used", glovoapi.WithRefreshCoordinator(rc)); !errors.Is(err, glovoapi.ErrHostedSessionPath) {
		t.Fatalf("hosted client with a session path: %v", err)
	}
	c, err := glovoapi.New("", glovoapi.WithRefreshCoordinator(rc), glovoapi.WithSession(testSession("A")))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Reload(); !errors.Is(err, glovoapi.ErrHostedNoFile) {
		t.Fatalf("hosted Reload: %v", err)
	}
	lat := 41.0
	if _, err := c.SetLocation(glovoapi.LocationUpdate{Latitude: &lat}); !errors.Is(err, glovoapi.ErrHostedNoFile) {
		t.Fatalf("hosted SetLocation: %v", err)
	}
}

func memHarness(t *testing.T, caps Caps) *harness {
	m := newMemDB()
	return newHarness(t, m, m, nil, caps)
}

// A caller that gives up while waiting for a held lease gets its context
// error, audited as a cancellation, never ErrBusy.
func TestAcquireCancellationIsNotBusy(t *testing.T) {
	m := newMemDB()
	h := newHarness(t, m, m, nil, Caps{})
	h.link(pidA, testSession("A1"))
	h.st.sleep = sleepCtx
	h.st.busyWait = time.Minute
	if _, err := h.raw.AcquireLease(context.Background(), pidA, "", "glh_other", time.Minute); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	err := h.st.Update(ctx, h.pid(pidA), func(ctx context.Context, s glovoapi.Session) (glovoapi.Session, error) {
		t.Error("fn ran without the lease")
		return s, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrBusy) {
		t.Fatalf("cancelled wait: %v", err)
	}
	var reasons []string
	for _, e := range m.events("refresh") {
		reasons = append(reasons, e.Reason)
	}
	if len(reasons) != 1 || reasons[0] != "canceled" {
		t.Fatalf("audit reasons %v", reasons)
	}
}

// A state change is a write: it takes a new version (2.3), so a refresh
// in flight across a suspension loses the fence and cannot land.
func TestStateChangeTakesVersionAndFencesRefresh(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		ctx := context.Background()
		a := h.pid(pidA)
		h.link(pidA, testSession("A1"))
		r := h.row(pidA)
		if err := h.raw.SetState(ctx, pidA, r.LinkID, StateActive); err != nil {
			t.Fatal(err)
		}
		if v := h.row(pidA).Version; v <= r.Version {
			t.Fatalf("state change kept version %d (was %d)", v, r.Version)
		}
		err := h.st.Update(ctx, a, func(ctx context.Context, s glovoapi.Session) (glovoapi.Session, error) {
			if err := h.raw.SetState(ctx, pidA, r.LinkID, StateSuspended); err != nil {
				return nil, err
			}
			s["accessToken"] = "access-after-suspend"
			return s, nil
		})
		if !errors.Is(err, ErrSuperseded) {
			t.Fatalf("refresh across a suspension: %v", err)
		}
		if got := h.row(pidA); got.State != StateSuspended {
			t.Fatalf("state %s", got.State)
		}
		if got := h.stored(pidA)["accessToken"]; got != "access-A1" {
			t.Fatalf("refresh landed on a suspended link: %v", got)
		}
		// The losing holder released its lease: once reactivated, the
		// link can be refreshed at once instead of waiting out the TTL.
		if err := h.raw.SetState(ctx, pidA, r.LinkID, StateActive); err != nil {
			t.Fatal(err)
		}
		if _, err := h.raw.AcquireLease(ctx, pidA, "", "glh_next", time.Minute); err != nil {
			t.Fatalf("lease left behind by the fence loser: %v", err)
		}
	})
}

// A caller cancelled during the refresh still gets its lease released and
// its audit row written: cleanup never runs on the caller's context.
func TestCancelledRefreshReleasesLeaseAndAudits(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		h.link(pidA, testSession("A1"))
		ctx, cancel := context.WithCancel(context.Background())
		err := h.st.Update(ctx, h.pid(pidA), func(ctx context.Context, s glovoapi.Session) (glovoapi.Session, error) {
			cancel()
			return nil, ctx.Err()
		})
		if err == nil {
			t.Fatal("cancelled refresh succeeded")
		}
		if _, err := h.raw.AcquireLease(context.Background(), pidA, "", "glh_next", time.Minute); err != nil {
			t.Fatalf("lease still held after a cancelled refresh: %v", err)
		}
		if h.mem != nil && len(h.mem.events("refresh")) != 1 {
			t.Fatalf("refresh audit rows: %d", len(h.mem.events("refresh")))
		}
	})
}

// A refresh Glovo accepted is persisted even when the caller stopped
// waiting right after: the rotated tokens are not lost to its context.
func TestCancelledCallerStillPersistsRotatedSession(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		h.link(pidA, testSession("A1"))
		ctx, cancel := context.WithCancel(context.Background())
		err := h.st.Update(ctx, h.pid(pidA), func(ctx context.Context, s glovoapi.Session) (glovoapi.Session, error) {
			s["accessToken"] = "access-rotated"
			cancel()
			return s, nil
		})
		if err != nil {
			t.Fatalf("rotated session not persisted: %v", err)
		}
		if got := h.stored(pidA)["accessToken"]; got != "access-rotated" {
			t.Fatalf("stored %v", got)
		}
	})
}

// Safety writes (the global kill, a pid suspension) land even on a
// cancelled caller.
func TestSafetyWritesSurviveCancelledCaller(t *testing.T) {
	t.Run("kill trip", func(t *testing.T) {
		forEachDB(t, func(t *testing.T, h *harness) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_ = h.st.kill.Trip(ctx, "test")
			if killed, err := h.raw.ReadControl(context.Background()); err != nil || !killed {
				t.Fatalf("trip on a cancelled caller not persisted: %v %v", killed, err)
			}
		})
	})
	t.Run("pid suspension", func(t *testing.T) {
		caps := DefaultCaps()
		caps.PIDDecryptsPerHour = 1
		m := newMemDB()
		h := newHarness(t, m, m, nil, caps)
		h.link(pidA, testSession("A1"))
		if _, err := h.st.Get(context.Background(), h.pid(pidA)); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := h.st.Get(ctx, h.pid(pidA)); !errors.Is(err, ErrSuspended) {
			t.Fatalf("cap breach: %v", err)
		}
		if st := h.row(pidA).State; st != StateSuspended {
			t.Fatalf("suspension on a cancelled caller not persisted: %s", st)
		}
	})
}
