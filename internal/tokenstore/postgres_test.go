package tokenstore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
)

// These run only against a real Postgres (TOKENSTORE_TEST_PG_DSN). CI has
// none, so in CI the SQL is exercised only through memDB's model of it.
func pgOnly(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" {
		t.Skip(pgDSNEnv + " not set")
	}
	return openPostgres(t, dsn)
}

func TestPostgresAuditIsInsertOnlyAndErasable(t *testing.T) {
	db := pgOnly(t)
	ctx := context.Background()
	p, err := NewPostgres(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	ev := AuditEvent{PID: pidA, Event: "decrypt", Outcome: outcomeOK, Reason: "issued", Actor: actorService}
	if err := p.InsertAudit(ctx, ev); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE glovo_store_audit SET reason = 'x'`,
		`DELETE FROM glovo_store_audit`,
		`TRUNCATE glovo_store_audit`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err == nil {
			t.Fatalf("%s succeeded", stmt)
		}
	}
	unlink := AuditEvent{PID: pidA, Event: "unlink", Outcome: outcomeOK, Reason: "erasure", Actor: actorOperator}
	erasure := AuditEvent{PID: pidA, Event: "erasure", Outcome: outcomeOK, Reason: "erasure", Actor: actorOperator}
	if err := p.EraseLink(ctx, pidA, unlink, erasure); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM glovo_audit_subjects WHERE pid = $1`, pidA).Scan(&n); err != nil || n != 0 {
		t.Fatalf("pid survives erasure: %d %v", n, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM glovo_store_audit a JOIN glovo_audit_subjects s USING (subject_id) WHERE s.erased_at IS NOT NULL`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("audit row lost its subject: %d %v", n, err)
	}
}

func TestPostgresSchemaVersionAndShipsKilled(t *testing.T) {
	db := pgOnly(t)
	ctx := context.Background()
	p, err := NewPostgres(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if killed, err := p.ReadControl(ctx); err != nil || !killed {
		t.Fatalf("fresh schema not killed: %v %v", killed, err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM glovo_control`); err != nil {
		t.Fatal(err)
	}
	if killed, err := p.ReadControl(ctx); err == nil || !killed {
		t.Fatalf("missing control row: %v %v", killed, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE glovo_schema_version SET version = 2`); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPostgres(ctx, db); err == nil {
		t.Fatal("schema version mismatch accepted")
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE glovo_schema_version`); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPostgres(ctx, db); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unreadable schema version: %v", err)
	}
}
