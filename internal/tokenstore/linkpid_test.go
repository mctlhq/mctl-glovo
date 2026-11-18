package tokenstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

// A link code belongs to the principal it was issued to. Anyone else who
// holds it (a leaked code, an attacker's code handed to a victim, a code
// pasted into the wrong session) is refused with the answer an unknown code
// gets, and the code is NOT burned: the owner can still use it.
func TestLinkCodeIsBoundToItsPrincipal(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		ctx := context.Background()
		a, b := h.pid(pidA), h.pid(pidB)
		code, _, err := h.st.CreateLinkCode(ctx, a, "consent-v1")
		if err != nil {
			t.Fatal(err)
		}
		if masked, err := h.st.PreviewLink(ctx, b, code); !errors.Is(err, ErrCodeRefused) || masked != "" {
			t.Fatalf("a stranger's preview = %q, %v; want ErrCodeRefused and no identity", masked, err)
		}
		if err := h.st.Link(ctx, b, code, testSession("B")); !errors.Is(err, ErrCodeRefused) {
			t.Fatalf("a stranger's upload = %v, want ErrCodeRefused", err)
		}
		if h.raw != nil {
			if _, err := h.raw.ReadLink(ctx, pidA); err == nil {
				t.Fatal("a link exists for the owner after a stranger's attempt")
			}
			if _, err := h.raw.ReadLink(ctx, pidB); err == nil {
				t.Fatal("a link exists for the stranger")
			}
		}
		// Not burned: the owner still previews and links with it.
		if _, err := h.st.PreviewLink(ctx, a, code); err != nil {
			t.Fatalf("the owner's preview after a stranger's attempt: %v", err)
		}
		if err := h.st.Link(ctx, a, code, testSession("A")); err != nil {
			t.Fatalf("the owner's upload after a stranger's attempt: %v", err)
		}
		// And a replay by the owner is refused.
		if err := h.st.Link(ctx, a, code, testSession("A")); !errors.Is(err, ErrCodeRefused) {
			t.Fatalf("replay = %v, want ErrCodeRefused", err)
		}
	})
}

func TestLinkRefusesAZeroCaller(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		ctx := context.Background()
		code, _, _ := h.st.CreateLinkCode(ctx, h.pid(pidA), "consent-v1")
		if _, err := h.st.PreviewLink(ctx, PID{}, code); !errors.Is(err, ErrInvalidPID) {
			t.Fatalf("preview with no caller = %v", err)
		}
		if err := h.st.Link(ctx, PID{}, code, testSession("A")); !errors.Is(err, ErrInvalidPID) {
			t.Fatalf("link with no caller = %v", err)
		}
		if err := h.st.Link(ctx, h.pid(pidA), code, testSession("A")); err != nil {
			t.Fatalf("the code must survive a refused zero caller: %v", err)
		}
	})
}

// unboundMem is a DB whose code queries ignore the caller, as if the pid
// condition were dropped from the SQL. The store must still never show or
// store anything for a stranger: its own check is the second line.
type unboundMem struct{ *memDB }

func (u unboundMem) PeekLinkCode(_ context.Context, hash []byte, _ string) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	c := u.live(hash)
	if c == nil {
		return "", errNoRow
	}
	return c.pid, nil
}

func (u unboundMem) RedeemLinkCode(_ context.Context, hash []byte, _ string) (string, string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	c := u.live(hash)
	if c == nil {
		return "", "", errNoRow
	}
	c.used = time.Now()
	return c.pid, c.consent, nil
}

func TestStoreRefusesAStrangerEvenIfTheDatabaseDoesNot(t *testing.T) {
	m := newMemDB()
	h := newHarness(t, unboundMem{m}, m, nil, Caps{})
	ctx := context.Background()
	a, b := h.pid(pidA), h.pid(pidB)
	code, _, _ := h.st.CreateLinkCode(ctx, a, "consent-v1")
	if masked, err := h.st.PreviewLink(ctx, b, code); !errors.Is(err, ErrCodeRefused) || masked != "" {
		t.Fatalf("preview = %q, %v", masked, err)
	}
	if err := h.st.Link(ctx, b, code, testSession("B")); !errors.Is(err, ErrCodeRefused) {
		t.Fatalf("link = %v", err)
	}
	for _, pid := range []string{pidA, pidB} {
		if _, err := m.ReadLink(ctx, pid); err == nil {
			t.Fatalf("a link was stored for %s by a stranger's upload", pid)
		}
	}
	if strings.Contains(h.logs.String(), "access-B") {
		t.Fatal("session material in the logs")
	}
}

// An expired code is refused by both routes on both backends, whoever holds
// it, and a code past its expiry is not usable by its owner either.
func TestExpiredLinkCodeIsRefusedOnEveryBackend(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		ctx := context.Background()
		raw := bytes.Repeat([]byte{7}, 32)
		code := base64.RawURLEncoding.EncodeToString(raw)
		sum := sha256.Sum256(raw)
		if err := h.raw.InsertLinkCode(ctx, sum[:], pidA, "consent-v1", time.Now().Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		a := h.pid(pidA)
		if _, err := h.st.PreviewLink(ctx, a, code); !errors.Is(err, ErrCodeRefused) {
			t.Fatalf("expired preview = %v", err)
		}
		if err := h.st.Link(ctx, a, code, testSession("A")); !errors.Is(err, ErrCodeRefused) {
			t.Fatalf("expired upload = %v", err)
		}
	})
}
