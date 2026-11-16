package tokenstore

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// flipDB injects faults into the operator flip path.
type flipDB struct {
	DB
	mu         sync.Mutex
	failSet    bool
	failRead   bool
	failAudit  bool
	flipRead   bool          // ReadControl answers the opposite of the row (a concurrent writer)
	holdFirst  chan struct{} // the first ReadControl blocks until closed (after reading)
	holdedOnce bool
	entered    chan struct{}
	lastBy     string
	// beforeWrite runs inside SetKilled(true|false), before the write.
	beforeWrite func()
	// readAfterWrite makes ReadControl fail once a write has landed.
	failReadAfterWrite bool
	written            bool
	// staleRead is the value the held first ReadControl returns instead of
	// the row (a read that saw the row before the write).
	staleRead *bool
}

func (f *flipDB) SetKilled(ctx context.Context, killed bool, by string) error {
	f.mu.Lock()
	fail := f.failSet
	f.lastBy = by
	hook := f.beforeWrite
	f.beforeWrite = nil // one-shot
	f.mu.Unlock()
	if fail {
		return errInjected
	}
	if hook != nil {
		hook()
	}
	err := f.DB.SetKilled(ctx, killed, by)
	f.mu.Lock()
	f.written = true
	f.mu.Unlock()
	return err
}

func (f *flipDB) ReadControl(ctx context.Context) (bool, error) {
	f.mu.Lock()
	fail := f.failRead || (f.failReadAfterWrite && f.written)
	hold := f.holdFirst != nil && !f.holdedOnce
	if hold {
		f.holdedOnce = true
	}
	f.mu.Unlock()
	if fail {
		return false, errInjected
	}
	v, err := f.DB.ReadControl(ctx)
	if f.flipRead {
		v = !v
	}
	if hold && f.staleRead != nil {
		v = *f.staleRead
	}
	if hold {
		close(f.entered)
		<-f.holdFirst
	}
	return v, err
}

func (f *flipDB) InsertAudit(ctx context.Context, ev AuditEvent) error {
	f.mu.Lock()
	fail := f.failAudit
	f.mu.Unlock()
	if fail {
		return errInjected
	}
	return f.DB.InsertAudit(ctx, ev)
}

func newFlip(t *testing.T, raw DB) (*KillSwitch, *flipDB) {
	t.Helper()
	f := &flipDB{DB: raw}
	k, err := NewKillSwitch(f)
	if err != nil {
		t.Fatal(err)
	}
	return k, f
}

// The flip is durable: a new switch over the same database (a restart)
// reads what was stored, in both directions.
func TestKillSwitchSetPersistsAcrossRestart(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		ctx := context.Background()
		k, _ := newFlip(t, h.raw)
		stored, err := k.Set(ctx, false, "control-api")
		if err != nil || stored {
			t.Fatalf("unkill: %v %v", stored, err)
		}
		restarted, _ := NewKillSwitch(h.raw)
		if err := restarted.Check(ctx); err != nil {
			t.Fatalf("unkilled state lost on restart: %v", err)
		}
		stored, err = k.Set(ctx, true, "control-api")
		if err != nil || !stored {
			t.Fatalf("kill: %v %v", stored, err)
		}
		restarted2, _ := NewKillSwitch(h.raw)
		if err := restarted2.Check(ctx); !errors.Is(err, ErrKilled) {
			t.Fatalf("killed state lost on restart: %v", err)
		}
	})
}

// The answer is the state read back from the row, and a failed write, a
// failed read-back or a missing row is an error with "killed", never a state.
func TestKillSwitchSetFailuresAreErrorsNotStates(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		ctx := context.Background()
		k, f := newFlip(t, h.raw)
		f.failSet = true
		if stored, err := k.Set(ctx, false, "x"); err == nil || !stored {
			t.Fatalf("failed write: %v %v", stored, err)
		}
		f.failSet = false
		f.failRead = true
		if stored, err := k.Set(ctx, false, "x"); err == nil || !stored {
			t.Fatalf("failed read-back: %v %v", stored, err)
		}
		f.failRead = false
		var nilK *KillSwitch
		if _, err := nilK.Set(ctx, false, "x"); err == nil {
			t.Fatal("nil switch flipped")
		}
	})
	// A missing control row: the UPDATE touches nothing, the flip fails.
	m := newMemDB()
	m.noCtl = true
	k, _ := newFlip(t, m)
	if stored, err := k.Set(context.Background(), false, "x"); err == nil || !stored {
		t.Fatalf("missing row: %v %v", stored, err)
	}
}

