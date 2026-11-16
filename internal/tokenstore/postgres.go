package tokenstore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"time"
)

// SchemaVersion is the schema this code expects. Postgres refuses to start
// on any other version (fail closed, mctl-api's glovo_schema_version
// pattern).
const SchemaVersion = 1

//go:embed migrations/*.sql
var migrations embed.FS

// Postgres implements DB over database/sql. It does not choose a driver:
// the caller opens *sql.DB with one (pgx's stdlib driver in production and
// in the integration test).
type Postgres struct{ db *sql.DB }

// NewPostgres checks the schema version and returns the DB. A version that
// could not be read, or a different version, is an error.
func NewPostgres(ctx context.Context, db *sql.DB) (*Postgres, error) {
	if db == nil {
		return nil, errors.New("tokenstore: nil *sql.DB")
	}
	var v int
	if err := db.QueryRowContext(ctx, `SELECT version FROM glovo_schema_version WHERE singleton`).Scan(&v); err != nil {
		return nil, fmt.Errorf("%w: read schema version", ErrUnavailable)
	}
	if v != SchemaVersion {
		return nil, fmt.Errorf("tokenstore: schema version %d, want %d", v, SchemaVersion)
	}
	return &Postgres{db: db}, nil
}

// Migrate applies the embedded migrations in one transaction. It runs as
// the migration owner role, never as the app role. It is not idempotent on
// purpose: it is for a fresh database.
func Migrate(ctx context.Context, db *sql.DB) error {
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, n := range names {
		body, err := migrations.ReadFile(n)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", n, err)
		}
	}
	return tx.Commit()
}

func unavailable(what string) error { return fmt.Errorf("%w: %s", ErrUnavailable, what) }

const linkColumns = `pid, link_id, state, ciphertext, key_version, version, refresh_failures,
	COALESCE(lease_token, ''), COALESCE(lease_until, 'epoch'::timestamptz)`

func scanLink(row interface{ Scan(...any) error }) (LinkRow, error) {
	var r LinkRow
	err := row.Scan(&r.PID, &r.LinkID, &r.State, &r.Ciphertext, &r.KeyVersion, &r.Version,
		&r.RefreshFailures, &r.LeaseHolder, &r.LeaseUntil)
	return r, err
}

func noRow(err error, what string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return errNoRow
	}
	return unavailable(what)
}

// ReadControl implements DB.
func (p *Postgres) ReadControl(ctx context.Context) (bool, error) {
	var killed bool
	if err := p.db.QueryRowContext(ctx, `SELECT c2_killed FROM glovo_control WHERE singleton`).Scan(&killed); err != nil {
		// No row is not "not killed": it is an unreadable control row.
		return true, unavailable("read control row")
	}
	return killed, nil
}

// SetKilled implements DB.
func (p *Postgres) SetKilled(ctx context.Context, killed bool, by string) error {
	res, err := p.db.ExecContext(ctx,
		`UPDATE glovo_control SET c2_killed = $1, updated_at = now(), updated_by = $2 WHERE singleton`, killed, by)
	if err != nil {
		return unavailable("write control row")
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return unavailable("write control row")
	}
	return nil
}

// ReadLink implements DB.
func (p *Postgres) ReadLink(ctx context.Context, pid string) (LinkRow, error) {
	r, err := scanLink(p.db.QueryRowContext(ctx, `SELECT `+linkColumns+` FROM glovo_links WHERE pid = $1`, pid))
	if err != nil {
		return LinkRow{}, noRow(err, "read link")
	}
	return r, nil
}

// AcquireLease implements DB.
func (p *Postgres) AcquireLease(ctx context.Context, pid, linkID, holder string, ttl time.Duration) (LinkRow, error) {
	r, err := scanLink(p.db.QueryRowContext(ctx, `
		UPDATE glovo_links
		   SET lease_token = $3,
		       lease_until = now() + make_interval(secs => $4)
		 WHERE pid = $1
		   AND ($2 = '' OR link_id = $2)
		   AND state = 'active'
		   AND (lease_until IS NULL OR lease_until <= now())
		RETURNING `+linkColumns, pid, linkID, holder, ttl.Seconds()))
	if err != nil {
		return LinkRow{}, noRow(err, "acquire lease")
	}
	return r, nil
}

