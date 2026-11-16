package tokenstore

import (
	"context"
	"errors"
	"time"
)

// DB is the persistence layer. Each method is one statement or one
// transaction. Postgres implements it (postgres.go); tests use a fake with
// the same semantics.
//
// Contract, for every method: a failed or partial read is an error, never
// an empty result. errNoRow is returned only for a documented absence (a
// query that ran and matched nothing).
type DB interface {
	// ReadControl returns glovo_control.c2_killed. A missing row is an
	// error, not "not killed".
	ReadControl(ctx context.Context) (killed bool, err error)
	// SetKilled writes glovo_control.c2_killed.
	SetKilled(ctx context.Context, killed bool, by string) error

	// ReadLink returns pid's row, or errNoRow.
	ReadLink(ctx context.Context, pid string) (LinkRow, error)
	// AcquireLease sets the refresh lease on pid's active row when no
	// unexpired lease is held, and returns the row. linkID "" matches any
	// link. Nothing matched is errNoRow; the caller re-reads to classify.
	AcquireLease(ctx context.Context, pid, linkID, holder string, ttl time.Duration) (LinkRow, error)
	// CommitLease stores a new ciphertext if the row still has linkID and
	// baseVersion, clears the lease and (unless KeepRefreshState) the failure counter, and returns
	// the new version. The fence is (linkID, baseVersion), never the lease
	// token: a lease only keeps two holders from calling Glovo at once, so
	// a write still lands after its lease expired, as long as nobody else
	// wrote. A failed fence is errFenceLost.
	CommitLease(ctx context.Context, w LeaseCommit) (int64, error)
	// ReleaseLease clears holder's lease. addFailure increments
	// refresh_failures; newState, when not "", replaces state.
	ReleaseLease(ctx context.Context, pid, linkID, holder string, addFailure bool, newState string) error
	// FindWrite returns the version of pid's row if its write_id is
	// writeID, or errNoRow. It resolves a commit with an unknown outcome.
	FindWrite(ctx context.Context, pid, writeID string) (int64, error)

	// ListLinks returns up to limit rows with pid > afterPID, by pid.
	ListLinks(ctx context.Context, afterPID string, limit int) ([]LinkRow, error)
	// CommitRewrap stores a rewrapped ciphertext if the row still has
	// linkID and baseVersion and no unexpired lease. A failed fence is
	// errFenceLost.
	CommitRewrap(ctx context.Context, pid, linkID string, baseVersion int64, ciphertext string, keyVersion int, writeID string) (int64, error)
	// SetState changes the state of pid's row if it still has linkID, and
	// takes a new version like every other write (2.3), so a refresh in
	// flight across the change loses its fence.
	// Nothing matched is errNoRow.
	SetState(ctx context.Context, pid, linkID, state string) error

	// InsertLinkCode stores a code hash, after marking every live code of
	// the pid used, in one transaction.
	InsertLinkCode(ctx context.Context, hash []byte, pid, consentVersion string, expires time.Time) error
	// PeekLinkCode returns the pid of a live code without burning it, or
	// errNoRow.
	PeekLinkCode(ctx context.Context, hash []byte) (string, error)
	// RedeemLinkCode burns a live code atomically and returns its pid and
	// consent version, or errNoRow.
	RedeemLinkCode(ctx context.Context, hash []byte) (pid, consentVersion string, err error)
	// UpsertLink stores a new link for pid, replacing any previous one,
	// with a fresh version from the sequence, and returns that version.
	UpsertLink(ctx context.Context, w LinkWrite) (int64, error)
	// DeleteLink deletes pid's row and live codes and inserts ev, in one
	// transaction. It succeeds when there was no row.
	DeleteLink(ctx context.Context, pid string, ev AuditEvent) error

	// InsertAudit appends one audit row.
	InsertAudit(ctx context.Context, ev AuditEvent) error
	// EraseLink is DeleteLink(unlink) plus the erasure event and the
	// pseudonymisation of pid's audit subject (Q8), all in one
	// transaction. The subject keeps its id; its pid is cleared, so no
	// live row maps the audit history back to the pid.
	EraseLink(ctx context.Context, pid string, unlink, erasure AuditEvent) error
}

// LinkRow is one glovo_links row, without timestamps the store does not use.
type LinkRow struct {
	PID             string
	LinkID          string
	State           string
	Ciphertext      string
	KeyVersion      int
	Version         int64
	RefreshFailures int
	LeaseHolder     string
	LeaseUntil      time.Time
}

// LeaseCommit is the fenced write after a refresh.
type LeaseCommit struct {
	PID, LinkID string
	BaseVersion int64
	Ciphertext  string
	KeyVersion  int
	WriteID     string
	// KeepRefreshState leaves refresh_failures and last_refresh_at as they
	// are: the write is not a refresh (a location change), so it must neither
	// clear the needs_relink failure count nor count as a refresh.
	KeepRefreshState bool
}

// LinkWrite is a new link.
type LinkWrite struct {
	PID, LinkID, ConsentVersion string
	Ciphertext                  string
	KeyVersion                  int
	WriteID                     string
}

// Link states.
const (
	StateActive      = "active"
	StateSuspended   = "suspended"
	StateNeedsRelink = "needs_relink"
)

var (
	errNoRow     = errors.New("tokenstore: no row")
	errFenceLost = errors.New("tokenstore: write fence lost")
)
