package authn

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testIssuer   = "https://api.mctl.ai"
	testResource = "https://glovo.mctl.ai/mcp"
	testPID      = "prn_test_owner"
)

// fakeJWKS serves a mutable key set and counts fetches.
type fakeJWKS struct {
	mu      sync.Mutex
	keys    []map[string]string
	down    bool
	fetches atomic.Int32
	srv     *httptest.Server
}

func newFakeJWKS(t *testing.T) *fakeJWKS {
	t.Helper()
	f := &fakeJWKS{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.fetches.Add(1)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.down {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": f.keys})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeJWKS) set(down bool, keys ...map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
	if keys != nil {
		f.keys = keys
	}
}

func pubJWK(kid string, pub ed25519.PublicKey) map[string]string {
	return map[string]string{
		"kty": "OKP", "crv": "Ed25519", "kid": kid, "alg": "EdDSA", "use": "sig",
		"x": base64.RawURLEncoding.EncodeToString(pub),
	}
}

type keypair struct {
	kid  string
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newKey(t *testing.T, kid string) keypair {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return keypair{kid: kid, pub: pub, priv: priv}
}

func b64(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func signEdDSA(k keypair, hdr, claims map[string]any) string {
	in := b64(hdr) + "." + b64(claims)
	return in + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(k.priv, []byte(in)))
}

func goodClaims(now time.Time) map[string]any {
	// aud is a single string: mctl-api's audience type marshals a
	// one-entry audience as a bare string.
	return map[string]any{
		"iss": testIssuer, "sub": "owner", "aud": testResource, "pid": testPID,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "groups": []string{},
	}
}

type fixture struct {
	jwks *fakeJWKS
	key  keypair
	now  time.Time
	v    *Verifier
}

func newFixture(t *testing.T, allow ...string) *fixture {
	t.Helper()
	f := &fixture{jwks: newFakeJWKS(t), key: newKey(t, "k1"), now: time.Unix(1_800_000_000, 0)}
	f.jwks.set(false, pubJWK(f.key.kid, f.key.pub))
	if allow == nil {
		allow = []string{testPID}
	}
	v, err := New(Config{
		Issuer: testIssuer, Resource: testResource, JWKSURL: f.jwks.srv.URL,
		AllowedPrincipals: allow, Now: func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.v = v
	return f
}

func (f *fixture) token(mut func(h, c map[string]any)) string {
	h := map[string]any{"alg": "EdDSA", "typ": "JWT", "kid": f.key.kid}
	c := goodClaims(f.now)
	if mut != nil {
		mut(h, c)
	}
	return signEdDSA(f.key, h, c)
}

func reasonOf(t *testing.T, err error) (string, int) {
	t.Helper()
	if err == nil {
		return "", 0
	}
	var ae *Error
	if !errors.As(err, &ae) {
		t.Fatalf("error %v is not *Error", err)
	}
	return ae.Reason, ae.Status
}

func TestVerifyTable(t *testing.T) {
	other := newKey(t, "k1") // same kid, different key: a forged signature
	for _, tc := range []struct {
		name   string
		tok    func(f *fixture) string
		reason string
		status int
	}{
		{"good token", func(f *fixture) string { return f.token(nil) }, "", 0},
		{"good token, aud one-element array", func(f *fixture) string {
			return f.token(func(_, c map[string]any) { c["aud"] = []string{testResource} })
		}, "", 0},
		{"bad signature", func(f *fixture) string {
			return signEdDSA(other, map[string]any{"alg": "EdDSA", "kid": "k1"}, goodClaims(f.now))
		}, ReasonSignature, 401},
		{"tampered payload", func(f *fixture) string {
			tok := f.token(nil)
			c := goodClaims(f.now)
			c["pid"] = "prn_someone_else"
			parts := splitTok(tok)
			return parts[0] + "." + b64(c) + "." + parts[2]
		}, ReasonSignature, 401},
		{"wrong iss", func(f *fixture) string {
			return f.token(func(_, c map[string]any) { c["iss"] = "https://evil.example" })
		}, ReasonIssuer, 401},
		{"iss trailing slash", func(f *fixture) string {
			return f.token(func(_, c map[string]any) { c["iss"] = testIssuer + "/" })
		}, ReasonIssuer, 401},
		{"wrong aud", func(f *fixture) string {
			return f.token(func(_, c map[string]any) { c["aud"] = "https://api.mctl.ai" })
		}, ReasonAudience, 401},
		{"aud with extra entry", func(f *fixture) string {
			return f.token(func(_, c map[string]any) { c["aud"] = []string{testResource, "https://api.mctl.ai"} })
		}, ReasonAudience, 401},
		{"aud repeated", func(f *fixture) string {
			return f.token(func(_, c map[string]any) { c["aud"] = []string{testResource, testResource} })
		}, ReasonAudience, 401},
		{"aud missing", func(f *fixture) string {
			return f.token(func(_, c map[string]any) { delete(c, "aud") })
		}, ReasonAudience, 401},
		{"expired", func(f *fixture) string {
			return f.token(func(_, c map[string]any) { c["exp"] = f.now.Add(-time.Minute).Unix() })
		}, ReasonExpired, 401},
		{"expired within leeway is accepted", func(f *fixture) string {
			return f.token(func(_, c map[string]any) { c["exp"] = f.now.Add(-10 * time.Second).Unix() })
		}, "", 0},
		{"exp missing", func(f *fixture) string {
			return f.token(func(_, c map[string]any) { delete(c, "exp") })
		}, ReasonExpired, 401},
		{"nbf in the future", func(f *fixture) string {
			return f.token(func(_, c map[string]any) { c["nbf"] = f.now.Add(5 * time.Minute).Unix() })
		}, ReasonNotYetValid, 401},
		{"iat in the future", func(f *fixture) string {
			return f.token(func(_, c map[string]any) { c["iat"] = f.now.Add(5 * time.Minute).Unix() })
		}, ReasonNotYetValid, 401},
		{"alg none", func(f *fixture) string {
			return b64(map[string]any{"alg": "none", "kid": "k1"}) + "." + b64(goodClaims(f.now)) + "."
		}, ReasonMalformed, 401},
		{"alg none with junk signature", func(f *fixture) string {
			return b64(map[string]any{"alg": "none", "kid": "k1"}) + "." + b64(goodClaims(f.now)) + ".AAAA"
		}, ReasonAlg, 401},
		{"alg HS256 keyed with the public key", func(f *fixture) string {
			in := b64(map[string]any{"alg": "HS256", "kid": "k1"}) + "." + b64(goodClaims(f.now))
			m := hmac.New(sha256.New, f.key.pub)
			m.Write([]byte(in))
			return in + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
		}, ReasonAlg, 401},
		{"crit header", func(f *fixture) string {
			return f.token(func(h, _ map[string]any) { h["crit"] = []string{"exp"} })
		}, ReasonMalformed, 401},
		{"no kid", func(f *fixture) string {
			return f.token(func(h, _ map[string]any) { delete(h, "kid") })
		}, ReasonUnknownKey, 401},
		{"pid missing", func(f *fixture) string {
			return f.token(func(_, c map[string]any) { delete(c, "pid") })
		}, ReasonNoPrincipal, 401},
		{"principal not allowlisted", func(f *fixture) string {
			return f.token(func(_, c map[string]any) { c["pid"] = "prn_stranger" })
		}, ReasonNotAllowed, 403},
		{"garbage", func(*fixture) string { return "not-a-jwt" }, ReasonMalformed, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			p, err := f.v.Verify(context.Background(), tc.tok(f))
			reason, status := reasonOf(t, err)
			if reason != tc.reason || status != tc.status {
				t.Fatalf("got (%q, %d), want (%q, %d)", reason, status, tc.reason, tc.status)
			}
			if tc.reason == "" && (p == nil || p.ID != testPID) {
				t.Fatalf("principal = %+v", p)
			}
			if tc.reason != "" && p != nil {
				t.Fatalf("principal returned with an error: %+v", p)
			}
		})
	}
}

func splitTok(tok string) [3]string {
	var out [3]string
	i := 0
	start := 0
	for j := 0; j < len(tok); j++ {
		if tok[j] == '.' {
			out[i] = tok[start:j]
			i++
			start = j + 1
		}
	}
	out[i] = tok[start:]
	return out
}

func TestEmptyAllowlistRefusesEveryone(t *testing.T) {
	for _, allow := range [][]string{{}, {""}, {" ", ""}} {
		f := newFixture(t, allow...)
		_, err := f.v.Verify(context.Background(), f.token(nil))
		if reason, status := reasonOf(t, err); reason != ReasonNotAllowed || status != 403 {
			t.Fatalf("allow %q: got (%q, %d)", allow, reason, status)
		}
	}
}

func TestUnknownKidRefetchesOnce(t *testing.T) {
	f := newFixture(t)
	if _, err := f.v.Verify(context.Background(), f.token(nil)); err != nil {
		t.Fatal(err)
	}
	if n := f.jwks.fetches.Load(); n != 1 {
		t.Fatalf("fetches = %d, want 1", n)
	}

	// Rotation: mctl-api publishes k2 and signs with it. The cached set
	// lacks k2, so the verifier refetches once and accepts.
	k2 := newKey(t, "k2")
	f.jwks.set(false, pubJWK(f.key.kid, f.key.pub), pubJWK(k2.kid, k2.pub))
	f.now = f.now.Add(time.Minute) // past jwksMinRefetch
	tok := signEdDSA(k2, map[string]any{"alg": "EdDSA", "kid": "k2"}, goodClaims(f.now))
	if _, err := f.v.Verify(context.Background(), tok); err != nil {
		t.Fatalf("rotated key: %v", err)
	}
	if n := f.jwks.fetches.Load(); n != 2 {
		t.Fatalf("fetches = %d, want 2", n)
	}

	// A kid that is still unknown after a successful refetch is a 401, and
	// further unknown kids within jwksMinRefetch do not fetch again.
	f.now = f.now.Add(time.Minute)
	k3 := newKey(t, "k3")
	for i := 0; i < 5; i++ {
		tok := signEdDSA(k3, map[string]any{"alg": "EdDSA", "kid": "k3"}, goodClaims(f.now))
		_, err := f.v.Verify(context.Background(), tok)
		if reason, status := reasonOf(t, err); reason != ReasonUnknownKey || status != 401 {
			t.Fatalf("unknown kid: got (%q, %d)", reason, status)
		}
	}
	if n := f.jwks.fetches.Load(); n != 3 {
		t.Fatalf("fetches = %d, want 3 (one refetch for the unknown kid)", n)
	}
}

func TestJWKSDownIs503(t *testing.T) {
	t.Run("never fetched", func(t *testing.T) {
		f := newFixture(t)
		f.jwks.set(true)
		_, err := f.v.Verify(context.Background(), f.token(nil))
		if reason, status := reasonOf(t, err); reason != ReasonJWKSDown || status != 503 {
			t.Fatalf("got (%q, %d)", reason, status)
		}
		if f.v.Ready(context.Background()) == nil {
			t.Fatal("ready with no key set")
		}
	})
	t.Run("malformed body is a failed read", func(t *testing.T) {
		f := newFixture(t)
		f.jwks.set(false, map[string]string{"kty": "RSA", "kid": "k1"})
		_, err := f.v.Verify(context.Background(), f.token(nil))
		if reason, status := reasonOf(t, err); reason != ReasonJWKSDown || status != 503 {
			t.Fatalf("got (%q, %d)", reason, status)
		}
	})
	t.Run("unknown kid while down", func(t *testing.T) {
		f := newFixture(t)
		if _, err := f.v.Verify(context.Background(), f.token(nil)); err != nil {
			t.Fatal(err)
		}
		f.jwks.set(true)
		f.now = f.now.Add(time.Minute)
		k2 := newKey(t, "k2")
		tok := signEdDSA(k2, map[string]any{"alg": "EdDSA", "kid": "k2"}, goodClaims(f.now))
		_, err := f.v.Verify(context.Background(), tok)
		if reason, status := reasonOf(t, err); reason != ReasonJWKSDown || status != 503 {
			t.Fatalf("got (%q, %d), want jwks_unavailable 503, never unknown_kid", reason, status)
		}
	})
	t.Run("cached key survives a short outage, not a long one", func(t *testing.T) {
		f := newFixture(t)
		if _, err := f.v.Verify(context.Background(), f.token(nil)); err != nil {
			t.Fatal(err)
		}
		f.jwks.set(true)
		f.now = f.now.Add(time.Hour) // stale, refresh fails, cache still usable
		if _, err := f.v.Verify(context.Background(), f.token(nil)); err != nil {
			t.Fatalf("short outage: %v", err)
		}
		f.now = f.now.Add(25 * time.Hour) // past jwksMaxStale
		_, err := f.v.Verify(context.Background(), f.token(nil))
		if reason, status := reasonOf(t, err); reason != ReasonJWKSDown || status != 503 {
			t.Fatalf("long outage: got (%q, %d)", reason, status)
		}
	})
}

func TestReady(t *testing.T) {
	f := newFixture(t)
	if err := f.v.Ready(context.Background()); err != nil {
		t.Fatalf("ready: %v", err)
	}
}

func TestPrivateKeyInJWKSIgnored(t *testing.T) {
	f := newFixture(t)
	j := pubJWK(f.key.kid, f.key.pub)
	j["d"] = "AAAA"
	f.jwks.set(false, j)
	_, err := f.v.Verify(context.Background(), f.token(nil))
	if reason, _ := reasonOf(t, err); reason != ReasonJWKSDown {
		t.Fatalf("got %q", reason)
	}
}
