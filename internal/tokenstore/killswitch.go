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

	mu        sync.Mutex
	okUntil   time.Time
	latched   bool
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
	k.mu.Unlock()

	killed, err := k.db.ReadControl(ctx)

	k.mu.Lock()
	defer k.mu.Unlock()
	k.readCalls++
	if err != nil || killed || k.latched {
		k.okUntil = time.Time{}
		return ErrKilled
	}
	k.okUntil = k.now().Add(k.maxCache)
	return nil
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
