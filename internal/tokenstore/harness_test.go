package tokenstore

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/mctlhq/mctl-glovo/internal/glovoapi"
)

// pgDSNEnv names a Postgres the integration runs use: any database the
// user may CREATE SCHEMA in. With it, every forEachDB test runs against the
// real SQL as well as memDB, and the Postgres-only tests run. Without it
// they skip, which is fine locally.
const pgDSNEnv = "TOKENSTORE_TEST_PG_DSN"

// pgRequiredEnv, when set (CI sets it), makes a missing DSN a failure
// rather than a skip. CI must never pass because it could not reach the
// database: "could not observe" is not "observed clean". An unreachable
// database with the DSN set already fails in openPostgres.
const pgRequiredEnv = "TOKENSTORE_TEST_PG_REQUIRED"

// pgDSN returns the DSN, failing the test when Postgres is required but no
// DSN is configured.
func pgDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(pgDSNEnv)
	if dsn == "" && os.Getenv(pgRequiredEnv) != "" {
		t.Fatalf("%s is set but %s is empty: the Postgres tests cannot run", pgRequiredEnv, pgDSNEnv)
	}
	return dsn
}

type backend struct {
	name string
	open func(t *testing.T) (DB, *memDB, func(a, b string))
}

func backends(t *testing.T) []backend {
	out := []backend{{name: "mem", open: func(*testing.T) (DB, *memDB, func(a, b string)) {
		m := newMemDB()
		swap := func(a, b string) {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.links[a].Ciphertext, m.links[b].Ciphertext = m.links[b].Ciphertext, m.links[a].Ciphertext
		}
		return m, m, swap
	}}}
	if dsn := pgDSN(t); dsn != "" {
		out = append(out, backend{name: "postgres", open: func(t *testing.T) (DB, *memDB, func(a, b string)) {
			db := openPostgres(t, dsn)
			p, err := NewPostgres(context.Background(), db)
			if err != nil {
				t.Fatal(err)
			}
			swap := func(a, b string) {
				if _, err := db.Exec(`UPDATE glovo_links x SET ciphertext = y.ciphertext FROM glovo_links y
					WHERE (x.pid = $1 AND y.pid = $2) OR (x.pid = $2 AND y.pid = $1)`, a, b); err != nil {
					t.Fatal(err)
				}
			}
			return p, nil, swap
		}})
	}
	return out
}

// openPostgres migrates a fresh schema and returns a pool bound to it.
func openPostgres(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	schema := "ts_" + strings.ToLower(newID(""))
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		// Unreachable or refused: a failure, never a skip.
		t.Fatalf("postgres from %s unusable: %v", pgDSNEnv, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		_ = admin.Close()
	})
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}

func forEachDB(t *testing.T, fn func(t *testing.T, h *harness)) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			raw, mem, swap := b.open(t)
			fn(t, newHarness(t, raw, mem, swap, Caps{}))
		})
	}
}

func newHarness(t *testing.T, raw DB, mem *memDB, swap func(a, b string), caps Caps) *harness {
	t.Helper()
	h := &harness{t: t, raw: raw, mem: mem, swap: swap, logs: &syncBuffer{}}
	h.db = newFaultDB(raw)
	h.transit = newFakeTransit(t)
	h.glovo = newFakeGlovo(t)
	if err := raw.SetKilled(context.Background(), false, "test"); err != nil {
		t.Fatal(err)
	}
	k, err := NewKillSwitch(h.db)
	if err != nil {
		t.Fatal(err)
	}
	h.kill = k
	st, err := New(Config{
		DB:      h.db,
		Transit: h.transit.client(t),
		Kill:    k,
		Caps:    caps,
		Logger:  slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		ValidateLink: func(context.Context, glovoapi.Session) error {
			return nil
		},
		MaskIdentity:  func(context.Context, string) (string, error) { return "a***@e***.com", nil },
		ClientOptions: []glovoapi.Option{glovoapi.WithBaseURL(h.glovo.srv.URL)},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.st = st.(*store)
	return h
}

func (h *harness) pid(s string) PID {
	p := pidFromString(s)
	if !p.valid() {
		h.t.Fatalf("bad test pid %q", s)
	}
	return p
}

// link runs the code flow for pid with sess and registers its tokens with
// the fake Glovo.
func (h *harness) link(pid string, sess glovoapi.Session) {
	h.t.Helper()
	ctx := context.Background()
	code, _, err := h.st.CreateLinkCode(ctx, h.pid(pid), "consent-v1")
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.Link(ctx, code, sess); err != nil {
		h.t.Fatal(err)
	}
	h.glovo.issue(sess)
}

func (h *harness) row(pid string) LinkRow {
	h.t.Helper()
	r, err := h.raw.ReadLink(context.Background(), pid)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

// stored decrypts pid's row directly, bypassing the store and its caps.
func (h *harness) stored(pid string) glovoapi.Session {
	h.t.Helper()
	r := h.row(pid)
	pt, err := h.transit.open(keyContext(h.pid(pid), r.LinkID), r.Ciphertext)
	if err != nil {
		h.t.Fatal(err)
	}
	s, err := decodeSession(pt)
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}
