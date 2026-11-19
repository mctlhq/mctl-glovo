package tokenstore

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/mctlhq/mctl-glovo/internal/glovoapi"
)

// LinkHarness gives the external link-flow tests (linkflow_test.go, package
// tokenstore_test) a real store over this package's fakes: memDB, which
// models the SQL in postgres.go, and the fake Vault transit.
type LinkHarness struct {
	Store Store
	// LinkConsent, when set, is the store's upload consent re-check.
	LinkConsent func(context.Context, PID) error
	t           *testing.T
	mem         *memDB
	kill        *KillSwitch
	vault       *fakeTransit
	logs        *syncBuffer
}

// NewLinkHarness builds the store. validate is the validating Glovo call;
// mask answers PreviewLink's masked identity.
func NewLinkHarness(t *testing.T, caps Caps, validate func(context.Context, glovoapi.Session) error, mask func(context.Context, string) (string, error)) *LinkHarness {
	t.Helper()
	h := &LinkHarness{t: t, mem: newMemDB(), vault: newFakeTransit(t), logs: &syncBuffer{}}
	if err := h.mem.SetKilled(context.Background(), false, "test"); err != nil {
		t.Fatal(err)
	}
	k, err := NewKillSwitch(h.mem)
	if err != nil {
		t.Fatal(err)
	}
	h.kill = k
	st, err := New(Config{
		DB:           h.mem,
		Transit:      h.vault.client(t),
		Kill:         k,
		Caps:         caps,
		Logger:       slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		ValidateLink: validate,
		MaskIdentity: mask,
		LinkConsent: func(ctx context.Context, pid PID) error {
			if h.LinkConsent == nil {
				return nil
			}
			return h.LinkConsent(ctx, pid)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.Store = st
	return h
}

// SetKilled flips the control row and drops the cached "running" answer.
func (h *LinkHarness) SetKilled(killed bool) {
	h.mem.mu.Lock()
	h.mem.killed = killed
	h.mem.mu.Unlock()
	h.kill.mu.Lock()
	h.kill.okUntil = time.Time{}
	h.kill.mu.Unlock()
}

// ExpireCodes moves every code's expiry into the past.
func (h *LinkHarness) ExpireCodes() {
	h.mem.mu.Lock()
	defer h.mem.mu.Unlock()
	for _, c := range h.mem.codes {
		c.expires = time.Now().Add(-time.Second)
	}
}

// Codes counts every code row, used or not.
func (h *LinkHarness) Codes() int {
	h.mem.mu.Lock()
	defer h.mem.mu.Unlock()
	return len(h.mem.codes)
}

// LinkRow returns pid's link row; ok is false when there is none.
func (h *LinkHarness) LinkRow(pid string) (row LinkRow, consent string, ok bool) {
	h.mem.mu.Lock()
	defer h.mem.mu.Unlock()
	r, ok := h.mem.links[pid]
	if !ok {
		return LinkRow{}, "", false
	}
	return r.LinkRow, r.consent, true
}

// Stored decrypts pid's row directly.
func (h *LinkHarness) Stored(pid string) glovoapi.Session {
	h.t.Helper()
	r, _, ok := h.LinkRow(pid)
	if !ok {
		h.t.Fatalf("no link for %s", pid)
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

// VaultCalls counts every transit request.
func (h *LinkHarness) VaultCalls() int64 { return h.vault.vaultCalls() }

// Logs is everything the store logged.
func (h *LinkHarness) Logs() string { return h.logs.String() }

// AuditEvents lists the audit rows' event/reason pairs.
func (h *LinkHarness) AuditEvents() []string {
	h.mem.mu.Lock()
	defer h.mem.mu.Unlock()
	var out []string
	for i := range h.mem.audit {
		out = append(out, h.mem.audit[i].Event+"/"+h.mem.audit[i].Reason)
	}
	return out
}

// AuditActors lists the actor of every audit row of one event.
func (h *LinkHarness) AuditActors(event string) []string {
	h.mem.mu.Lock()
	defer h.mem.mu.Unlock()
	var out []string
	for i := range h.mem.audit {
		if h.mem.audit[i].Event == event {
			out = append(out, h.mem.audit[i].Actor)
		}
	}
	return out
}
