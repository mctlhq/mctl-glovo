package authn

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// fakeAdmitter admits the principals in yes; err overrides.
type fakeAdmitter struct {
	yes   map[string]bool
	err   error
	calls int
}

func (a *fakeAdmitter) Admit(_ context.Context, pid string) error {
	a.calls++
	if a.err != nil {
		return a.err
	}
	if !a.yes[pid] {
		return fmt.Errorf("%w: no consent", ErrNotAdmitted)
	}
	return nil
}

// newGated is a fixture whose verifier has an Admitter and RequireGrant.
func newGated(t *testing.T, a *fakeAdmitter, allow ...string) *fixture {
	t.Helper()
	f := newFixture(t, testPID)
	v, err := New(Config{
		Issuer: testIssuer, Resource: testResource, JWKSURL: f.jwks.srv.URL,
		AllowedPrincipals: allow, Admitter: a, RequireGrant: true, Now: func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.v = v
	return f
}

func personal(_, c map[string]any) { c["tgr"], c["tgk"] = "tgr_p", GrantKindPersonal }

func TestRequireGrantShapes(t *testing.T) {
	a := &fakeAdmitter{yes: map[string]bool{testPID: true}}
	f := newGated(t, a)
	p, err := f.v.Verify(context.Background(), f.token(personal))
	if err != nil || !p.Personal || p.GrantID != "tgr_p" || p.Tenant != "" || p.ID != testPID {
		t.Fatalf("personal: %+v %v", p, err)
	}
	p, err = f.v.Verify(context.Background(), f.token(func(_, c map[string]any) { c["tgr"], c["stn"] = "tgr_t", "alpha" }))
	if err != nil || p.Personal || p.GrantID != "tgr_t" || p.Tenant != "alpha" {
		t.Fatalf("team: %+v %v", p, err)
	}
	for name, mut := range map[string]func(h, c map[string]any){
		"no grant":               nil,
		"personal naming a team": func(_, c map[string]any) { c["tgr"], c["tgk"], c["stn"] = "tgr_p", GrantKindPersonal, "alpha" },
		"team without a team":    func(_, c map[string]any) { c["tgr"] = "tgr_t" },
		"unknown kind":           func(_, c map[string]any) { c["tgr"], c["tgk"], c["stn"] = "tgr_t", "team", "alpha" },
		"kind without grant":     func(_, c map[string]any) { c["tgk"] = GrantKindPersonal },
	} {
		_, err := f.v.Verify(context.Background(), f.token(mut))
		if reason, status := reasonOf(t, err); reason != ReasonGrant || status != 401 {
			t.Errorf("%s: (%q, %d)", name, reason, status)
		}
	}
	// Off, the grant claims are not read: today's behaviour.
	plain := newFixture(t)
	if p, err := plain.v.Verify(context.Background(), plain.token(nil)); err != nil || p.GrantID != "" || p.Personal {
		t.Fatalf("RequireGrant off: %+v %v", p, err)
	}
}

func TestAdmitter(t *testing.T) {
	ctx := context.Background()
	t.Run("consent, no allowlist: admitted", func(t *testing.T) {
		f := newGated(t, &fakeAdmitter{yes: map[string]bool{testPID: true}})
		if _, err := f.v.Verify(ctx, f.token(personal)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("no consent: 403 not_admitted", func(t *testing.T) {
		f := newGated(t, &fakeAdmitter{})
		_, err := f.v.Verify(ctx, f.token(personal))
		if reason, status := reasonOf(t, err); reason != ReasonNotAdmitted || status != 403 {
			t.Fatalf("(%q, %d)", reason, status)
		}
	})
	t.Run("admitter error: 503, never a pass", func(t *testing.T) {
		f := newGated(t, &fakeAdmitter{yes: map[string]bool{testPID: true}, err: errors.New("mctl-api down")})
		_, err := f.v.Verify(ctx, f.token(personal))
		if reason, status := reasonOf(t, err); reason != ReasonAdmitDown || status != 503 {
			t.Fatalf("(%q, %d)", reason, status)
		}
	})
	t.Run("consent but not on a set allowlist: 403", func(t *testing.T) {
		a := &fakeAdmitter{yes: map[string]bool{testPID: true}}
		f := newGated(t, a, "prn_someone_else")
		_, err := f.v.Verify(ctx, f.token(personal))
		if reason, status := reasonOf(t, err); reason != ReasonNotAllowed || status != 403 {
			t.Fatalf("(%q, %d)", reason, status)
		}
		if a.calls != 0 {
			t.Fatal("admitter read for a principal off the allowlist")
		}
	})
	t.Run("on a set allowlist still needs consent", func(t *testing.T) {
		f := newGated(t, &fakeAdmitter{}, testPID)
		_, err := f.v.Verify(ctx, f.token(personal))
		if reason, _ := reasonOf(t, err); reason != ReasonNotAdmitted {
			t.Fatalf("%q", reason)
		}
	})
	t.Run("no admitter and no allowlist: refused", func(t *testing.T) {
		f := newFixture(t, []string{}...)
		_, err := f.v.Verify(ctx, f.token(personal))
		if reason, _ := reasonOf(t, err); reason != ReasonNotAllowed {
			t.Fatalf("%q", reason)
		}
	})
}
