// Package authn verifies mctl-api access tokens for the Glovo home gateway.
//
// The gateway is a resource server only: it holds no signing or shared
// secret and verifies asymmetrically (EdDSA) against mctl-api's JWKS. A token
// is accepted only when every check holds: signature, issuer, an audience
// that is exactly the Glovo resource, exp/nbf/iat, and a principal (the pid
// claim) that is admitted: on an explicit allowlist, or, when an Admitter is
// configured, admitted by it (and on the allowlist too, if one is set).
// Without an Admitter an empty allowlist admits nobody.
package authn

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// AlgEdDSA is the only accepted JWS algorithm.
const AlgEdDSA = "EdDSA"

// AlgRS256 is accepted by the ServiceVerifier only (ZITADEL signs with it).
const AlgRS256 = "RS256"

// Leeway is the clock skew tolerated on exp, nbf and iat.
const Leeway = 30 * time.Second

// Failure reasons: a closed set, safe for logs and metrics. They never carry
// token material.
const (
	ReasonMalformed    = "malformed"
	ReasonAlg          = "alg_refused"
	ReasonUnknownKey   = "unknown_kid"
	ReasonSignature    = "bad_signature"
	ReasonIssuer       = "iss_mismatch"
	ReasonAudience     = "aud_mismatch"
	ReasonExpired      = "expired"
	ReasonNotYetValid  = "not_yet_valid"
	ReasonNoPrincipal  = "no_principal"
	ReasonNotAllowed   = "principal_not_allowed"
	ReasonJWKSDown     = "jwks_unavailable"
	ReasonTokenTooLong = "token_too_long"
	// ReasonGrant: RequireGrant is on and the token names neither a team
	// grant nor a personal grant (or names both).
	ReasonGrant = "grant_refused"
	// ReasonNotAdmitted: the Admitter gave a definite no (no consent).
	ReasonNotAdmitted = "not_admitted"
	// ReasonAdmitDown: the Admitter could not decide; never a pass.
	ReasonAdmitDown = "admission_unavailable"
	// ReasonFreshLogin: the verifier requires a fresh login and the token
	// carries no auth_time, a malformed one, one in the future, or one older
	// than the maximum age.
	ReasonFreshLogin = "fresh_login_required"
)

// MaxFreshLoginAge is the hard cap on Config.MaxAuthAge: a verifier asked to
// accept a login older than this refuses to be built.
const MaxFreshLoginAge = 15 * time.Minute

// GrantKindPersonal is the tgk claim of a token minted under a personal
// grant: a grant scoped to the principal alone, with no team (stn).
const GrantKindPersonal = "personal"

// ErrNotAdmitted is an Admitter's definite no. Any other Admitter error
// means the decision could not be read and refuses with 503.
var ErrNotAdmitted = errors.New("authn: principal not admitted")

// Admitter decides, per request, whether a verified principal may proceed
// (consentgate.Gate: an active glovo:c2 consent). nil is yes; an error
// wrapping ErrNotAdmitted is no; any other error is a failed read.
type Admitter interface {
	Admit(ctx context.Context, principalID string) error
}

// maxTokenBytes bounds the work an unauthenticated caller can cause.
const maxTokenBytes = 8 << 10

// Error is a refused token. Status is the HTTP status the refusal maps to:
// 401 for a token that is not valid here, 403 for a valid token whose
// principal is not allowed, 503 when the JWKS could not be observed.
type Error struct {
	Reason string
	Status int
}

func (e *Error) Error() string { return "authn: " + e.Reason }

func refuse(reason string) *Error {
	switch reason {
	case ReasonNotAllowed, ReasonNotAdmitted, ReasonSubjectNotAllowed:
		return &Error{Reason: reason, Status: http.StatusForbidden}
	case ReasonJWKSDown, ReasonAdmitDown:
		return &Error{Reason: reason, Status: http.StatusServiceUnavailable}
	}
	return &Error{Reason: reason, Status: http.StatusUnauthorized}
}

// Principal is the verified caller.
type Principal struct {
	// ID is the mctl-api principal id (the pid claim), e.g. prn_....
	ID string
	// Subject is the sub claim, for logs only.
	Subject string
	// GrantID is the tgr claim: the mctl-api grant the token was minted
	// under. Read only with RequireGrant; it is a pointer, not authority.
	GrantID string
	// Tenant is the stn claim of a team grant; empty for a personal one.
	Tenant string
	// Personal is true for a personal grant (tgk=personal, no team).
	Personal bool
}

