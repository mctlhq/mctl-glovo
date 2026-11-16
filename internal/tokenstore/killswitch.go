package tokenstore

import (
	"context"
	"errors"
	"sync"
	"time"
)

// KillSwitch is the runtime switch glovo_control.c2_killed (design 6).
//
// A "running" answer is cached for at most MaxCache (5 s). A killed
// answer and a failed read are never cached, and a failed read counts as
// killed. Trip latches the switch in this process until restart, even if
// writing the control row fails.
type KillSwitch struct {
	db       DB
	now      func() time.Time
	maxCache time.Duration

	// setMu serialises operator flips (Set).
	setMu sync.Mutex

	mu      sync.Mutex
	okUntil time.Time
	latched bool
	// gen counts flips. A read that began before a flip must not cache its
	// "running" answer after it (check-then-act).
	gen       uint64
	readCalls int // tests: how often the row was read
}

// MaxKillCache is the longest a "running" answer is reused.
const MaxKillCache = 5 * time.Second

// NewKillSwitch returns a switch over db's control row.
func NewKillSwitch(db DB) (*KillSwitch, error) {
	if db == nil {
		return nil, errors.New("tokenstore: kill switch needs a DB")
	}
	return &KillSwitch{db: db, now: time.Now, maxCache: MaxKillCache}, nil
}

// Check returns nil when C2 may run, ErrKilled otherwise.
func (k *KillSwitch) Check(ctx context.Context) error {
	if k == nil {
		return ErrKilled
	}
	k.mu.Lock()
	if k.latched {
		k.mu.Unlock()
		return ErrKilled
	}
	if k.now().Before(k.okUntil) {
		k.mu.Unlock()
		return nil
	}
	gen := k.gen
	k.mu.Unlock()

	killed, err := k.db.ReadControl(ctx)

	k.mu.Lock()
	defer k.mu.Unlock()
	k.readCalls++
	if err != nil || killed || k.latched {
		k.okUntil = time.Time{}
		return ErrKilled
	}
	if k.gen != gen {
		// A flip landed while the row was being read: this answer may be
		// stale. Not cached, and not trusted for this call either.
		return ErrKilled
	}
	k.okUntil = k.now().Add(k.maxCache)
	return nil
}

// Set is the operator flip. It writes glovo_control, reads the row back and
// returns the state now stored, so a caller can confirm the exact state. Any
// error means the state is unknown to the caller, never a state. The cached
// "running" answer is dropped, so this process sees the flip at once.
//
// Both flips are audited (kill_on / kill_off, actor operator). A kill stays
// in force if its audit row cannot be written. An un-kill whose audit row
// cannot be written is undone (killed again) and reported as an error: the
// switch only opens with a record of who opened it. Set does not clear a
// Trip latch: that holds until restart.
func (k *KillSwitch) Set(ctx context.Context, killed bool, by string) (bool, error) {
	if k == nil {
		return true, ErrUnavailable
	}
	k.setMu.Lock()
	defer k.setMu.Unlock()
	dctx, cancel := detached(ctx)
	defer cancel()

	event := "kill_off"
	if killed {
		event = "kill_on"
	}
	k.invalidate()
	if err := k.db.SetKilled(dctx, killed, by); err != nil {
		_ = k.db.InsertAudit(dctx, AuditEvent{Event: event, Outcome: outcomeError, Reason: "write_failed", Actor: actorOperator})
		return true, err
	}
	k.invalidate()
	stored, err := k.db.ReadControl(dctx)
	if err != nil {
		return true, err
	}
	if stored != killed {
		return stored, unavailable("control row differs from the request")
	}
	aerr := k.db.InsertAudit(dctx, AuditEvent{Event: event, Outcome: outcomeOK, Reason: "operator_control", Actor: actorOperator})
	if aerr != nil && !killed {
		// Undo: a failed audit must not leave the switch open.
		_ = k.db.SetKilled(dctx, true, by)
		k.invalidate()
		return true, aerr
	}
	return stored, nil
}

// invalidate drops the cached "running" answer and voids in-flight reads.
func (k *KillSwitch) invalidate() {
	k.mu.Lock()
	k.gen++
	k.okUntil = time.Time{}
	k.mu.Unlock()
}

// Trip latches the switch in this process and writes the control row.
// The latch holds even when the write fails.
func (k *KillSwitch) Trip(ctx context.Context, by string) error {
	k.mu.Lock()
	k.latched = true
	k.okUntil = time.Time{}
	k.mu.Unlock()
	// A safety write: it must land even if the caller has given up.
	dctx, cancel := detached(ctx)
	defer cancel()
	return k.db.SetKilled(dctx, true, by)
}