// CommitLease implements DB.
func (p *Postgres) CommitLease(ctx context.Context, w LeaseCommit) (int64, error) {
	var v int64
	err := p.db.QueryRowContext(ctx, `
		UPDATE glovo_links
		   SET ciphertext = $4, key_version = $5, write_id = $6,
		       version = nextval('glovo_link_version_seq'),
		       lease_token = NULL, lease_until = NULL,
		       refresh_failures = CASE WHEN $7 THEN refresh_failures ELSE 0 END,
		       last_refresh_at = CASE WHEN $7 THEN last_refresh_at ELSE now() END,
		       updated_at = now()
		 WHERE pid = $1 AND link_id = $2 AND version = $3
		RETURNING version`,
		w.PID, w.LinkID, w.BaseVersion, w.Ciphertext, w.KeyVersion, w.WriteID, w.KeepRefreshState).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errFenceLost
	}
	if err != nil {
		return 0, unavailable("commit refresh")
	}
	return v, nil
}

// ReleaseLease implements DB.
func (p *Postgres) ReleaseLease(ctx context.Context, pid, linkID, holder string, addFailure bool, newState string) error {
	inc := 0
	if addFailure {
		inc = 1
	}
	_, err := p.db.ExecContext(ctx, `
		UPDATE glovo_links
		   SET lease_token = NULL, lease_until = NULL,
		       refresh_failures = refresh_failures + $4,
		       state = CASE WHEN $5 = '' THEN state ELSE $5 END,
		       version = CASE WHEN $5 = '' THEN version ELSE nextval('glovo_link_version_seq') END,
		       updated_at = now()
		 WHERE pid = $1 AND link_id = $2 AND lease_token = $3`,
		pid, linkID, holder, inc, newState)
	if err != nil {
		return unavailable("release lease")
	}
	return nil
}

// FindWrite implements DB.
func (p *Postgres) FindWrite(ctx context.Context, pid, writeID string) (int64, error) {
	var v int64
	err := p.db.QueryRowContext(ctx, `SELECT version FROM glovo_links WHERE pid = $1 AND write_id = $2`, pid, writeID).Scan(&v)
	if err != nil {
		return 0, noRow(err, "find write")
	}
	return v, nil
}

// ListLinks implements DB.
func (p *Postgres) ListLinks(ctx context.Context, afterPID string, limit int) ([]LinkRow, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT `+linkColumns+` FROM glovo_links WHERE pid > $1 ORDER BY pid LIMIT $2`, afterPID, limit)
	if err != nil {
		return nil, unavailable("list links")
	}
	defer func() { _ = rows.Close() }()
	out := []LinkRow{}
	for rows.Next() {
		r, err := scanLink(rows)
		if err != nil {
			return nil, unavailable("list links")
		}
		out = append(out, r)
	}
	// A failure after some rows is a failed read, not a shorter list.
	if err := rows.Err(); err != nil {
		return nil, unavailable("list links")
	}
	return out, nil
}

// CommitRewrap implements DB.
func (p *Postgres) CommitRewrap(ctx context.Context, pid, linkID string, baseVersion int64, ciphertext string, keyVersion int, writeID string) (int64, error) {
	var v int64
	err := p.db.QueryRowContext(ctx, `
		UPDATE glovo_links
		   SET ciphertext = $4, key_version = $5, write_id = $6,
		       version = nextval('glovo_link_version_seq'), updated_at = now()
		 WHERE pid = $1 AND link_id = $2 AND version = $3
		   AND (lease_until IS NULL OR lease_until <= now())
		RETURNING version`, pid, linkID, baseVersion, ciphertext, keyVersion, writeID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errFenceLost
	}
	if err != nil {
		return 0, unavailable("commit rewrap")
	}
	return v, nil
}

// SetState implements DB.
func (p *Postgres) SetState(ctx context.Context, pid, linkID, state string) error {
	res, err := p.db.ExecContext(ctx,
		`UPDATE glovo_links SET state = $3, version = nextval('glovo_link_version_seq'), updated_at = now()
		  WHERE pid = $1 AND link_id = $2`, pid, linkID, state)
	if err != nil {
		return unavailable("set state")
	}
	n, err := res.RowsAffected()
	if err != nil {
		return unavailable("set state")
	}
	if n == 0 {
		return errNoRow
	}
	return nil
}

// InsertLinkCode implements DB.
func (p *Postgres) InsertLinkCode(ctx context.Context, hash []byte, pid, consentVersion string, expires time.Time) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return unavailable("issue link code")
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`UPDATE glovo_link_codes SET used_at = now() WHERE pid = $1 AND used_at IS NULL`, pid); err != nil {
		return unavailable("issue link code")
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO glovo_link_codes (code_hash, pid, consent_version, expires_at) VALUES ($1, $2, $3, $4)`,
		hash, pid, consentVersion, expires); err != nil {
		return unavailable("issue link code")
	}
	if err := tx.Commit(); err != nil {
		return unavailable("issue link code")
	}
	return nil
}

