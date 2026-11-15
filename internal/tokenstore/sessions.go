package tokenstore

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/mctlhq/mctl-glovo/internal/glovoapi"
)

// Sessions keeps one hosted Glovo client per pid in memory (design 4.2).
//
// Every Client call re-reads the row's state, link id and version, with no
// decrypt. A row that is gone, not active, on another link or at another
// version drops the cached client first, so unlink, suspension and relink
// take effect at once. A failed read refuses the call; it never falls back
// to the cache. The one exception to "drop on version change" is a client
// holding an unpersisted refresh: it is kept while the row still has the
// version it was refreshed from, and its write is retried first.
type Sessions struct {
	store Store
	idle  time.Duration
	opts  []glovoapi.Option
	now   func() time.Time

	mu    sync.Mutex
	m     map[string]*cached
	locks map[string]*sync.Mutex
}

type cached struct {
	lease    *Lease
	client   *glovoapi.Client
	lastUsed time.Time
}

// DefaultIdleTTL is how long an unused client is kept.
const DefaultIdleTTL = 10 * time.Minute

// NewSessions returns a cache over st. idle zero means DefaultIdleTTL.
func NewSessions(st Store, idle time.Duration, opts ...glovoapi.Option) (*Sessions, error) {
	if st == nil {
		return nil, errors.New("tokenstore: sessions need a store")
	}
	return &Sessions{
		store: st,
		idle:  orDur(idle, DefaultIdleTTL),
		opts:  opts,
		now:   time.Now,
		m:     map[string]*cached{},
		locks: map[string]*sync.Mutex{},
	}, nil
}

func (m *Sessions) pidLock(pid string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.locks[pid]
	if !ok {
		l = &sync.Mutex{}
		m.locks[pid] = l
	}
	return l
}

func (m *Sessions) get(pid string) *cached {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.m[pid]
}

func (m *Sessions) put(pid string, c *cached) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c == nil {
		delete(m.m, pid)
		return
	}
	m.m[pid] = c
}

func (m *Sessions) idleFor(c *cached, now time.Time) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return now.Sub(c.lastUsed)
}

func (m *Sessions) touch(c *cached, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c.lastUsed = now
}

// Drop forgets pid's client (unlink calls it).
func (m *Sessions) Drop(pid PID) { m.put(pid.id, nil) }

// DropAll forgets every client.
func (m *Sessions) DropAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m = map[string]*cached{}
}

// Len is the number of cached clients.
func (m *Sessions) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.m)
}

// Client returns pid's hosted client, decrypting on a miss.
func (m *Sessions) Client(ctx context.Context, pid PID) (*glovoapi.Client, error) {
	if !pid.valid() {
		return nil, ErrInvalidPID
	}
	l := m.pidLock(pid.id)
	l.Lock()
	defer l.Unlock()

	st, err := m.store.Status(ctx, pid)
	if err != nil {
		return nil, err
	}
	if st.Killed {
		m.DropAll()
		return nil, ErrKilled
	}
	if !st.Linked {
		m.put(pid.id, nil)
		return nil, ErrNotLinked
	}
	switch st.State {
	case StateActive:
	case StateSuspended:
		m.put(pid.id, nil)
		return nil, ErrSuspended
	case StateNeedsRelink:
		m.put(pid.id, nil)
		return nil, ErrNeedsRelink
	default:
		m.put(pid.id, nil)
		return nil, ErrIntegrity
	}

	now := m.now()
	if c := m.get(pid.id); c != nil {
		persisted := false
		if c.lease.LinkID() == st.LinkID && c.lease.Unpersisted() {
			// Pinned (2.5): its expected row is the pre-refresh pair, so a
			// version mismatch alone does not evict it. Retry the write; a
			// lost COMMIT that actually landed is found by its write_id.
			switch err := c.lease.persistPending(ctx); {
			case errors.Is(err, errPendingConflict):
				// Another writer won on this link_id: discard.
				m.put(pid.id, nil)
				return nil, ErrSuperseded
			case err != nil:
				// Neither served nor evicted: the pinned client holds
				// the only copy of the rotated refresh token.
				return nil, ErrUnavailable
			}
			// The fenced write just landed (or was found), so this
			// client is the current state even though st predates it.
			persisted = true
		}
		keep := c.lease.LinkID() == st.LinkID && (persisted || c.lease.Version() == st.Version)
		if keep && m.idleFor(c, now) > m.idle {
			keep = false
		}
		if keep {
			m.touch(c, now)
			return c.client, nil
		}
		m.put(pid.id, nil)
	}

	lease, err := m.store.Get(ctx, pid)
	if err != nil {
		return nil, err
	}
	client, err := lease.Client(m.opts...)
	if err != nil {
		return nil, err
	}
	m.put(pid.id, &cached{lease: lease, client: client, lastUsed: now})
	return client, nil
}

// Sweep drops idle clients, and every client when the kill switch is on or
// unreadable. Run it at least every MaxKillCache.
func (m *Sessions) Sweep(ctx context.Context) {
	if m.store.Running(ctx) != nil {
		m.DropAll()
		return
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for pid, c := range m.m {
		if now.Sub(c.lastUsed) > m.idle && !c.lease.Unpersisted() {
			delete(m.m, pid)
		}
	}
}
