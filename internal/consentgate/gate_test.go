package consentgate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mctlhq/mctl-glovo/internal/authn"
	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

const (
	pidA    = "prn_alice000000000000000000"
	version = "c2-link-v1"
)

type fakeSource struct {
	mu       sync.Mutex
	versions map[string]string
	err      error
	reads    int
}

func (f *fakeSource) RecordedConsent(_ context.Context, pid tokenstore.PID) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.err != nil {
		return "", f.err
	}
	v, ok := f.versions[pid.String()]
	if !ok {
		return "", ErrNoConsent
	}
	return v, nil
}

func (f *fakeSource) set(v string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v == "" {
		delete(f.versions, pidA)
	} else {
		f.versions[pidA] = v
	}
	f.err = err
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newGate(t *testing.T) (*Gate, *fakeSource, *clock) {
	t.Helper()
	src := &fakeSource{versions: map[string]string{pidA: version}}
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	g, err := New(Config{Source: src, Version: version, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	return g, src, c
}

func pid() tokenstore.PID { return tokenstore.PIDFrom(&authn.Principal{ID: pidA}) }

func TestNewBoundsTheCache(t *testing.T) {
	src := &fakeSource{}
	for _, ttl := range []time.Duration{-time.Second, MaxTTL + time.Nanosecond, time.Hour} {
		if _, err := New(Config{Source: src, Version: version, TTL: ttl}); err == nil {
			t.Errorf("ttl %s accepted", ttl)
		}
	}
	if _, err := New(Config{Source: src, Version: version, TTL: MaxTTL}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Version: version}); err == nil {
		t.Fatal("no source accepted")
	}
	if _, err := New(Config{Source: src}); err == nil {
		t.Fatal("no version accepted")
	}
	g, _ := New(Config{Source: src, Version: version})
	if g.ttl != DefaultTTL || DefaultTTL > MaxTTL || MaxTTL > time.Minute {
		t.Fatalf("ttl %s", g.ttl)
	}
}

// A positive answer is reused only within the TTL; a withdrawn consent
// refuses at the first read after it, so within one TTL.
func TestPositiveCacheBoundsRevocation(t *testing.T) {
	ctx := context.Background()
	g, src, c := newGate(t)
	if err := g.Check(ctx, pid()); err != nil {
		t.Fatal(err)
	}
	src.set("", nil) // withdrawn in mctl-api
	c.t = c.t.Add(DefaultTTL - time.Second)
	if err := g.Check(ctx, pid()); err != nil || src.reads != 1 {
		t.Fatalf("cached answer not used: %v reads=%d", err, src.reads)
	}
	c.t = c.t.Add(time.Second)
	if err := g.Check(ctx, pid()); !errors.Is(err, ErrNoConsent) {
		t.Fatalf("withdrawn consent after the TTL: %v", err)
	}
}

// "No" and failed reads are never cached, and a failed read is never a pass.
func TestNegativesAndErrorsAreNotCached(t *testing.T) {
	ctx := context.Background()
	g, src, c := newGate(t)
	src.set("", nil)
	for i := 0; i < 2; i++ {
		if err := g.Check(ctx, pid()); !errors.Is(err, ErrNoConsent) {
			t.Fatalf("no consent: %v", err)
		}
	}
	if src.reads != 2 {
		t.Fatalf("a no was cached: reads=%d", src.reads)
	}
	src.set(version, nil)
	if err := g.Check(ctx, pid()); err != nil {
		t.Fatalf("consent recorded after a no: %v", err)
	}
	c.t = c.t.Add(DefaultTTL)
	src.set(version, errors.New("mctl-api down"))
	for i := 0; i < 2; i++ {
		err := g.Check(ctx, pid())
		if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrNoConsent) {
			t.Fatalf("read error: %v", err)
		}
	}
	src.set(version, nil)
	reads := src.reads
	if err := g.Check(ctx, pid()); err != nil || src.reads != reads+1 {
		t.Fatalf("after an error: %v reads=%d", err, src.reads-reads)
	}
}

func TestOtherVersionIsNoConsent(t *testing.T) {
	g, src, _ := newGate(t)
	src.set("c2-link-v0", nil)
	if err := g.Check(context.Background(), pid()); !errors.Is(err, ErrNoConsent) {
		t.Fatal(err)
	}
	if err := g.Check(context.Background(), tokenstore.PID{}); !errors.Is(err, ErrNoConsent) {
		t.Fatal("zero pid admitted")
	}
}

func TestAdapters(t *testing.T) {
	ctx := context.Background()
	g, src, _ := newGate(t)
	if g.Admit(ctx, pidA) != nil || g.LinkConsent(ctx, pid()) != nil {
		t.Fatal("consent held refused")
	}
	g2, src2, _ := newGate(t)
	src2.set("", nil)
	if err := g2.Admit(ctx, pidA); !errors.Is(err, authn.ErrNotAdmitted) {
		t.Fatalf("admit no: %v", err)
	}
	if err := g2.LinkConsent(ctx, pid()); !errors.Is(err, tokenstore.ErrConsentRefused) {
		t.Fatalf("link no: %v", err)
	}
	if err := g2.Admit(ctx, "not-a-prn"); !errors.Is(err, authn.ErrNotAdmitted) {
		t.Fatalf("malformed pid: %v", err)
	}
	src2.set(version, errors.New("down"))
	if err := g2.Admit(ctx, pidA); err == nil || errors.Is(err, authn.ErrNotAdmitted) {
		t.Fatalf("admit error: %v", err)
	}
	if err := g2.LinkConsent(ctx, pid()); err == nil || errors.Is(err, tokenstore.ErrConsentRefused) {
		t.Fatalf("link error: %v", err)
	}
	_ = src
}

// CheckFresh never rests on the positive cache: a consent withdrawn right
// after a cached yes is seen at once, which is what a link upload needs.
func TestCheckFreshIgnoresThePositiveCache(t *testing.T) {
	g, src, _ := newGate(t)
	ctx := context.Background()
	pid := tokenstore.PIDFrom(&authn.Principal{ID: pidA})
	if err := g.Check(ctx, pid); err != nil {
		t.Fatalf("Check: %v", err)
	}
	src.set("", nil) // withdrawn after the cached yes
	if err := g.Check(ctx, pid); err != nil {
		t.Fatalf("control: the cached yes should still answer Check, got %v", err)
	}
	if err := g.CheckFresh(ctx, pid); !errors.Is(err, ErrNoConsent) {
		t.Fatalf("CheckFresh after withdrawal = %v, want ErrNoConsent", err)
	}
	// The negative answer dropped the cached yes for everyone.
	if err := g.Check(ctx, pid); !errors.Is(err, ErrNoConsent) {
		t.Fatalf("Check after CheckFresh refused = %v, want ErrNoConsent", err)
	}
}

func TestCheckFreshReadsEveryTimeAndFailsClosed(t *testing.T) {
	g, src, _ := newGate(t)
	ctx := context.Background()
	pid := tokenstore.PIDFrom(&authn.Principal{ID: pidA})
	for i := 0; i < 3; i++ {
		if err := g.CheckFresh(ctx, pid); err != nil {
			t.Fatal(err)
		}
	}
	if src.reads != 3 {
		t.Fatalf("CheckFresh read the source %d times in 3 calls, want 3", src.reads)
	}
	src.set(version, errors.New("mctl-api down"))
	if err := g.CheckFresh(ctx, pid); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unreadable = %v, want ErrUnavailable", err)
	}
	if err := g.CheckFresh(ctx, tokenstore.PID{}); !errors.Is(err, ErrNoConsent) {
		t.Fatalf("zero pid = %v, want ErrNoConsent", err)
	}
}

// LinkConsent is the upload's re-check: after a cached yes and a withdrawal it
// must refuse with the consent sentinel the store maps to consent_refused.
func TestLinkConsentIsUncached(t *testing.T) {
	g, src, _ := newGate(t)
	ctx := context.Background()
	pid := tokenstore.PIDFrom(&authn.Principal{ID: pidA})
	if err := g.Admit(ctx, pidA); err != nil { // fills the cache, as the token check does
		t.Fatal(err)
	}
	src.set("", nil)
	if err := g.LinkConsent(ctx, pid); !errors.Is(err, tokenstore.ErrConsentRefused) {
		t.Fatalf("LinkConsent after withdrawal = %v, want ErrConsentRefused", err)
	}
	src.set(version, errors.New("down"))
	if err := g.LinkConsent(ctx, pid); err == nil || errors.Is(err, tokenstore.ErrConsentRefused) {
		t.Fatalf("LinkConsent unreadable = %v, want a non-consent error (failed read)", err)
	}
}
