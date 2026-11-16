package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	pgDSNEnv      = "TOKENSTORE_TEST_PG_DSN"
	pgRequiredEnv = "TOKENSTORE_TEST_PG_REQUIRED"
	sentinelPass  = "s3cr3t-sentinel-pw"
)

func randHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// pgEnv is a fresh schema on the test Postgres. dsn() is a DSN bound to it.
type pgEnv struct {
	t      *testing.T
	admin  *sql.DB
	schema string
	base   string
	user   string
}

func newPG(t *testing.T) *pgEnv {
	t.Helper()
	base := os.Getenv(pgDSNEnv)
	if base == "" {
		if os.Getenv(pgRequiredEnv) != "" {
			t.Fatalf("%s is set but %s is empty: the Postgres tests cannot run", pgRequiredEnv, pgDSNEnv)
		}
		t.Skip(pgDSNEnv + " not set")
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	schema := "gm_" + randHex(t)
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatalf("postgres from %s unusable: %v", pgDSNEnv, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		_ = admin.Close()
	})
	return &pgEnv{t: t, admin: admin, schema: schema, base: base, user: u.User.Username()}
}

func (p *pgEnv) dsn() string {
	u, _ := url.Parse(p.base)
	q := u.Query()
	q.Set("search_path", p.schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// exec runs a statement inside the schema, as the admin connection user.
func (p *pgEnv) exec(q string, a ...any) {
	p.t.Helper()
	if _, err := p.admin.Exec(`SET search_path = `+p.schema+`; `+q, a...); err != nil {
		// A multi-statement string cannot take arguments; run separately.
		tx, e2 := p.admin.Begin()
		if e2 != nil {
			p.t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, e := tx.Exec(`SET LOCAL search_path = ` + p.schema); e != nil {
			p.t.Fatal(e)
		}
		if _, e := tx.Exec(q, a...); e != nil {
			p.t.Fatal(e)
		}
		if e := tx.Commit(); e != nil {
			p.t.Fatal(e)
		}
	}
}

func (p *pgEnv) queryInt(q string) int {
	p.t.Helper()
	tx, err := p.admin.Begin()
	if err != nil {
		p.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`SET LOCAL search_path = ` + p.schema); err != nil {
		p.t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(q).Scan(&n); err != nil {
		p.t.Fatal(err)
	}
	return n
}

func (p *pgEnv) queryBool(q string) bool {
	p.t.Helper()
	tx, err := p.admin.Begin()
	if err != nil {
		p.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`SET LOCAL search_path = ` + p.schema); err != nil {
		p.t.Fatal(err)
	}
	var b bool
	if err := tx.QueryRow(q).Scan(&b); err != nil {
		p.t.Fatal(err)
	}
	return b
}

type out struct {
	code           int
	stdout, stderr string
}

func (p *pgEnv) run(extra ...string) out {
	p.t.Helper()
	return runWith(p.t, p.dsn(), append([]string{"-insecure-no-tls", "-expect-role", p.user}, extra...)...)
}

func runWith(t *testing.T, dsn string, args ...string) out {
	t.Helper()
	var so, se bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	getenv := func(k string) string {
		if k == "GLOVO_MIGRATE_DATABASE_URL" {
			return dsn
		}
		return ""
	}
	code := run(ctx, args, getenv, &so, &se)
	return out{code, so.String(), se.String()}
}

func TestFreshDatabaseIsMigratedAndShipsKilled(t *testing.T) {
	p := newPG(t)
	o := p.run()
	if o.code != exitOK || !strings.Contains(o.stdout, "applied at version 1") {
		t.Fatalf("code %d stdout %q stderr %q", o.code, o.stdout, o.stderr)
	}
	if got := p.queryInt(`SELECT version FROM glovo_schema_version`); got != 1 {
		t.Fatalf("version %d", got)
	}
	if !p.queryBool(`SELECT c2_killed FROM glovo_control`) {
		t.Fatal("the store must ship killed")
	}
}

// The core idempotence invariant: a rerun neither fails nor undoes an
// operator's decision. After the kill switch is turned off, a second run
// must leave it off.
func TestRerunIsANoOpAndKeepsKillSwitchState(t *testing.T) {
	p := newPG(t)
	if o := p.run(); o.code != exitOK {
		t.Fatalf("first run: %+v", o)
	}
	p.exec(`UPDATE glovo_control SET c2_killed = false, updated_by = 'operator'`)
	o := p.run()
	if o.code != exitOK || !strings.Contains(o.stdout, "already current at version 1") {
		t.Fatalf("second run: %+v", o)
	}
	if p.queryBool(`SELECT c2_killed FROM glovo_control`) {
		t.Fatal("a rerun reset the kill switch")
	}
	if got := p.queryInt(`SELECT count(*) FROM glovo_control`); got != 1 {
		t.Fatalf("glovo_control rows %d", got)
	}
}

func TestConcurrentRunsAllSucceedAndMigrateOnce(t *testing.T) {
	p := newPG(t)
	const n = 6
	var wg sync.WaitGroup
	outs := make([]out, n)
	start := make(chan struct{})
	for i := range outs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			outs[i] = p.run()
		}()
	}
	close(start)
	wg.Wait()
	applied := 0
	for _, o := range outs {
		if o.code != exitOK {
			t.Fatalf("a concurrent run failed: %+v", o)
		}
		if strings.Contains(o.stdout, "applied at") {
			applied++
		}
	}
	if applied != 1 {
		t.Fatalf("%d runs applied the schema, want exactly 1", applied)
	}
}

func TestOtherSchemaVersionIsRefused(t *testing.T) {
	p := newPG(t)
	if o := p.run(); o.code != exitOK {
		t.Fatalf("%+v", o)
	}
	p.exec(`UPDATE glovo_schema_version SET version = 2`)
	if o := p.run(); o.code != exitRefused {
		t.Fatalf("newer version: code %d, want %d (%q)", o.code, exitRefused, o.stderr)
	}
	p.exec(`UPDATE glovo_schema_version SET version = 0`)
	if o := p.run(); o.code != exitRefused {
		t.Fatalf("older version: code %d, want %d (%q)", o.code, exitRefused, o.stderr)
	}
}

// A partial schema (tables without a version row) is not "absent" and not
// "current": the migrator must stop, and must not try to create over it.
func TestPartialSchemaIsRefused(t *testing.T) {
	p := newPG(t)
	if o := p.run(); o.code != exitOK {
		t.Fatalf("%+v", o)
	}
	p.exec(`DROP TABLE glovo_schema_version`)
	o := p.run()
	if o.code != exitRefused || !strings.Contains(o.stderr, "partial schema") {
		t.Fatalf("code %d stderr %q", o.code, o.stderr)
	}
}

// A version table that exists but cannot be read as exactly one row is a
// failure to observe (exit 3), never version 0 or "absent" (which would
// migrate over it).
func TestUnreadableVersionIsAFailureNotAbsence(t *testing.T) {
	p := newPG(t)
	if o := p.run(); o.code != exitOK {
		t.Fatalf("%+v", o)
	}
	p.exec(`ALTER TABLE glovo_schema_version DISABLE TRIGGER ALL`)
	p.exec(`DELETE FROM glovo_schema_version`)
	o := p.run()
	if o.code != exitFailed {
		t.Fatalf("empty version table: code %d, want %d (%q)", o.code, exitFailed, o.stderr)
	}
	if got := p.queryInt(`SELECT count(*) FROM glovo_schema_version`); got != 0 {
		t.Fatalf("the migrator wrote to a schema it could not read: %d rows", got)
	}
}

// A version row that cannot be decoded is a failed read (3), not version 0
// (which would be a refusal) and not a pass.
func TestUndecodableVersionIsAFailure(t *testing.T) {
	p := newPG(t)
	if o := p.run(); o.code != exitOK {
		t.Fatalf("%+v", o)
	}
	p.exec(`ALTER TABLE glovo_schema_version ALTER COLUMN version TYPE text USING version::text`)
	p.exec(`UPDATE glovo_schema_version SET version = 'one'`)
	if o := p.run(); o.code != exitFailed {
		t.Fatalf("code %d, want %d (%q)", o.code, exitFailed, o.stderr)
	}
}

func TestWrongRoleIsRefusedBeforeAnyChange(t *testing.T) {
	p := newPG(t)
	o := runWith(t, p.dsn(), "-insecure-no-tls", "-expect-role", "glovo_c2_migrator_not_me")
	if o.code != exitRefused {
		t.Fatalf("code %d (%q)", o.code, o.stderr)
	}
	if got := p.queryBool(`SELECT to_regclass('glovo_schema_version') IS NULL`); !got {
		t.Fatal("schema created despite the wrong role")
	}
}

func TestRuntimeGrantsBothWays(t *testing.T) {
	p := newPG(t)
	role := "gm_rt_" + randHex(t)
	if _, err := p.admin.Exec(`CREATE ROLE ` + role + ` NOLOGIN`); err != nil {
		t.Skipf("cannot create a role here: %v", err)
	}
	t.Cleanup(func() {
		_, _ = p.admin.Exec(`DROP SCHEMA IF EXISTS ` + p.schema + ` CASCADE`)
		_, _ = p.admin.Exec(`DROP ROLE IF EXISTS ` + role)
	})
	o := p.run("-runtime-role", role)
	if o.code != exitOK || !strings.Contains(o.stdout, "runtime grants applied") {
		t.Fatalf("%+v", o)
	}
	// A second run keeps working and keeps the grants.
	if o := p.run("-runtime-role", role); o.code != exitOK {
		t.Fatalf("rerun: %+v", o)
	}
	priv := func(table, priv string) bool {
		return p.queryBool(`SELECT has_table_privilege('` + role + `', '` + table + `', '` + priv + `')`)
	}
	for _, c := range []struct {
		table, priv string
		want        bool
	}{
		{"glovo_links", "DELETE", true},
		{"glovo_link_codes", "INSERT", true},
		{"glovo_store_audit", "INSERT", true},
		{"glovo_store_audit", "UPDATE", false},
		{"glovo_store_audit", "DELETE", false},
		{"glovo_control", "INSERT", false},
		{"glovo_control", "DELETE", false},
		{"glovo_schema_version", "UPDATE", false},
	} {
		if got := priv(c.table, c.priv); got != c.want {
			t.Errorf("%s %s = %v, want %v", c.table, c.priv, got, c.want)
		}
	}
	if !p.queryBool(`SELECT has_column_privilege('` + role + `', 'glovo_control', 'c2_killed', 'UPDATE')`) {
		t.Error("runtime role cannot flip c2_killed")
	}
	if p.queryBool(`SELECT has_column_privilege('` + role + `', 'glovo_control', 'singleton', 'UPDATE')`) {
		t.Error("runtime role can update glovo_control.singleton")
	}
	if !p.queryBool(`SELECT has_sequence_privilege('` + role + `', 'glovo_link_version_seq', 'USAGE')`) {
		t.Error("runtime role cannot use the version sequence")
	}
}

// A runtime role that does not exist is a failure, not a silent skip, and
// leaves the (already applied) schema alone.
func TestMissingRuntimeRoleFails(t *testing.T) {
	p := newPG(t)
	o := p.run("-runtime-role", "gm_no_such_role_"+randHex(t))
	if o.code != exitFailed {
		t.Fatalf("code %d (%q)", o.code, o.stderr)
	}
	if got := p.queryInt(`SELECT version FROM glovo_schema_version`); got != 1 {
		t.Fatalf("version %d", got)
	}
}

// Flag handling and TLS need no database.

func TestRefusesWithoutTLS(t *testing.T) {
	for _, dsn := range []string{
		"postgres://u:" + sentinelPass + "@127.0.0.1:1/db?sslmode=disable",
		"postgres://u:" + sentinelPass + "@127.0.0.1:1/db?sslmode=prefer",
		"postgres://u:" + sentinelPass + "@127.0.0.1:1/db?sslmode=allow",
	} {
		o := runWith(t, dsn)
		if o.code != exitRefused {
			t.Errorf("%s: code %d, want %d (%q)", dsn[:40], o.code, exitRefused, o.stderr)
		}
		if strings.Contains(o.stderr+o.stdout, sentinelPass) {
			t.Errorf("output leaks the DSN password: %q", o.stderr)
		}
	}
}

func TestRefusesBadInput(t *testing.T) {
	if o := runWith(t, ""); o.code != exitRefused {
		t.Errorf("empty DSN env: code %d", o.code)
	}
	if o := runWith(t, "postgres://u@h/d?sslmode=require", "extra"); o.code != exitRefused {
		t.Errorf("stray argument: code %d", o.code)
	}
	if o := runWith(t, "postgres://u@h/d?sslmode=require", "-expect-role", `x"; DROP`); o.code != exitRefused {
		t.Errorf("bad role name: code %d", o.code)
	}
	if o := runWith(t, "postgres://u@h/d?sslmode=require", "-runtime-role", "glovo_c2_migrator"); o.code != exitRefused {
		t.Errorf("runtime role equal to migration role: code %d", o.code)
	}
	o := runWith(t, "not a dsn "+sentinelPass+"\x00")
	if o.code != exitRefused || strings.Contains(o.stderr, sentinelPass) {
		t.Errorf("garbage DSN: code %d stderr %q", o.code, o.stderr)
	}
}

// An unreachable database is a failed read (3), not a refusal and not
// success.
func TestUnreachableDatabaseIsAFailure(t *testing.T) {
	o := runWith(t, "postgres://u:"+sentinelPass+"@127.0.0.1:1/db?sslmode=disable&connect_timeout=2", "-insecure-no-tls")
	if o.code != exitFailed {
		t.Fatalf("code %d (%q)", o.code, o.stderr)
	}
	if strings.Contains(o.stderr+o.stdout, sentinelPass) {
		t.Errorf("output leaks the DSN password: %q", o.stderr)
	}
}

// dropObject removes one listed object from an applied schema, leaving a
// partial state for the probe tests.
func (p *pgEnv) dropObject(o schemaObject) {
	p.t.Helper()
	switch o.kind {
	case "table":
		p.exec(`DROP TABLE ` + o.name + ` CASCADE`)
	case "sequence":
		p.exec(`DROP SEQUENCE ` + o.name)
	case "index":
		p.exec(`DROP INDEX ` + o.name)
	case "function":
		p.exec(`DROP FUNCTION ` + o.name + `() CASCADE`)
	case "trigger":
		p.exec(`DROP TRIGGER ` + o.name + ` ON glovo_store_audit`)
	}
}

// Direction 1: a version row exists but any object of migration 0001 is
// missing. Never "already current".
func TestVersionWithAnyObjectMissingIsRefused(t *testing.T) {
	for _, o := range schemaObjects {
		if o.name == "glovo_schema_version" {
			continue
		}
		t.Run(o.name, func(t *testing.T) {
			p := newPG(t)
			if r := p.run(); r.code != exitOK {
				t.Fatalf("%+v", r)
			}
			p.dropObject(o)
			r := p.run()
			if r.code != exitRefused || !strings.Contains(r.stderr, "glovo_schema_version exists but ") || !strings.Contains(r.stderr, o.name) || strings.Contains(r.stdout, "already current") {
				t.Fatalf("code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
			}
		})
	}
}

// Direction 2: any object of migration 0001 exists without a version row,
// not just glovo_links.
func TestAnyObjectWithoutVersionIsRefused(t *testing.T) {
	for _, o := range schemaObjects {
		if o.name == "glovo_schema_version" {
			continue
		}
		t.Run(o.name, func(t *testing.T) {
			p := newPG(t)
			if r := p.run(); r.code != exitOK {
				t.Fatalf("%+v", r)
			}
			// Leave only this object: drop everything else.
			p.exec(`DROP TABLE glovo_schema_version, glovo_links, glovo_link_codes, glovo_control, glovo_store_audit, glovo_audit_subjects CASCADE`)
			p.exec(`DROP FUNCTION IF EXISTS glovo_store_audit_immutable() CASCADE`)
			p.exec(`DROP SEQUENCE IF EXISTS glovo_link_version_seq`)
			switch o.kind {
			case "table":
				p.exec(`CREATE TABLE ` + o.name + ` (x int)`)
			case "sequence":
				p.exec(`CREATE SEQUENCE ` + o.name)
			case "index":
				p.exec(`CREATE TABLE tmp_idx (x int); CREATE INDEX ` + o.name + ` ON tmp_idx (x)`)
			case "function":
				p.exec(`CREATE FUNCTION ` + o.name + `() RETURNS int LANGUAGE sql AS 'SELECT 1'`)
			case "trigger":
				p.exec(`CREATE TABLE glovo_store_audit (x int)`)
				p.exec(`CREATE FUNCTION tmp_trg() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END; $$`)
				p.exec(`CREATE TRIGGER ` + o.name + ` BEFORE INSERT ON glovo_store_audit FOR EACH ROW EXECUTE FUNCTION tmp_trg()`)
			}
			r := p.run()
			// The distinct message for this direction: objects exist without
			// the version row (not "version exists but X missing").
			if r.code != exitRefused || !strings.Contains(r.stderr, o.name+" exist without glovo_schema_version") || strings.Contains(r.stderr, "exists but") {
				t.Fatalf("code %d stderr %q", r.code, r.stderr)
			}
			if p.queryBool(`SELECT to_regclass('glovo_schema_version') IS NOT NULL`) {
				t.Fatal("the migrator created the schema over a partial state")
			}
		})
	}
}

// schemaObjects must list everything the migrations create. Applies them to
// an empty schema and compares with the catalog; a new migration that adds
// an unlisted object fails here.
func TestSchemaObjectsCoverTheMigrations(t *testing.T) {
	p := newPG(t)
	if r := p.run(); r.code != exitOK {
		t.Fatalf("%+v", r)
	}
	tx, err := p.admin.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`SET LOCAL search_path = ` + p.schema); err != nil {
		t.Fatal(err)
	}
	// Indexes that back a constraint exist iff their table does.
	rows, err := tx.Query(`
		SELECT CASE c.relkind WHEN 'r' THEN 'table' WHEN 'S' THEN 'sequence' WHEN 'i' THEN 'index' ELSE 'other:' || c.relkind::text END, c.relname
		  FROM pg_class c
		 WHERE c.relnamespace = $1::regnamespace
		   AND NOT EXISTS (SELECT 1 FROM pg_constraint k WHERE k.conindid = c.oid AND c.relkind = 'i')
		UNION ALL
		SELECT 'function', proname FROM pg_proc WHERE pronamespace = $1::regnamespace
		UNION ALL
		SELECT 'trigger', tgname FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
		 WHERE c.relnamespace = $1::regnamespace AND NOT t.tgisinternal`, p.schema)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	listed := map[schemaObject]bool{}
	for _, o := range schemaObjects {
		listed[o] = true
	}
	found := map[schemaObject]bool{}
	for rows.Next() {
		var o schemaObject
		if err := rows.Scan(&o.kind, &o.name); err != nil {
			t.Fatal(err)
		}
		found[o] = true
		if !listed[o] {
			t.Errorf("migrations create %s %s, missing from schemaObjects", o.kind, o.name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for o := range listed {
		if !found[o] {
			t.Errorf("schemaObjects lists %s %s, which the migrations do not create", o.kind, o.name)
		}
	}
}

// migrateWith runs migrate() in-process so a fault can be injected into the
// probe's database access.
func (p *pgEnv) migrateWith(ctx context.Context, runtimeRole string, wrap func(catalogQuery) catalogQuery) (result, error) {
	return migrate(ctx, config{DSN: p.dsn(), ExpectRole: p.user, RuntimeRole: runtimeRole, AllowNoTLS: true, wrapCatalog: wrap})
}

// A probe that cannot read the catalog is a failure (exit 3): nothing is
// applied, and nothing reports "applied" or "already current". Covers an
// error on the first read, an error mid-scan (after earlier objects were
// read as present or absent) and a context cancelled during the probe.
func TestProbeFailureIsAFailureNotAbsence(t *testing.T) {
	failAt := func(n int, err error) func(catalogQuery) catalogQuery {
		return func(real catalogQuery) catalogQuery {
			calls := 0
			return func(ctx context.Context, q, name string) (bool, error) {
				calls++
				if calls == n {
					return false, err
				}
				return real(ctx, q, name)
			}
		}
	}
	cancelAt := func(n int, cancel context.CancelFunc) func(catalogQuery) catalogQuery {
		return func(real catalogQuery) catalogQuery {
			calls := 0
			return func(ctx context.Context, q, name string) (bool, error) {
				calls++
				if calls == n {
					cancel() // the real query then fails on the dead context
				}
				return real(ctx, q, name)
			}
		}
	}
	cases := []struct {
		name  string
		apply bool // start from an applied schema instead of an empty one
		wrap  func(cancel context.CancelFunc) func(catalogQuery) catalogQuery
	}{
		{"first read fails, empty db", false, func(context.CancelFunc) func(catalogQuery) catalogQuery { return failAt(1, errors.New("boom")) }},
		{"mid-scan fails, empty db", false, func(context.CancelFunc) func(catalogQuery) catalogQuery { return failAt(5, errors.New("boom")) }},
		{"last read fails, empty db", false, func(context.CancelFunc) func(catalogQuery) catalogQuery {
			return failAt(len(schemaObjects), errors.New("boom"))
		}},
		{"mid-scan fails, applied db", true, func(context.CancelFunc) func(catalogQuery) catalogQuery { return failAt(5, errors.New("boom")) }},
		{"driver error, applied db", true, func(context.CancelFunc) func(catalogQuery) catalogQuery { return failAt(1, driver.ErrBadConn) }},
		{"context cancelled mid-scan, empty db", false, func(c context.CancelFunc) func(catalogQuery) catalogQuery { return cancelAt(4, c) }},
		{"context cancelled mid-scan, applied db", true, func(c context.CancelFunc) func(catalogQuery) catalogQuery { return cancelAt(4, c) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPG(t)
			if tc.apply {
				if r := p.run(); r.code != exitOK {
					t.Fatalf("%+v", r)
				}
				p.exec(`UPDATE glovo_control SET c2_killed = false`)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			res, err := p.migrateWith(ctx, "", tc.wrap(cancel))
			if err == nil {
				t.Fatalf("probe failure reported success: %+v", res)
			}
			if exitCode(err) != exitFailed {
				t.Fatalf("exit code %d, want %d (%v)", exitCode(err), exitFailed, err)
			}
			if res != (result{}) {
				t.Fatalf("a result accompanied the error: %+v", res)
			}
			present := p.queryBool(`SELECT to_regclass('glovo_schema_version') IS NOT NULL`)
			if present != tc.apply {
				t.Fatalf("schema present=%v, want %v: the failed probe changed the database", present, tc.apply)
			}
			if tc.apply && p.queryBool(`SELECT c2_killed FROM glovo_control`) {
				t.Fatal("the kill switch was reset by a failed probe")
			}
		})
	}
}

type blockingExecer struct{ called chan struct{} }

func (b blockingExecer) ExecContext(ctx context.Context, _ string, _ ...any) (sql.Result, error) {
	close(b.called)
	<-ctx.Done() // a dead socket: only the context frees us
	return nil, ctx.Err()
}

// The explicit unlock is bounded: a connection whose unlock never returns
// must not hold the run past the injected bound.
func TestUnlockIsBounded(t *testing.T) {
	b := blockingExecer{called: make(chan struct{})}
	done := make(chan struct{})
	start := time.Now()
	go func() {
		unlockSession(context.Background(), b, 50*time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("unlockSession did not return: the unlock is unbounded")
	}
	<-b.called
	if d := time.Since(start); d < 40*time.Millisecond {
		t.Fatalf("returned after %v, before the bound", d)
	}
}

// A cancelled run context must not cancel the unlock (that is the point of
// the fresh context), and the production bound stays small.
func TestUnlockBoundIsSmallAndSurvivesCancelledRun(t *testing.T) {
	if unlockTimeout <= 0 || unlockTimeout > 30*time.Second {
		t.Fatalf("unlockTimeout %v out of range", unlockTimeout)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var uctxErr error
	unlockSession(ctx, ctxProbe{&uctxErr}, time.Second)
	if uctxErr != nil {
		t.Fatalf("unlock context already dead: %v", uctxErr)
	}
}

type ctxProbe struct{ err *error }

func (c ctxProbe) ExecContext(ctx context.Context, _ string, _ ...any) (sql.Result, error) {
	*c.err = ctx.Err()
	return nil, nil
}
