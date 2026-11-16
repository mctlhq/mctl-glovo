package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mctlhq/mctl-glovo/internal/linkflow"
	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

const (
	admPidA  = "prn_01HZZZZZZZZZZZZZZZZZZZZZZA"
	admPidB  = "prn_01HZZZZZZZZZZZZZZZZZZZZZZB"
	admToken = "machine-token-not-a-secret" //nolint:gosec // fake test value
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// admitEnv is a fake mctl-api consent read plus a real APIConsent client.
type admitEnv struct {
	admit     callAdmitter
	clock     *fakeClock
	hits      atomic.Int64
	tokenFile string
	mu        sync.Mutex
	// answer maps pid to the (status, body) the fake returns.
	answer func(pid string) (int, string)
}

func activeBody(pid string) string {
	return `{"principal_id":"` + pid + `","state":"active","consent_version":"` + linkflow.ConsentVersion + `"}`
}
func noneBody(pid string) string { return `{"principal_id":"` + pid + `","state":"none"}` }

func newAdmitEnv(t *testing.T, ttl time.Duration) *admitEnv {
	t.Helper()
	e := &admitEnv{clock: &fakeClock{t: time.Unix(1_800_000_000, 0)}}
	e.answer = func(pid string) (int, string) { return 200, activeBody(pid) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hits.Add(1)
		pid := strings.TrimPrefix(r.URL.Path, linkflow.ConsentReadPath)
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, linkflow.ConsentReadPath) || r.Header.Get("Authorization") != "Bearer "+admToken {
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		e.mu.Lock()
		st, body := e.answer(pid)
		e.mu.Unlock()
		w.WriteHeader(st)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	e.tokenFile = filepath.Join(t.TempDir(), "token")
	e.setToken(t, admToken+"\n")
	src, err := linkflow.NewAPIConsent(linkflow.APIConsentConfig{
		BaseURL: srv.URL, TokenFile: e.tokenFile,
		Client: &http.Client{Timeout: 300 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.admit, err = newCallAdmitterTTL(src, ttl, e.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *admitEnv) setToken(t *testing.T, s string) {
	t.Helper()
	if err := os.WriteFile(e.tokenFile, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (e *admitEnv) set(f func(pid string) (int, string)) {
	e.mu.Lock()
	e.answer = f
	e.mu.Unlock()
}

// Only an explicit active consent admits.
func TestAdmitAdmitsOnlyExplicitPositive(t *testing.T) {
	e := newAdmitEnv(t, admitTTL)
	if err := e.admit(context.Background(), admPidA); err != nil {
		t.Fatalf("active consent refused: %v", err)
	}
}

// Every failure class refuses, and none is cached as an admission.
func TestAdmitEveryFailureClassRefuses(t *testing.T) {
	big := `{"principal_id":"` + admPidA + `","state":"active","consent_version":"` + linkflow.ConsentVersion + `","pad":"` + strings.Repeat("x", 8<<10) + `"}`
	cases := map[string]func(pid string) (int, string){
		"none": func(pid string) (int, string) { return 200, noneBody(pid) },
		"wrong version": func(pid string) (int, string) {
			return 200, strings.Replace(activeBody(pid), linkflow.ConsentVersion, "old-v0", 1)
		},
		"503":               func(string) (int, string) { return 503, `{"code":"glovo_consent_unavailable"}` },
		"401":               func(string) (int, string) { return 401, `{}` },
		"403":               func(string) (int, string) { return 403, `{}` },
		"404 unmounted":     func(pid string) (int, string) { return 404, noneBody(pid) },
		"429":               func(string) (int, string) { return 429, `` },
		"500 active body":   func(pid string) (int, string) { return 500, activeBody(pid) },
		"201 active body":   func(pid string) (int, string) { return 201, activeBody(pid) },
		"302":               func(string) (int, string) { return 302, `` },
		"malformed":         func(string) (int, string) { return 200, `{"state":` },
		"empty body":        func(string) (int, string) { return 200, `` },
		"unknown state":     func(pid string) (int, string) { return 200, `{"principal_id":"` + pid + `","state":"pending"}` },
		"active no version": func(pid string) (int, string) { return 200, `{"principal_id":"` + pid + `","state":"active"}` },
		"other principal":   func(string) (int, string) { return 200, activeBody(admPidB) },
		"oversized body":    func(string) (int, string) { return 200, big },
		"no principal field": func(string) (int, string) {
			return 200, `{"state":"active","consent_version":"` + linkflow.ConsentVersion + `"}`
		},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			e := newAdmitEnv(t, admitTTL)
			e.set(f)
			if err := e.admit(context.Background(), admPidA); err == nil {
				t.Fatal("admitted")
			}
			// Never cached as a pass: a second call asks again.
			before := e.hits.Load()
			if err := e.admit(context.Background(), admPidA); err == nil || e.hits.Load() == before {
				t.Fatalf("second call err=%v hits %d->%d", err, before, e.hits.Load())
			}
		})
	}
}

func TestAdmitTimeoutAndTransportRefuse(t *testing.T) {
	// A server that never answers within the client timeout.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(func() { close(release); srv.Close() })
	tf := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tf, []byte(admToken), 0o600); err != nil {
		t.Fatal(err)
	}
	src, err := linkflow.NewAPIConsent(linkflow.APIConsentConfig{BaseURL: srv.URL, TokenFile: tf, Client: &http.Client{Timeout: 100 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := newCallAdmitter(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a(context.Background(), admPidA); err == nil {
		t.Fatal("timeout admitted")
	}
	// Connection refused.
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	src2, _ := linkflow.NewAPIConsent(linkflow.APIConsentConfig{BaseURL: url, TokenFile: tf})
	a2, _ := newCallAdmitter(src2, nil)
	if err := a2(context.Background(), admPidA); err == nil {
		t.Fatal("connection refused admitted")
	}
}

type deadlineSource struct{ deadline time.Duration }

func (d *deadlineSource) RecordedConsent(ctx context.Context, _ tokenstore.PID) (string, error) {
	dl, ok := ctx.Deadline()
	if !ok {
		d.deadline = -1
		return linkflow.ConsentVersion, nil
	}
	d.deadline = time.Until(dl)
	return linkflow.ConsentVersion, nil
}

// The decision carries its own deadline: the tool-call timeout starts only
// after admission, so a slow mctl-api must not be bounded by the HTTP client
// alone.
func TestAdmitHasOwnDeadline(t *testing.T) {
	src := &deadlineSource{}
	a, err := newCallAdmitter(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a(context.Background(), admPidA); err != nil {
		t.Fatal(err)
	}
	if src.deadline <= 0 || src.deadline > admitTimeout {
		t.Fatalf("source saw deadline %v, want in (0, %v]", src.deadline, admitTimeout)
	}
}

// A token file that cannot be a machine-user token refuses without a request.
func TestAdmitBadTokenFileRefusesWithoutRequest(t *testing.T) {
	cases := map[string]func(t *testing.T, e *admitEnv){
		"unreadable": func(t *testing.T, e *admitEnv) { _ = os.Remove(e.tokenFile) },
		"empty":      func(t *testing.T, e *admitEnv) { e.setToken(t, "  \n") },
		"oversized":  func(t *testing.T, e *admitEnv) { e.setToken(t, strings.Repeat("a", 9<<10)) },
		"newline inside": func(t *testing.T, e *admitEnv) {
			e.setToken(t, admToken+"\nX-Injected: 1")
		},
		"space inside": func(t *testing.T, e *admitEnv) { e.setToken(t, "a b") },
		"directory": func(t *testing.T, e *admitEnv) {
			_ = os.Remove(e.tokenFile)
			if err := os.Mkdir(e.tokenFile, 0o700); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			e := newAdmitEnv(t, admitTTL)
			f(t, e)
			if err := e.admit(context.Background(), admPidA); err == nil {
				t.Fatal("admitted")
			}
			if n := e.hits.Load(); n != 0 {
				t.Fatalf("%d request(s) sent with a bad token file", n)
			}
		})
	}
}

// The token is re-read on every call (no caching): a rotated file is used on
// the next miss, and a file that turns bad refuses the next miss.
func TestAdmitTokenReadOnEveryCall(t *testing.T) {
	e := newAdmitEnv(t, admitTTL)
	if err := e.admit(context.Background(), admPidA); err != nil {
		t.Fatal(err)
	}
	e.setToken(t, "rotated-token")
	e.clock.Advance(admitTTL)
	// The fake only accepts the original token: a call after rotation must
	// present the new one, which the fake rejects with 418, so it refuses.
	if err := e.admit(context.Background(), admPidA); err == nil {
		t.Fatal("old token still used after rotation")
	}
	e.setToken(t, admToken)
	if err := e.admit(context.Background(), admPidA); err != nil {
		t.Fatalf("restored token refused: %v", err)
	}
}

// Two principals never share a cache entry.
func TestAdmitPrincipalsNeverShareCache(t *testing.T) {
	e := newAdmitEnv(t, admitTTL)
	e.set(func(pid string) (int, string) {
		if pid == admPidA {
			return 200, activeBody(pid)
		}
		return 200, noneBody(pid)
	})
	if err := e.admit(context.Background(), admPidA); err != nil {
		t.Fatalf("A refused: %v", err)
	}
	if err := e.admit(context.Background(), admPidA); err != nil || e.hits.Load() != 1 {
		t.Fatalf("A not served from cache: err=%v hits=%d", err, e.hits.Load())
	}
	if err := e.admit(context.Background(), admPidB); err == nil {
		t.Fatal("B admitted on A's cache entry")
	}
	if e.hits.Load() != 2 {
		t.Fatalf("B did not trigger its own read (hits %d)", e.hits.Load())
	}
	// A pid that is not a verified prn_ id is never admitted and never read.
	for _, bad := range []string{"", "prn_", "A", "prn_a/../b"} {
		before := e.hits.Load()
		if err := e.admit(context.Background(), bad); err == nil || e.hits.Load() != before {
			t.Fatalf("malformed pid %q: err=%v", bad, err)
		}
	}
}

// A revocation takes effect within the hard TTL, and a failed read after a
// positive answer refuses rather than reusing it.
func TestAdmitRevocationWithinTTL(t *testing.T) {
	if admitTTL > maxAdmitTTL || admitTTL > 10*time.Second {
		t.Fatalf("admitTTL %v exceeds the hard bound", admitTTL)
	}
	e := newAdmitEnv(t, admitTTL)
	if err := e.admit(context.Background(), admPidA); err != nil {
		t.Fatal(err)
	}
	e.set(func(pid string) (int, string) { return 200, noneBody(pid) }) // consent withdrawn
	e.clock.Advance(admitTTL - time.Millisecond)
	if err := e.admit(context.Background(), admPidA); err != nil {
		t.Fatalf("inside the TTL the positive is still served (bounded staleness): %v", err)
	}
	e.clock.Advance(time.Millisecond)
	if err := e.admit(context.Background(), admPidA); err == nil {
		t.Fatal("revoked consent still admitted at the TTL")
	}
	// And stays refused, not cached.
	if err := e.admit(context.Background(), admPidA); err == nil {
		t.Fatal("revoked consent admitted on retry")
	}

	// A failed read after a positive: refuse, then recover.
	e.set(func(pid string) (int, string) { return 200, activeBody(pid) })
	if err := e.admit(context.Background(), admPidA); err != nil {
		t.Fatal(err)
	}
	e.set(func(string) (int, string) { return 503, `` })
	e.clock.Advance(admitTTL)
	if err := e.admit(context.Background(), admPidA); err == nil {
		t.Fatal("unreadable consent admitted from an old positive")
	}
	e.set(func(pid string) (int, string) { return 200, activeBody(pid) })
	if err := e.admit(context.Background(), admPidA); err != nil {
		t.Fatalf("did not recover: %v", err)
	}
}

func TestAdmitTTLBounds(t *testing.T) {
	src := &deadlineSource{}
	for _, ttl := range []time.Duration{0, -time.Second, maxAdmitTTL + time.Nanosecond, time.Minute} {
		if _, err := newCallAdmitterTTL(src, ttl, nil); err == nil {
			t.Errorf("ttl %v accepted", ttl)
		}
	}
	if _, err := newCallAdmitterTTL(src, maxAdmitTTL, nil); err != nil {
		t.Errorf("ttl at the bound refused: %v", err)
	}
	if _, err := newCallAdmitterTTL(nil, admitTTL, nil); err == nil {
		t.Error("nil source accepted")
	}
}

// Consent mode has an admitter; allowlist-only mode must not (its source
// never consents, so one would refuse every call).
func TestAdmitterForModes(t *testing.T) {
	a, err := admitterFor(linkflow.UnavailableConsent{}, false)
	if err != nil || a != nil {
		t.Fatalf("allowlist mode = %v %v, want nil", a != nil, err)
	}
	a, err = admitterFor(linkflow.UnavailableConsent{}, true)
	if err != nil || a == nil {
		t.Fatalf("consent mode = %v %v", a != nil, err)
	}
	if err := a(context.Background(), admPidA); err == nil || !errors.Is(err, errAdmitRefused) {
		t.Fatalf("refusal = %v, want errAdmitRefused", err)
	}
}

// No token, pid or upstream detail in the refusal.
func TestAdmitRefusalCarriesNoDetail(t *testing.T) {
	e := newAdmitEnv(t, admitTTL)
	e.set(func(string) (int, string) { return 503, `{"detail":"` + admToken + `"}` })
	err := e.admit(context.Background(), admPidA)
	if err == nil {
		t.Fatal("admitted")
	}
	for _, leak := range []string{admToken, admPidA, "503", "detail"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("refusal %q leaks %q", err, leak)
		}
	}
}
