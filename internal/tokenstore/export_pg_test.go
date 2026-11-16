package tokenstore

import (
	"context"
	"database/sql"
	"log/slog"
	"testing"
	"time"

	"github.com/mctlhq/mctl-glovo/internal/glovoapi"
)

// PGIso gives the isolation suite (isolation_pg_test.go, package
// tokenstore_test) a real store over a real Postgres schema and the fake
// Vault transit. Unlike LinkHarness it never uses memDB: the suite exists to
// prove the SQL, not a model of it.
type PGIso struct {
	Store    Store
	Sessions *Sessions
	DB       *sql.DB
	t        *testing.T
	pg       *Postgres
	kill     *KillSwitch
	vault    *fakeTransit
	logs     *syncBuffer
	base     string
}

// NewPGIso builds the store on a fresh schema of the Postgres named by
// TOKENSTORE_TEST_PG_DSN. Glovo requests of every client go to glovoURL. It
// skips when no DSN is set, unless TOKENSTORE_TEST_PG_REQUIRED is set (then
// it fails: CI must never pass because the database was unreachable).
func NewPGIso(t *testing.T, glovoURL string, caps Caps, validate func(context.Context, glovoapi.Session) error) *PGIso {
	t.Helper()
	dsn := pgDSN(t)
	if dsn == "" {
		t.Skip(pgDSNEnv + " not set")
	}
	h := &PGIso{t: t, vault: newFakeTransit(t), logs: &syncBuffer{}, base: glovoURL}
	h.DB = openPostgres(t, dsn)
	pg, err := NewPostgres(context.Background(), h.DB)
	if err != nil {
		t.Fatal(err)
	}
	h.pg = pg
	if err := pg.SetKilled(context.Background(), false, "test"); err != nil {
		t.Fatal(err)
	}
	k, err := NewKillSwitch(pg)
	if err != nil {
		t.Fatal(err)
	}
	h.kill = k
	if validate == nil {
		validate = func(context.Context, glovoapi.Session) error { return nil }
	}
	opts := []glovoapi.Option{glovoapi.WithBaseURL(glovoURL)}
	st, err := New(Config{
		DB:            pg,
		Transit:       h.vault.client(t),
		Kill:          k,
		Caps:          caps,
		Logger:        slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		ValidateLink:  validate,
		ClientOptions: opts,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.Store = st
	h.Sessions, err = NewSessions(st, 0, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// SetKilled flips the control row and drops the cached "running" answer, so
// the change is visible to the very next call.
func (h *PGIso) SetKilled(killed bool) {
	h.t.Helper()
	if err := h.pg.SetKilled(context.Background(), killed, "test"); err != nil {
		h.t.Fatal(err)
	}
	h.kill.mu.Lock()
	h.kill.okUntil = time.Time{}
	h.kill.mu.Unlock()
}

// NewSessions returns a second, empty session cache over the same store
// (another replica, or a restart).
func (h *PGIso) NewSessions() *Sessions {
	h.t.Helper()
	s, err := NewSessions(h.Store, 0, glovoapi.WithBaseURL(h.base))
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

// SwapCiphertexts exchanges the ciphertexts of two rows, as an attacker
// with write access to the table would.
func (h *PGIso) SwapCiphertexts(a, b string) {
	h.t.Helper()
	if _, err := h.DB.Exec(`UPDATE glovo_links x SET ciphertext = y.ciphertext FROM glovo_links y
		WHERE (x.pid = $1 AND y.pid = $2) OR (x.pid = $2 AND y.pid = $1)`, a, b); err != nil {
		h.t.Fatal(err)
	}
}

// Stored decrypts pid's row directly, bypassing the store and its caps.
func (h *PGIso) Stored(pid string) glovoapi.Session {
	h.t.Helper()
	r, err := h.pg.ReadLink(context.Background(), pid)
	if err != nil {
		h.t.Fatal(err)
	}
	pt, err := h.vault.open(keyContext(pidFromString(pid), r.LinkID), r.Ciphertext)
	if err != nil {
		h.t.Fatal(err)
	}
	s, err := decodeSession(pt)
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

// LinkID returns pid's current link id ("" when there is no row).
func (h *PGIso) LinkID(pid string) string {
	r, err := h.pg.ReadLink(context.Background(), pid)
	if err != nil {
		return ""
	}
	return r.LinkID
}

// Logs is everything the store logged so far.
func (h *PGIso) Logs() string { return h.logs.String() }

// SetKilledCacheOnly drops the cached "running" answer without touching the
// control row (the row was changed behind the store's back).
func (h *PGIso) SetKilledCacheOnly() {
	h.kill.mu.Lock()
	h.kill.okUntil = time.Time{}
	h.kill.mu.Unlock()
}