// A read-back that differs from the request (another writer got in) is an
// error carrying the state actually stored, not a confirmation.
func TestKillSwitchSetRefusesAMismatchedReadBack(t *testing.T) {
	m := newMemDB()
	k, f := newFlip(t, m)
	f.flipRead = true
	stored, err := k.Set(context.Background(), false, "x")
	if err == nil || !stored {
		t.Fatalf("mismatch accepted: %v %v", stored, err)
	}
}

// The flip is visible at once in this process: no waiting out the cache.
func TestKillSwitchSetDropsTheRunningCache(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		ctx := context.Background()
		k, _ := newFlip(t, h.raw)
		if _, err := k.Set(ctx, false, "x"); err != nil {
			t.Fatal(err)
		}
		if err := k.Check(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := k.Set(ctx, true, "x"); err != nil {
			t.Fatal(err)
		}
		if err := k.Check(ctx); !errors.Is(err, ErrKilled) {
			t.Fatalf("cached running answer outlived a kill: %v", err)
		}
	})
}

// A Check that read the row before a flip must not cache its stale
// "running" answer after it (check-then-act).
func TestKillSwitchStaleReadDoesNotOutliveAFlip(t *testing.T) {
	m := newMemDB()
	if err := m.SetKilled(context.Background(), false, "t"); err != nil {
		t.Fatal(err)
	}
	k, f := newFlip(t, m)
	f.holdFirst = make(chan struct{})
	f.entered = make(chan struct{})
	res := make(chan error, 1)
	go func() { res <- k.Check(context.Background()) }()
	<-f.entered // read "running", not yet returned
	if _, err := k.Set(context.Background(), true, "x"); err != nil {
		t.Fatal(err)
	}
	close(f.holdFirst)
	if err := <-res; !errors.Is(err, ErrKilled) {
		t.Fatalf("stale read passed after a kill: %v", err)
	}
	if err := k.Check(context.Background()); !errors.Is(err, ErrKilled) {
		t.Fatalf("stale read was cached: %v", err)
	}
}

// A kill stays in force without its audit row; an un-kill does not open
// without one (and the row ends killed).
func TestKillSwitchAuditFailure(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		ctx := context.Background()
		k, f := newFlip(t, h.raw)
		if _, err := k.Set(ctx, false, "x"); err != nil {
			t.Fatal(err)
		}
		f.failAudit = true
		if stored, err := k.Set(ctx, true, "x"); err != nil || !stored {
			t.Fatalf("kill must hold without an audit row: %v %v", stored, err)
		}
		if killed, _ := h.raw.ReadControl(ctx); !killed {
			t.Fatal("kill did not land")
		}
		if stored, err := k.Set(ctx, false, "x"); err == nil || !stored {
			t.Fatalf("unkill without an audit row: %v %v", stored, err)
		}
		if killed, err := h.raw.ReadControl(ctx); err != nil || !killed {
			t.Fatalf("switch left open after a failed audit: %v %v", killed, err)
		}
		if err := k.Check(ctx); !errors.Is(err, ErrKilled) {
			t.Fatalf("Check after a failed unkill: %v", err)
		}
	})
}

