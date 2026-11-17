package authn

import (
	"context"
	"testing"
	"time"
)

const testLinkResource = "https://glovo.mctl.ai/link"

// linkFixture is a verifier for the link audience that requires a login no
// older than 5 minutes, signed by the same key set as the MCP fixture.
func linkFixture(t *testing.T, maxAge time.Duration) *fixture {
	t.Helper()
	f := newFixture(t)
	v, err := New(Config{
		Issuer: testIssuer, Resource: testLinkResource, JWKSURL: f.jwks.srv.URL,
		AllowedPrincipals: []string{testPID}, MaxAuthAge: maxAge, Now: func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.v = v
	return f
}

func (f *fixture) linkToken(mut func(c map[string]any)) string {
	return f.token(func(_, c map[string]any) {
		c["aud"] = testLinkResource
		c["auth_time"] = f.now.Add(-time.Minute).Unix()
		if mut != nil {
			mut(c)
		}
	})
}

func TestFreshLoginTable(t *testing.T) {
	const max = 5 * time.Minute
	for _, tc := range []struct {
		name   string
		mut    func(f *fixture, c map[string]any)
		reason string
		status int
	}{
		{"fresh", nil, "", 0},
		{"exactly the max age is accepted", func(f *fixture, c map[string]any) { c["auth_time"] = f.now.Add(-max).Unix() }, "", 0},
		{"one second past the max age", func(f *fixture, c map[string]any) { c["auth_time"] = f.now.Add(-max - time.Second).Unix() }, ReasonFreshLogin, 401},
		{"a day old", func(f *fixture, c map[string]any) { c["auth_time"] = f.now.Add(-24 * time.Hour).Unix() }, ReasonFreshLogin, 401},
		{"auth_time missing (token of another origin)", func(_ *fixture, c map[string]any) { delete(c, "auth_time") }, ReasonFreshLogin, 401},
		{"auth_time zero", func(_ *fixture, c map[string]any) { c["auth_time"] = 0 }, ReasonFreshLogin, 401},
		{"auth_time negative", func(_ *fixture, c map[string]any) { c["auth_time"] = -5 }, ReasonFreshLogin, 401},
		{"auth_time a string", func(f *fixture, c map[string]any) { c["auth_time"] = "1799999999" }, ReasonFreshLogin, 401},
		{"auth_time null", func(_ *fixture, c map[string]any) { c["auth_time"] = nil }, ReasonFreshLogin, 401},
		{"auth_time fractional", func(f *fixture, c map[string]any) { c["auth_time"] = float64(f.now.Unix()) - 10.5 }, ReasonFreshLogin, 401},
		{"auth_time within leeway of the future", func(f *fixture, c map[string]any) { c["auth_time"] = f.now.Add(Leeway).Unix() }, "", 0},
		{"auth_time beyond leeway in the future", func(f *fixture, c map[string]any) { c["auth_time"] = f.now.Add(Leeway + time.Second).Unix() }, ReasonFreshLogin, 401},
		{"a fresh auth_time does not rescue a wrong audience", func(_ *fixture, c map[string]any) { c["aud"] = testResource }, ReasonAudience, 401},
		{"a fresh auth_time does not rescue a missing pid", func(_ *fixture, c map[string]any) { delete(c, "pid") }, ReasonNoPrincipal, 401},
		{"principal not on the allowlist", func(_ *fixture, c map[string]any) { c["pid"] = "prn_stranger" }, ReasonNotAllowed, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := linkFixture(t, max)
			tok := f.linkToken(func(c map[string]any) {
				if tc.mut != nil {
					tc.mut(f, c)
				}
			})
			p, err := f.v.Verify(context.Background(), tok)
			reason, status := reasonOf(t, err)
			if reason != tc.reason || status != tc.status {
				t.Fatalf("got %q/%d, want %q/%d", reason, status, tc.reason, tc.status)
			}
			if tc.reason == "" && (p == nil || p.ID != testPID) {
				t.Fatalf("principal %+v", p)
			}
		})
	}
}

// An MCP-audience token, even with a perfect auth_time, never opens the link
// routes; a link-audience token never opens the MCP data plane.
func TestLinkAndMCPAudiencesAreDisjoint(t *testing.T) {
	f := linkFixture(t, 5*time.Minute)
	mcp, err := New(Config{Issuer: testIssuer, Resource: testResource, JWKSURL: f.jwks.srv.URL,
		AllowedPrincipals: []string{testPID}, Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	mcpTok := f.token(func(_, c map[string]any) { c["auth_time"] = f.now.Unix() })
	if _, err := f.v.Verify(context.Background(), mcpTok); err == nil {
		t.Fatal("an MCP-audience token with a fresh auth_time was accepted by the link verifier")
	} else if r, _ := reasonOf(t, err); r != ReasonAudience {
		t.Fatalf("reason %q, want %q", r, ReasonAudience)
	}
	if _, err := mcp.Verify(context.Background(), f.linkToken(nil)); err == nil {
		t.Fatal("a link-audience token was accepted by the MCP verifier")
	} else if r, _ := reasonOf(t, err); r != ReasonAudience {
		t.Fatalf("reason %q, want %q", r, ReasonAudience)
	}
}

// Without MaxAuthAge auth_time is not read: the MCP verifier neither needs it
// nor trusts it.
func TestAuthTimeIsIgnoredWithoutAMaxAge(t *testing.T) {
	f := newFixture(t)
	for _, at := range []any{nil, "garbage", -1} {
		tok := f.token(func(_, c map[string]any) { c["auth_time"] = at })
		if _, err := f.v.Verify(context.Background(), tok); err != nil {
			t.Fatalf("auth_time %v: %v", at, err)
		}
	}
}

func TestMaxAuthAgeIsBounded(t *testing.T) {
	f := newFixture(t)
	for _, d := range []time.Duration{-time.Second, MaxFreshLoginAge + time.Nanosecond, time.Hour} {
		if _, err := New(Config{Issuer: testIssuer, Resource: testLinkResource, JWKSURL: f.jwks.srv.URL,
			AllowedPrincipals: []string{testPID}, MaxAuthAge: d}); err == nil {
			t.Errorf("max auth age %s accepted", d)
		}
	}
	if _, err := New(Config{Issuer: testIssuer, Resource: testLinkResource, JWKSURL: f.jwks.srv.URL,
		AllowedPrincipals: []string{testPID}, MaxAuthAge: MaxFreshLoginAge}); err != nil {
		t.Fatalf("the cap itself must be accepted: %v", err)
	}
}
