package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

// lockKey is the session advisory lock that serialises concurrent
// migrators (two Job pods, a retry overlapping a slow first run).
const lockKey int64 = 0x676c6f766f633200 // "glovoc2\0"

type config struct {
	DSN         string
	ExpectRole  string
	RuntimeRole string
	AllowNoTLS  bool
}

type result struct {
	Outcome       string // "applied" or "already current"
	Version       int
	GrantsApplied bool
}

// refusedError is a deliberate no (exit 1). Every other error is a failure
// to observe or to apply (exit 3).
type refusedError struct{ msg string }

func (e refusedError) Error() string { return "refused: " + e.msg }

func refuse(format string, a ...any) error { return refusedError{fmt.Sprintf(format, a...)} }

func exitCode(err error) int {
	var r refusedError
	if errors.As(err, &r) {
		return exitRefused
	}
	return exitFailed
}

var roleName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

func migrate(ctx context.Context, c config) (result, error) {
	if !roleName.MatchString(c.ExpectRole) {
		return result{}, refuse("-expect-role is not a plain role name")
	}
	if c.RuntimeRole != "" && !roleName.MatchString(c.RuntimeRole) {
		return result{}, refuse("-runtime-role is not a plain role name")
	}
	if c.RuntimeRole == c.ExpectRole {
		return result{}, refuse("the runtime role must not be the migration role")
	}
	cfg, err := pgx.ParseConfig(c.DSN)
	if err != nil {
		// pgx's message can quote the DSN; say nothing of it.
		return result{}, refuse("the DSN cannot be parsed")
	}
	// TLS must be mandatory: sslmode=prefer/allow leave a plaintext
	// fallback, disable has no TLS config at all.
	if !c.AllowNoTLS && (cfg.TLSConfig == nil || len(cfg.Fallbacks) > 0) {
		return result{}, refuse("the DSN must set sslmode=require or stricter")
	}
	db := stdlib.OpenDB(*cfg)
	defer func() { _ = db.Close() }()

	// One dedicated session holds the advisory lock for the whole run.
	conn, err := db.Conn(ctx)
	if err != nil {
		return result{}, errors.New("cannot connect to the database")
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		return result{}, errors.New("cannot take the migration lock")
	}
	defer func() {
		// Closing the session releases it anyway; unlock explicitly on a
		// fresh context so a cancelled run does not leave it to the pool.
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, lockKey)
	}()

	var role string
	if err := conn.QueryRowContext(ctx, `SELECT current_user`).Scan(&role); err != nil {
		return result{}, errors.New("cannot read the connection role")
	}
	if role != c.ExpectRole {
		return result{}, refuse("connected as role %q, want %q", role, c.ExpectRole)
	}

	var res result
	var hasVersion, hasLinks bool
	if err := conn.QueryRowContext(ctx,
		`SELECT to_regclass('glovo_schema_version') IS NOT NULL, to_regclass('glovo_links') IS NOT NULL`,
	).Scan(&hasVersion, &hasLinks); err != nil {
		return result{}, errors.New("cannot read the catalog")
	}
	switch {
	case !hasVersion && hasLinks:
		return result{}, refuse("glovo_links exists without glovo_schema_version: partial schema, not touching it")
	case !hasVersion:
		if err := tokenstore.Migrate(ctx, db); err != nil {
			return result{}, fmt.Errorf("apply schema: %w", err)
		}
		res.Outcome = "applied"
	default:
		v, err := readVersion(ctx, conn)
		if err != nil {
			return result{}, err
		}
		if v != tokenstore.SchemaVersion {
			return result{}, refuse("schema version is %d, this build expects %d", v, tokenstore.SchemaVersion)
		}
		res.Outcome = "already current"
	}
	// Re-read after the fact: success is the observed version, not the
	// absence of an error.
	v, err := readVersion(ctx, conn)
	if err != nil {
		return result{}, err
	}
	if v != tokenstore.SchemaVersion {
		return result{}, fmt.Errorf("after migration the schema version is %d, want %d", v, tokenstore.SchemaVersion)
	}
	res.Version = v

	if c.RuntimeRole != "" {
		if err := grantRuntime(ctx, conn, c.RuntimeRole); err != nil {
			return result{}, err
		}
		res.GrantsApplied = true
	}
	return res, nil
}

// readVersion reads the single version row. A missing row, an unreadable
// table or a second row is an error, never version 0.
func readVersion(ctx context.Context, conn *sql.Conn) (int, error) {
	var n, v int
	if err := conn.QueryRowContext(ctx, `SELECT count(*), COALESCE(max(version), 0) FROM glovo_schema_version`).Scan(&n, &v); err != nil {
		return 0, errors.New("cannot read the schema version")
	}
	if n != 1 {
		return 0, fmt.Errorf("glovo_schema_version has %d rows, want 1", n)
	}
	return v, nil
}

// grantRuntime applies the app-role grants listed at the end of
// 0001_tokenstore.sql. GRANT is idempotent, so it runs on every invocation:
// a role created after the first migration still gets its grants.
func grantRuntime(ctx context.Context, conn *sql.Conn, role string) error {
	q := pgx.Identifier{role}.Sanitize()
	stmts := []string{
		`GRANT SELECT, INSERT, UPDATE, DELETE ON glovo_links, glovo_link_codes TO ` + q,
		`GRANT SELECT ON glovo_control, glovo_schema_version TO ` + q,
		`GRANT UPDATE (c2_killed, updated_at, updated_by) ON glovo_control TO ` + q,
		`GRANT SELECT, INSERT, UPDATE (pid, erased_at) ON glovo_audit_subjects TO ` + q,
		`GRANT SELECT, INSERT ON glovo_store_audit TO ` + q,
		`GRANT USAGE ON SEQUENCE glovo_link_version_seq TO ` + q,
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return errors.New("grants: cannot begin")
	}
	defer func() { _ = tx.Rollback() }()
	for _, s := range stmts {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("grants to %q failed (does the role exist?): %w", role, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return errors.New("grants: commit failed")
	}
	return nil
}