// Each flip writes an audit row (kill_on / kill_off, operator).
func TestKillSwitchSetAuditsEveryFlip(t *testing.T) {
	m := newMemDB()
	k, _ := newFlip(t, m)
	ctx := context.Background()
	for _, killed := range []bool{false, true, false} {
		if _, err := k.Set(ctx, killed, "x"); err != nil {
			t.Fatal(err)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	want := []string{"kill_off", "kill_on", "kill_off"}
	if len(m.audit) != len(want) {
		t.Fatalf("audit rows: %d", len(m.audit))
	}
	for i, a := range m.audit {
		if a.Event != want[i] || a.Outcome != outcomeOK || a.Actor != actorOperator || a.PID != "" {
			t.Fatalf("row %d: %+v", i, a.AuditEvent)
		}
	}
}

// Postgres: the audit rows pass the table's CHECKs and land, and the
// flip writes updated_by.
func TestPostgresKillSwitchSetAuditRows(t *testing.T) {
	db := pgOnly(t)
	ctx := context.Background()
	p, err := NewPostgres(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := NewKillSwitch(p)
	if _, err := k.Set(ctx, false, "control-api"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Set(ctx, true, "control-api"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM glovo_store_audit WHERE event IN ('kill_on','kill_off') AND actor_kind = 'operator' AND outcome = 'ok' AND subject_id IS NULL`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("audit rows: %d %v", n, err)
	}
	var by string
	if err := db.QueryRowContext(ctx, `SELECT updated_by FROM glovo_control`).Scan(&by); err != nil || by != "control-api" {
		t.Fatalf("updated_by: %q %v", by, err)
	}
	// A missing row is a failed flip, with Postgres semantics.
	if _, err := db.ExecContext(ctx, `DELETE FROM glovo_control`); err != nil {
		t.Fatal(err)
	}
	if stored, err := k.Set(ctx, false, "control-api"); err == nil || !stored {
		t.Fatalf("flip with no control row: %v %v", stored, err)
	}
}

// A Check that starts after the first invalidate but reads the OLD row, and
// returns only after the write landed, must not cache "running" under the
// new generation (guards the second invalidate in Set).
func TestKillSwitchReadStartedMidFlipDoesNotCacheOldRunning(t *testing.T) {
	m := newMemDB()
	if err := m.SetKilled(context.Background(), false, "t"); err != nil {
		t.Fatal(err)
	}
	k, f := newFlip(t, m)
	old := false // the old row: running
	f.staleRead = &old
	f.holdFirst = make(chan struct{})
	f.entered = make(chan struct{})
	res := make(chan error, 1)
	f.beforeWrite = func() {
		// Inside Set, after the first invalidate, before the write.
		go func() { res <- k.Check(context.Background()) }()
		<-f.entered // the Check has read the old "running" row
	}
	if stored, err := k.Set(context.Background(), true, "x"); err != nil || !stored {
		t.Fatalf("kill: %v %v", stored, err)
	}
	close(f.holdFirst) // the Check returns after the write landed
	<-res
	if err := k.Check(context.Background()); !errors.Is(err, ErrKilled) {
		t.Fatalf("old running answer cached across a kill: %v", err)
	}
}

// If the un-kill commits but the read-back fails, the row is closed again
// and an error row is audited, never left open unrecorded.
func TestKillSwitchUnkillReadBackFailureReKills(t *testing.T) {
	m := newMemDB()
	m.killed = true // the store ships killed
	k, f := newFlip(t, m)
	f.failReadAfterWrite = true
	if stored, err := k.Set(context.Background(), false, "x"); err == nil || !stored {
		t.Fatalf("unkill with failed read-back: %v %v", stored, err)
	}
	f.failReadAfterWrite = false
	if killed, err := m.ReadControl(context.Background()); err != nil || !killed {
		t.Fatalf("row left open: %v %v", killed, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.audit) != 1 || m.audit[0].Event != "kill_off" || m.audit[0].Outcome != outcomeError {
		t.Fatalf("audit: %+v", m.audit)
	}
}

// While Trip-latched, an un-kill still writes the row and its audit record,
// but Set answers the effective state (killed), never the stored "open".
func TestKillSwitchUnkillWhileLatchedAnswersKilled(t *testing.T) {
	m := newMemDB()
	k, _ := newFlip(t, m)
	_ = k.Trip(context.Background(), "t")
	stored, err := k.Set(context.Background(), false, "x")
	if err != nil || !stored {
		t.Fatalf("unkill while latched: %v %v", stored, err)
	}
	if killed, _ := m.ReadControl(context.Background()); killed {
		t.Fatal("the un-kill row write was skipped")
	}
	m.mu.Lock()
	n := len(m.audit)
	m.mu.Unlock()
	if n != 1 {
		t.Fatalf("audit rows: %d", n)
	}
	if err := k.Check(context.Background()); !errors.Is(err, ErrKilled) {
		t.Fatalf("latch cleared by an un-kill: %v", err)
	}
}
