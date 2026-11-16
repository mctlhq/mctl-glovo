package authn

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// ReasonSubjectNotAllowed: a valid token of a service subject that is not on
// the control allow-list.
const ReasonSubjectNotAllowed = "subject_not_allowed"

// ServiceConfig configures a ServiceVerifier: the check of a machine-user
// token (ZITADEL) presented to the cluster-internal control endpoint. It is
// the Verifier's checks (pinned algorithm, kid, signature against a fetched
// key set, exact issuer, audience, exp/nbf/iat) with a different issuer and
// key set, and an allow-list of subjects in place of the pid and grant.
type ServiceConfig struct {
	// Issuer is the exact expected iss.
	Issuer string
	// Audience must be one of the aud entries (a machine-user token
	// usually names several: its project and client).
	Audience string
	JWKSURL  string
	// Subjects is the allow-list of sub values. Empty refuses everyone:
	// there is no "any valid token" mode.
	Subjects   []string
	HTTPClient *http.Client
	Now        func() time.Time
}

// ServiceVerifier verifies a machine-user bearer token against an explicit
// subject allow-list.
type ServiceVerifier struct {
	issuer   string
	audience string
	subjects map[string]struct{}
	jwks     *jwksCache
	now      func() time.Time
}

// NewService builds a ServiceVerifier. It makes no network call.
func NewService(cfg ServiceConfig) (*ServiceVerifier, error) {
	if cfg.Issuer == "" || cfg.Audience == "" || cfg.JWKSURL == "" {
		return nil, errors.New("authn: service issuer, audience and jwks url are required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	subs := map[string]struct{}{}
	for _, s := range cfg.Subjects {
		if s = strings.TrimSpace(s); s != "" {
			subs[s] = struct{}{}
		}
	}
	return &ServiceVerifier{
		issuer:   cfg.Issuer,
		audience: cfg.Audience,
		subjects: subs,
		jwks:     newJWKSCache(cfg.JWKSURL, cfg.HTTPClient, now, true),
		now:      now,
	}, nil
}

// Refresh fetches the key set now (startup).
func (v *ServiceVerifier) Refresh(ctx context.Context) error { return v.jwks.fetch(ctx) }

// Verify checks token and returns the caller's subject. An empty allow-list
// refuses before any key work. A returned error is always an *Error.
func (v *ServiceVerifier) Verify(ctx context.Context, token string) (string, error) {
	if v == nil || len(v.subjects) == 0 {
		return "", refuse(ReasonSubjectNotAllowed)
	}
	if len(token) > maxTokenBytes {
		return "", refuse(ReasonTokenTooLong)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", refuse(ReasonMalformed)
	}
	hdrJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", refuse(ReasonMalformed)
	}
	var hdr header
	if err := json.Unmarshal(hdrJSON, &hdr); err != nil {
		return "", refuse(ReasonMalformed)
	}
	if hdr.Alg != AlgEdDSA && hdr.Alg != AlgRS256 {
		return "", refuse(ReasonAlg)
	}
	if len(hdr.Crit) > 0 {
		return "", refuse(ReasonMalformed)
	}
	if hdr.Kid == "" {
		return "", refuse(ReasonUnknownKey)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) == 0 {
		return "", refuse(ReasonSignature)
	}
	key, ok, err := v.jwks.key(ctx, hdr.Kid)
	if err != nil {
		return "", refuse(ReasonJWKSDown)
	}
	if !ok {
		return "", refuse(ReasonUnknownKey)
	}
	signed := []byte(parts[0] + "." + parts[1])
	// The key type must match the header's alg: an alg never selects a
	// verification method the key was not published for.
	switch k := key.(type) {
	case ed25519.PublicKey:
		if hdr.Alg != AlgEdDSA || len(sig) != ed25519.SignatureSize || !ed25519.Verify(k, signed, sig) {
			return "", refuse(ReasonSignature)
		}
	case *rsa.PublicKey:
		sum := sha256.Sum256(signed)
		if hdr.Alg != AlgRS256 || rsa.VerifyPKCS1v15(k, crypto.SHA256, sum[:], sig) != nil {
			return "", refuse(ReasonSignature)
		}
	default:
		return "", refuse(ReasonSignature)
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", refuse(ReasonMalformed)
	}
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.UseNumber()
	var c claims
	if err := dec.Decode(&c); err != nil {
		return "", refuse(ReasonMalformed)
	}
	if c.Iss != v.issuer {
		return "", refuse(ReasonIssuer)
	}
	if !audienceContains(c.Aud, v.audience) {
		return "", refuse(ReasonAudience)
	}
	now := v.now()
	exp, ok := unixTime(c.Exp)
	if !ok || !now.Before(exp.Add(Leeway)) {
		return "", refuse(ReasonExpired)
	}
	if c.Nbf != nil {
		if nbf, ok := unixTime(c.Nbf); !ok || nbf.After(now.Add(Leeway)) {
			return "", refuse(ReasonNotYetValid)
		}
	}
	if c.Iat != nil {
		if iat, ok := unixTime(c.Iat); !ok || iat.After(now.Add(Leeway)) {
			return "", refuse(ReasonNotYetValid)
		}
	}
	if c.Sub == "" {
		return "", refuse(ReasonNoPrincipal)
	}
	if _, ok := v.subjects[c.Sub]; !ok {
		return "", refuse(ReasonSubjectNotAllowed)
	}
	return c.Sub, nil
}

// audienceContains reports whether aud (a string or an array of strings)
// names want.
func audienceContains(raw json.RawMessage, want string) bool {
	if len(raw) == 0 {
		return false
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return one == want
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return false
	}
	for _, a := range many {
		if a == want {
			return true
		}
	}
	return false
}