// Config configures a Verifier.
type Config struct {
	// Issuer is the exact expected iss, e.g. https://api.mctl.ai.
	Issuer string
	// Resource is the exact expected audience, e.g. https://glovo.mctl.ai/mcp.
	Resource string
	// JWKSURL is mctl-api's key set, e.g. https://api.mctl.ai/.well-known/jwks.json.
	JWKSURL string
	// AllowedPrincipals is the pid allowlist. Without an Admitter it is the
	// whole gate and empty refuses everyone. With one it is an extra
	// restriction when set (a staged pilot), and unset leaves the decision
	// to the Admitter alone.
	AllowedPrincipals []string
	// Admitter, when set, must admit every principal (after the allowlist).
	Admitter Admitter
	// RequireGrant refuses a token that does not name exactly one grant
	// shape: a team grant (tgr and stn, no tgk) or a personal grant (tgr,
	// tgk=personal, no stn). Off, the grant claims are not read.
	RequireGrant bool
	// MaxAuthAge, when above zero, makes a fresh login part of the token's
	// validity (the link routes): the token must carry an integer auth_time
	// that is not in the future (beyond Leeway) and no older than MaxAuthAge.
	// A token without auth_time (a GitHub-derived token, a refresh-derived
	// one, an ordinary connector token) is refused with ReasonFreshLogin.
	// Zero leaves auth_time unread. Above MaxFreshLoginAge is a
	// configuration error.
	MaxAuthAge time.Duration
	HTTPClient *http.Client
	Now        func() time.Time
}

// Verifier verifies bearer tokens.
type Verifier struct {
	issuer   string
	resource string
	allowed  map[string]struct{}
	admit    Admitter
	grant    bool
	maxAge   time.Duration
	jwks     *jwksCache
	now      func() time.Time
}

// New builds a Verifier. It makes no network call.
func New(cfg Config) (*Verifier, error) {
	if cfg.Issuer == "" || cfg.Resource == "" || cfg.JWKSURL == "" {
		return nil, errors.New("authn: issuer, resource and jwks url are required")
	}
	if cfg.MaxAuthAge < 0 || cfg.MaxAuthAge > MaxFreshLoginAge {
		return nil, errors.New("authn: the maximum auth age must be within (0, 15m], or zero for none")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	allowed := make(map[string]struct{}, len(cfg.AllowedPrincipals))
	for _, p := range cfg.AllowedPrincipals {
		if p = strings.TrimSpace(p); p != "" {
			allowed[p] = struct{}{}
		}
	}
	return &Verifier{
		issuer:   cfg.Issuer,
		resource: cfg.Resource,
		allowed:  allowed,
		admit:    cfg.Admitter,
		grant:    cfg.RequireGrant,
		maxAge:   cfg.MaxAuthAge,
		jwks:     newJWKSCache(cfg.JWKSURL, cfg.HTTPClient, now, false),
		now:      now,
	}, nil
}

// Refresh fetches the JWKS now, for startup and readiness.
func (v *Verifier) Refresh(ctx context.Context) error { return v.jwks.fetch(ctx) }

// Ready reports whether a usable key set is cached; it tries one throttled
// refresh when it is not.
func (v *Verifier) Ready(ctx context.Context) error {
	if v.jwks.usable() {
		return nil
	}
	_, _ = v.jwks.refreshThrottled(ctx)
	if v.jwks.usable() {
		return nil
	}
	return ErrJWKSUnavailable
}

type header struct {
	Alg  string          `json:"alg"`
	Kid  string          `json:"kid"`
	Crit json.RawMessage `json:"crit"`
}

type claims struct {
	Iss string          `json:"iss"`
	Sub string          `json:"sub"`
	Aud json.RawMessage `json:"aud"`
	PID string          `json:"pid"`
	Tgr string          `json:"tgr"`
	Stn string          `json:"stn"`
	Tgk string          `json:"tgk"`
	Exp *json.Number    `json:"exp"`
	Nbf *json.Number    `json:"nbf"`
	Iat *json.Number    `json:"iat"`
	// AuthTime is read only when the verifier requires a fresh login. It is
	// kept raw: a quoted number, a fraction or an exponent is not a login time.
	AuthTime json.RawMessage `json:"auth_time"`
}

// Verify checks token and returns the principal. A returned error is always
// an *Error.
func (v *Verifier) Verify(ctx context.Context, token string) (*Principal, error) {
	if len(token) > maxTokenBytes {
		return nil, refuse(ReasonTokenTooLong)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, refuse(ReasonMalformed)
	}
	hdrJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, refuse(ReasonMalformed)
	}
	var hdr header
	if err := json.Unmarshal(hdrJSON, &hdr); err != nil {
		return nil, refuse(ReasonMalformed)
	}
	// Pin the algorithm before any key work: none, HS256 and every other
	// alg are refused outright.
	if hdr.Alg != AlgEdDSA {
		return nil, refuse(ReasonAlg)
	}
	if len(hdr.Crit) > 0 {
		return nil, refuse(ReasonMalformed)
	}
	if hdr.Kid == "" {
		return nil, refuse(ReasonUnknownKey)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, refuse(ReasonSignature)
	}
	key, ok, err := v.jwks.key(ctx, hdr.Kid)
	if err != nil {
		return nil, refuse(ReasonJWKSDown)
	}
	if !ok {
		return nil, refuse(ReasonUnknownKey)
	}
	edKey, isEd := key.(ed25519.PublicKey)
	if !isEd || !ed25519.Verify(edKey, []byte(parts[0]+"."+parts[1]), sig) {
		return nil, refuse(ReasonSignature)
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, refuse(ReasonMalformed)
	}
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.UseNumber()
	var c claims
	if err := dec.Decode(&c); err != nil {
		return nil, refuse(ReasonMalformed)
	}
	if c.Iss != v.issuer {
		return nil, refuse(ReasonIssuer)
	}
	if !exactAudience(c.Aud, v.resource) {
		return nil, refuse(ReasonAudience)
	}
	now := v.now()
	exp, ok := unixTime(c.Exp)
	if !ok {
		return nil, refuse(ReasonExpired)
	}
	if !now.Before(exp.Add(Leeway)) {
		return nil, refuse(ReasonExpired)
	}
	if c.Nbf != nil {
		nbf, ok := unixTime(c.Nbf)
		if !ok || nbf.After(now.Add(Leeway)) {
			return nil, refuse(ReasonNotYetValid)
		}
	}
	if c.Iat != nil {
		iat, ok := unixTime(c.Iat)
		if !ok || iat.After(now.Add(Leeway)) {
			return nil, refuse(ReasonNotYetValid)
		}
	}
	if strings.TrimSpace(c.PID) == "" {
		return nil, refuse(ReasonNoPrincipal)
	}
	if v.maxAge > 0 && !freshLogin(c.AuthTime, now, v.maxAge) {
		return nil, refuse(ReasonFreshLogin)
	}
	p := &Principal{ID: c.PID, Subject: c.Sub}
	if v.grant {
		switch {
		case c.Tgr == "":
			return nil, refuse(ReasonGrant)
		case c.Tgk == GrantKindPersonal && c.Stn == "":
			p.GrantID, p.Personal = c.Tgr, true
		case c.Tgk == "" && c.Stn != "":
			p.GrantID, p.Tenant = c.Tgr, c.Stn
		default:
			return nil, refuse(ReasonGrant)
		}
	}
	// The allowlist binds whenever it is set; without an Admitter it is
	// the only gate, so an empty one refuses everyone.
	if _, ok := v.allowed[c.PID]; !ok && (v.admit == nil || len(v.allowed) > 0) {
		return nil, refuse(ReasonNotAllowed)
	}
	if v.admit != nil {
		switch err := v.admit.Admit(ctx, c.PID); {
		case errors.Is(err, ErrNotAdmitted):
			return nil, refuse(ReasonNotAdmitted)
		case err != nil:
			return nil, refuse(ReasonAdmitDown)
		}
	}
	return p, nil
}