// PeekLinkCode implements DB.
func (p *Postgres) PeekLinkCode(ctx context.Context, hash []byte) (string, error) {
	var pid string
	err := p.db.QueryRowContext(ctx,
		`SELECT pid FROM glovo_link_codes WHERE code_hash = $1 AND used_at IS NULL AND expires_at > now()`, hash).Scan(&pid)
	if err != nil {
		return "", noRow(err, "preview link code")
	}
	return pid, nil
}

// RedeemLinkCode implements DB.
func (p *Postgres) RedeemLinkCode(ctx context.Context, hash []byte) (string, string, error) {
	var pid, consent string
	err := p.db.QueryRowContext(ctx, `
		UPDATE glovo_link_codes SET used_at = now()
		 WHERE code_hash = $1 AND used_at IS NULL AND expires_at > now()
		RETURNING pid, consent_version`, hash).Scan(&pid, &consent)
	if err != nil {
		return "", "", noRow(err, "redeem link code")
	}
	return pid, consent, nil
}

// UpsertLink implements DB.
func (p *Postgres) UpsertLink(ctx context.Context, w LinkWrite) (int64, error) {
	var v int64
	err := p.db.QueryRowContext(ctx, `
		INSERT INTO glovo_links (pid, link_id, state, ciphertext, key_version, version, write_id, consent_version)
		VALUES ($1, $2, 'active', $3, $4, nextval('glovo_link_version_seq'), $5, $6)
		ON CONFLICT (pid) DO UPDATE
		   SET link_id = EXCLUDED.link_id, state = 'active',
		       ciphertext = EXCLUDED.ciphertext, key_version = EXCLUDED.key_version,
		       version = EXCLUDED.version, write_id = EXCLUDED.write_id,
		       consent_version = EXCLUDED.consent_version,
		       linked_at = now(), updated_at = now(), last_used_at = NULL,
		       last_refresh_at = NULL, access_expires_at = NULL, refresh_failures = 0,
		       lease_token = NULL, lease_until = NULL
		RETURNING version`,
		w.PID, w.LinkID, w.Ciphertext, w.KeyVersion, w.WriteID, w.ConsentVersion).Scan(&v)
	if err != nil {
		return 0, unavailable("store link")
	}
	return v, nil
}

// DeleteLink implements DB.
func (p *Postgres) DeleteLink(ctx context.Context, pid string, ev AuditEvent) error {
	return p.deleteLink(ctx, pid, ev, nil)
}

// EraseLink implements DB.
func (p *Postgres) EraseLink(ctx context.Context, pid string, unlink, erasure AuditEvent) error {
	return p.deleteLink(ctx, pid, unlink, &erasure)
}

func (p *Postgres) deleteLink(ctx context.Context, pid string, ev AuditEvent, erasure *AuditEvent) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return unavailable("unlink")
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM glovo_links WHERE pid = $1`, pid); err != nil {
		return unavailable("unlink")
	}
	// Unlink kills the live codes; erasure removes every code row of the
	// pid, used ones included, so no table keeps the pid in clear.
	codes := `DELETE FROM glovo_link_codes WHERE pid = $1 AND used_at IS NULL`
	if erasure != nil {
		codes = `DELETE FROM glovo_link_codes WHERE pid = $1`
	}
	if _, err := tx.ExecContext(ctx, codes, pid); err != nil {
		return unavailable("unlink")
	}
	if err := insertAudit(ctx, tx, ev); err != nil {
		return err
	}
	if erasure != nil {
		if err := insertAudit(ctx, tx, *erasure); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE glovo_audit_subjects SET pid = NULL, erased_at = now() WHERE pid = $1`, pid); err != nil {
			return unavailable("erase audit subject")
		}
	}
	if err := tx.Commit(); err != nil {
		return unavailable("unlink")
	}
	return nil
}

// InsertAudit implements DB.
func (p *Postgres) InsertAudit(ctx context.Context, ev AuditEvent) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return unavailable("write audit")
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertAudit(ctx, tx, ev); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return unavailable("write audit")
	}
	return nil
}

func insertAudit(ctx context.Context, tx *sql.Tx, ev AuditEvent) error {
	var subject sql.NullString
	if ev.PID != "" {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO glovo_audit_subjects (subject_id, pid) VALUES ($1, $2) ON CONFLICT (pid) DO NOTHING`,
			newID("gas_"), ev.PID); err != nil {
			return unavailable("write audit")
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT subject_id FROM glovo_audit_subjects WHERE pid = $1`, ev.PID).Scan(&subject); err != nil {
			return unavailable("write audit")
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO glovo_store_audit (id, subject_id, link_id, event, outcome, reason, actor_kind, request_id)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, $7, NULLIF($8, ''))`,
		newID("gsa_"), subject, ev.LinkID, ev.Event, ev.Outcome, ev.Reason, ev.Actor, ev.RequestID); err != nil {
		return unavailable("write audit")
	}
	return nil
}
