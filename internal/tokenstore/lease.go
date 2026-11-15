package tokenstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/mctlhq/mctl-glovo/internal/glovoapi"
)

// Lease is a decrypted session. It has no exported field and no method
// that returns the session: it renders, marshals and logs as "[redacted]",
// and it only builds the per-pid hosted Glovo client.
type Lease struct {
	store *store
	pid   PID

	mu      sync.Mutex
	linkID  string
	version int64
	session glovoapi.Session
	pending *pendingWrite
}

const redacted = "[redacted]"

// String implements fmt.Stringer.
func (l *Lease) String() string { return redacted }

// GoString implements fmt.GoStringer.
func (l *Lease) GoString() string { return redacted }

// Format implements fmt.Formatter, so every verb (%v, %+v, %#v, %s, %q,
// %x) prints the marker.
func (l *Lease) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

// MarshalJSON implements json.Marshaler.
func (l *Lease) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText implements encoding.TextMarshaler.
func (l *Lease) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// LogValue implements slog.LogValuer.
func (l *Lease) LogValue() slog.Value { return slog.StringValue(redacted) }

// Version is the row version this lease reflects.
func (l *Lease) Version() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.version
}

// LinkID is the link this lease decrypted.
func (l *Lease) LinkID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.linkID
}

// Unpersisted reports a refreshed session that Postgres has not confirmed.
func (l *Lease) Unpersisted() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pending != nil
}

// Client builds a hosted glovoapi.Client for this session. It never has a
// session path: refreshes go through the store's lease and fence, and every
// request passes the kill switch and the request caps. The store's
// ClientOptions apply first, then opts.
func (l *Lease) Client(opts ...glovoapi.Option) (*glovoapi.Client, error) {
	l.mu.Lock()
	sess := copySession(l.session)
	l.mu.Unlock()
	all := make([]glovoapi.Option, 0, len(l.store.clientOpts)+len(opts)+3)
	all = append(all, l.store.clientOpts...)
	all = append(all, opts...)
	all = append(all,
		glovoapi.WithSession(sess),
		glovoapi.WithRefreshCoordinator(l.coordinate),
		glovoapi.WithRequestGate(l.gate),
	)
	return glovoapi.New("", all...)
}

// gate runs before every Glovo request of the lease's clients. A pinned
// (unpersisted) lease serves nothing: it first retries the write, and
// refuses the request while the write is not confirmed (2.5).
func (l *Lease) gate(ctx context.Context) error {
	s := l.store
	if err := s.kill.Check(ctx); err != nil {
		return ErrKilled
	}
	if err := l.persistPending(ctx); err != nil {
		if errors.Is(err, errPendingConflict) {
			return ErrSuperseded
		}
		return ErrUnavailable
	}
	now := s.now()
	if allowAll(now, capHit{s.lim.globalReqMin, ""}, capHit{s.lim.pidReqMin, l.pid.id}, capHit{s.lim.pidReqDay, l.pid.id}) >= 0 {
		s.log.LogAttrs(ctx, slog.LevelWarn, "tokenstore", slog.String("event", "cap_exceeded"), slog.String("reason", "rate_limited"), slog.String("link_id", l.LinkID()))
		return ErrRateLimited
	}
	return nil
}

func tokenOf(s glovoapi.Session, k string) string {
	v, _ := s[k].(string)
	return v
}

// coordinate is the client's RefreshCoordinator. It refreshes only if the
// stored session still carries the tokens the client holds; otherwise
// another holder already rotated them and the stored session is adopted.
func (l *Lease) coordinate(ctx context.Context, held glovoapi.Session, exchange func(context.Context, glovoapi.Session) (glovoapi.Session, error)) (glovoapi.Session, error) {
	linkID := l.LinkID()
	if l.Unpersisted() {
		// Rotating again before the last rotation is stored would spend
		// the only copy of the newest refresh token.
		if err := l.persistPending(ctx); err != nil {
			return nil, err
		}
	}
	next, ver, p, err := l.store.update(ctx, l.pid, linkID, actorService, func(ctx context.Context, cur glovoapi.Session) (glovoapi.Session, bool, error) {
		if tokenOf(cur, "refreshToken") != tokenOf(held, "refreshToken") || tokenOf(cur, "accessToken") != tokenOf(held, "accessToken") {
			return cur, false, nil
		}
		n, err := exchange(ctx, cur)
		return n, err == nil, err
	})
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.version, l.pending, l.session = ver, p, copySession(next)
	l.mu.Unlock()
	return next, nil
}

// errPendingConflict means the row moved on while a refreshed session was
// waiting to be stored: that session is lost.
var errPendingConflict = errors.New("tokenstore: unpersisted session superseded")

// persistPending retries the write of an unpersisted session.
func (l *Lease) persistPending(ctx context.Context) error {
	l.mu.Lock()
	p := l.pending
	l.mu.Unlock()
	if p == nil {
		return nil
	}
	v, err := l.store.commit(ctx, l.pid, p)
	if errors.Is(err, errFenceLost) {
		_ = l.store.audit(ctx, AuditEvent{PID: l.pid.id, LinkID: p.linkID, Event: "persist_discarded", Outcome: outcomeRefused, Reason: "fence_lost"})
		return errPendingConflict
	}
	if err != nil {
		return ErrUnavailable
	}
	l.mu.Lock()
	if l.pending == p {
		l.pending, l.version = nil, v
	}
	l.mu.Unlock()
	_ = l.store.audit(ctx, AuditEvent{PID: l.pid.id, LinkID: p.linkID, Event: "persist_retry", Outcome: outcomeOK, Reason: "persisted"})
	return nil
}
