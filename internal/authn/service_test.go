package authn

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"testing"
	"time"
)

const (
	svcIssuer   = "https://idp.test"
	svcAudience = "control-project"
	svcSubject  = "machine-1"
)

func rsaJWK(kid string, pub *rsa.PublicKey) map[string]string {
	return map[string]string{
		"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig",
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

func signRS256(k *rsa.PrivateKey, hdr, claims map[string]any) string {
	in := b64(hdr) + "." + b64(claims)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return in + "." + base64.RawURLEncoding.EncodeToString(sig)
}

type svcFixture struct {
	jwks *fakeJWKS
	rsa  *rsa.PrivateKey
	ed   keypair
	now  time.Time
	v    *ServiceVerifier
}

func newSvcFixture(t *testing.T, subjects ...string) *svcFixture {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &svcFixture{jwks: newFakeJWKS(t), rsa: k, ed: newKey(t, "ed1"), now: time.Unix(1_800_000_000, 0)}
	f.jwks.set(false, rsaJWK("r1", &k.PublicKey), pubJWK(f.ed.kid, f.ed.pub))
	v, err := NewService(ServiceConfig{
		Issuer: svcIssuer, Audience: svcAudience, JWKSURL: f.jwks.srv.URL,
		Subjects: subjects, Now: func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.v = v
	return f
}

func (f *svcFixture) claims() map[string]any {
	return map[string]any{
		"iss": svcIssuer, "sub": svcSubject, "aud": []string{"other", svcAudience},
		"iat": f.now.Unix(), "exp": f.now.Add(time.Hour).Unix(),
	}
}

func (f *svcFixture) rs(claims map[string]any) string {
	return signRS256(f.rsa, map[string]any{"alg": "RS256", "kid": "r1", "typ": "JWT"}, claims)
}

func wantSvcReason(t *testing.T, err error, reason string, status int) {
	t.Helper()
	var ae *Error
	if !errors.As(err, &ae) || ae.Reason != reason || ae.Status != status {
		t.Fatalf("got %v, want %s/%d", err, reason, status)
	}
}

func TestServiceVerifierAcceptsAllowListedSubjectRS256AndEdDSA(t *testing.T) {
	f := newSvcFixture(t, svcSubject)
	got, err := f.v.Verify(context.Background(), f.rs(f.claims()))
	if err != nil || got != svcSubject {
		t.Fatalf("rs256: %q %v", got, err)
	}
	ed := signEdDSA(f.ed, map[string]any{"alg": "EdDSA", "kid": "ed1"}, f.claims())
	if got, err := f.v.Verify(context.Background(), ed); err != nil || got != svcSubject {
		t.Fatalf("eddsa: %q %v", got, err)
	}
}

func TestServiceVerifierEmptyAllowListRefusesEverything(t *testing.T) {
	f := newSvcFixture(t)
	_, err := f.v.Verify(context.Background(), f.rs(f.claims()))
	wantSvcReason(t, err, ReasonSubjectNotAllowed, http.StatusForbidden)
	var nilV *ServiceVerifier
	if _, err := nilV.Verify(context.Background(), "x"); err == nil {
		t.Fatal("nil verifier accepted")
	}
}

func TestServiceVerifierRefusals(t *testing.T) {
	f := newSvcFixture(t, svcSubject)
	ctx := context.Background()
	mut := func(fn func(map[string]any)) string {
		c := f.claims()
		fn(c)
		return f.rs(c)
	}
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	cases := []struct {
		name   string
		token  string
		reason string
		status int
	}{
		{"unknown subject", mut(func(c map[string]any) { c["sub"] = "someone-else" }), ReasonSubjectNotAllowed, 403},
		{"missing subject", mut(func(c map[string]any) { delete(c, "sub") }), ReasonNoPrincipal, 401},
		{"wrong issuer", mut(func(c map[string]any) { c["iss"] = "https://evil" }), ReasonIssuer, 401},
		{"wrong audience", mut(func(c map[string]any) { c["aud"] = []string{"nope"} }), ReasonAudience, 401},
		{"expired", mut(func(c map[string]any) { c["exp"] = f.now.Add(-time.Hour).Unix() }), ReasonExpired, 401},
		{"no exp", mut(func(c map[string]any) { delete(c, "exp") }), ReasonExpired, 401},
		{"future nbf", mut(func(c map[string]any) { c["nbf"] = f.now.Add(time.Hour).Unix() }), ReasonNotYetValid, 401},
		{"alg none", b64(map[string]any{"alg": "none", "kid": "r1"}) + "." + b64(f.claims()) + ".AA", ReasonAlg, 401},
		{"HS256", b64(map[string]any{"alg": "HS256", "kid": "r1"}) + "." + b64(f.claims()) + ".AA", ReasonAlg, 401},
		{"bad signature", signRS256(other, map[string]any{"alg": "RS256", "kid": "r1"}, f.claims()), ReasonSignature, 401},
		{"unknown kid", signRS256(f.rsa, map[string]any{"alg": "RS256", "kid": "zz"}, f.claims()), ReasonUnknownKey, 401},
		{"no kid", signRS256(f.rsa, map[string]any{"alg": "RS256"}, f.claims()), ReasonUnknownKey, 401},
		// Key confusion: an RSA key never verifies as EdDSA, nor the reverse.
		{"rsa key claimed as EdDSA", signEdDSA(keypair{priv: ed25519.NewKeyFromSeed(make([]byte, 32))}, map[string]any{"alg": "EdDSA", "kid": "r1"}, f.claims()), ReasonSignature, 401},
		{"ed key claimed as RS256", signRS256(f.rsa, map[string]any{"alg": "RS256", "kid": "ed1"}, f.claims()), ReasonSignature, 401},
		{"malformed", "not-a-jwt", ReasonMalformed, 401},
		{"empty", "", ReasonMalformed, 401},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := f.v.Verify(ctx, c.token)
			wantSvcReason(t, err, c.reason, c.status)
		})
	}
}

func TestServiceVerifierUnreadableKeySetIsUnavailableNotRefusedAsUnknown(t *testing.T) {
	f := newSvcFixture(t, svcSubject)
	f.jwks.set(true)
	_, err := f.v.Verify(context.Background(), f.rs(f.claims()))
	wantSvcReason(t, err, ReasonJWKSDown, http.StatusServiceUnavailable)
}

func TestMctlAPIVerifierStillRefusesRSAKeys(t *testing.T) {
	// The RSA support is for the service verifier only: the mctl-api
	// verifier must keep its EdDSA-only key set.
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	body := []byte(`{"keys":[` + string(mustJSON(rsaJWK("r1", &k.PublicKey))) + `]}`)
	if _, err := parseJWKS(body, false); err == nil {
		t.Fatal("RSA key accepted by an EdDSA-only set")
	}
	if keys, err := parseJWKS(body, true); err != nil || len(keys) != 1 {
		t.Fatalf("rsa set: %v %v", keys, err)
	}
}

func TestServiceJWKSRefusesSmallRSAKey(t *testing.T) {
	k, _ := rsa.GenerateKey(rand.Reader, 1024) //nolint:gosec // deliberately too small: the test asserts it is refused
	body := []byte(`{"keys":[` + string(mustJSON(rsaJWK("r1", &k.PublicKey))) + `]}`)
	if _, err := parseJWKS(body, true); err == nil {
		t.Fatal("1024-bit RSA key accepted")
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