// wholeNumber is a JSON integer literal: no quotes, fraction or exponent.
var wholeNumber = regexp.MustCompile(`^-?[0-9]{1,19}$`)

// freshLogin reports whether auth_time is a whole-second NumericDate no later
// than now+Leeway and no older than maxAge. A missing, fractional,
// non-numeric or out-of-range value is not a fresh login.
func freshLogin(raw json.RawMessage, now time.Time, maxAge time.Duration) bool {
	if !wholeNumber.Match(raw) {
		return false
	}
	secs, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || secs <= 0 || secs > 1<<33 {
		return false
	}
	at := time.Unix(secs, 0)
	return !at.After(now.Add(Leeway)) && now.Sub(at) <= maxAge
}

// exactAudience accepts aud only when it names exactly one audience equal to
// resource: the single string mctl-api mints for a one-entry audience, or a
// one-element array. A multi-entry aud is refused even when it contains the
// resource, matching mctl-api's own exact-audience rule for Glovo tokens.
func exactAudience(raw json.RawMessage, resource string) bool {
	if len(raw) == 0 {
		return false
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return one == resource
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return false
	}
	return len(many) == 1 && many[0] == resource
}

// unixTime reads a NumericDate. Fractional seconds are allowed by RFC 7519;
// a missing, non-numeric or out-of-range value is not a time.
func unixTime(n *json.Number) (time.Time, bool) {
	if n == nil {
		return time.Time{}, false
	}
	f, err := n.Float64()
	if err != nil || f < 0 || f > 1<<33 {
		return time.Time{}, false
	}
	return time.Unix(0, 0).Add(time.Duration(f * float64(time.Second))), true
}

type ctxKey struct{}

// WithPrincipal stores the verified principal in ctx.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the verified principal, or nil.
func FromContext(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxKey{}).(*Principal)
	return p
}
