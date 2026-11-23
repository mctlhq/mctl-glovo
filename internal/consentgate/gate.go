// Package consentgate answers, per request, whether a principal holds an
// active glovo:c2 consent, read from mctl-api's consent record. When the
// source also requires approved access (GLOVO_C2_REQUIRE_ACCESS), "holds a
// consent" below means consent and approval together.
//
// It is the C2 access gate: the token verifier admits a principal only
// through it (authn.Admitter), and /link/upload re-checks it before a
// session is stored. A positive answer is cached for at most MaxTTL, so a
// consent withdrawn in mctl-api takes effect here within that time. A "no"
// and a failed read are never cached: the next request reads again, and a
// failed read refuses, never passes.
package consentgate

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mctlhq/mctl-glovo/internal/authn"
	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

// MaxTTL bounds how long a positive answer is trusted.
const MaxTTL = 60 * time.Second

// DefaultTTL is the positive cache lifetime when Config.TTL is zero.
const DefaultTTL = 30 * time.Second

// maxEntries bounds the cache; past it a positive answer is not cached.
const maxEntries = 10000

var (
	// ErrNoConsent is the documented absence: mctl-api has no active
	// consent record for the principal. Sources return it (or wrap it).
	ErrNoConsent = errors.New("consentgate: no consent recorded")
	// ErrAccessNotApproved is the second definite no: the consent holds,
	// but mctl-api reports the principal's access request as anything other
	// than approved (GLOVO_C2_REQUIRE_ACCESS). It wraps ErrNoConsent, so
	// every caller that only knows ErrNoConsent still refuses and never
	// caches; callers that tell the two apart name a different cause.
	ErrAccessNotApproved = fmt.Errorf("%w: access not approved", ErrNoConsent)
	// ErrUnavailable means the consent could not be read. It refuses.
	ErrUnavailable = errors.New("consentgate: consent unavailable")
)

// Source reads the consent version recorded for pid (linkflow.APIConsent).
// ErrNoConsent (ErrAccessNotApproved wraps it) is the only "no"; any other
// error is a failed read. A source that also requires approved access
// returns a version only when both hold, so the gate never caches a consent
// without the approval.
type Source interface {
	RecordedConsent(ctx context.Context, pid tokenstore.PID) (string, error)
}

// Config configures a Gate.
type Config struct {
	Source Source
	// Version is the consent text version that counts; a consent to any
	// other version is no consent.
	Version string
	// TTL is the positive cache lifetime: zero is DefaultTTL, at most
	// MaxTTL.
	TTL time.Duration
	Now func() time.Time
}

// Gate is the consent check with its short positive cache.
type Gate struct {
	src     Source
	version string
	ttl     time.Duration
	now     func() time.Time

	mu sync.Mutex
	ok map[string]time.Time // pid -> positive answer valid until
}

// New builds a Gate.
func New(cfg Config) (*Gate, error) {
	if cfg.Source == nil || cfg.Version == "" {
		return nil, errors.New("consentgate: a source and a consent version are required")
	}
	ttl := cfg.TTL
	if ttl == 0 {
		ttl = DefaultTTL
	}
	if ttl < 0 || ttl > MaxTTL {
		return nil, fmt.Errorf("consentgate: ttl %s is outside (0, %s]", ttl, MaxTTL)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Gate{src: cfg.Source, version: cfg.Version, ttl: ttl, now: now, ok: map[string]time.Time{}}, nil
}

// Check is nil when pid holds an active consent to Version. ErrNoConsent is
// a definite no; ErrUnavailable is a failed read. Both refuse.
func (g *Gate) Check(ctx context.Context, pid tokenstore.PID) error {
	return g.check(ctx, pid, true)
}

// CheckFresh is Check without the positive cache: it always reads the
// source, so a consent withdrawn a moment ago is seen now. The link routes
// use it, because a link stores a real Glovo session and must never rest on
// a cached yes. A positive answer still refreshes the cache for Check; a
// negative or failed one drops it.
func (g *Gate) CheckFresh(ctx context.Context, pid tokenstore.PID) error {
	return g.check(ctx, pid, false)
}

func (g *Gate) check(ctx context.Context, pid tokenstore.PID, useCache bool) error {
	id := pid.String()
	if id == "" {
		return ErrNoConsent
	}
	now := g.now()
	if useCache {
		g.mu.Lock()
		until, hit := g.ok[id]
		g.mu.Unlock()
		if hit && now.Before(until) {
			return nil
		}
	}
	v, err := g.src.RecordedConsent(ctx, pid)
	switch {
	case errors.Is(err, ErrAccessNotApproved):
		g.forget(id)
		return ErrAccessNotApproved
	case errors.Is(err, ErrNoConsent):
		g.forget(id)
		return ErrNoConsent
	case err != nil:
		// Never cached, and an earlier positive answer is not reused.
		g.forget(id)
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	case v != g.version:
		g.forget(id)
		return ErrNoConsent
	}
	g.remember(id, now.Add(g.ttl), now)
	return nil
}

// Admit implements authn.Admitter for the token verifier.
func (g *Gate) Admit(ctx context.Context, principalID string) error {
	pid := tokenstore.PIDFrom(&authn.Principal{ID: principalID})
	switch err := g.Check(ctx, pid); {
	case errors.Is(err, ErrAccessNotApproved):
		return fmt.Errorf("%w: %w", authn.ErrAccessNotApproved, err)
	case errors.Is(err, ErrNoConsent):
		return fmt.Errorf("%w: %w", authn.ErrNotAdmitted, err)
	case err != nil:
		return err
	}
	return nil
}

// LinkConsent is tokenstore.Config.LinkConsent: the re-check /link/upload
// makes after the code is redeemed and before the session is stored. It reads
// the source every time (CheckFresh), never the positive cache.
func (g *Gate) LinkConsent(ctx context.Context, pid tokenstore.PID) error {
	switch err := g.CheckFresh(ctx, pid); {
	case errors.Is(err, ErrNoConsent):
		return fmt.Errorf("%w: %w", tokenstore.ErrConsentRefused, err)
	case err != nil:
		return err
	}
	return nil
}

func (g *Gate) forget(id string) {
	g.mu.Lock()
	delete(g.ok, id)
	g.mu.Unlock()
}

func (g *Gate) remember(id string, until, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.ok) >= maxEntries {
		for k, u := range g.ok {
			if !now.Before(u) {
				delete(g.ok, k)
			}
		}
		if len(g.ok) >= maxEntries {
			return
		}
	}
	g.ok[id] = until
}
