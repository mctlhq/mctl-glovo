package glovoapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func hostedRC(next Session) RefreshCoordinator {
	return func(ctx context.Context, held Session, exchange func(context.Context, Session) (Session, error)) (Session, error) {
		return next, nil
	}
}

// A hosted client never takes a session path, never reads one and never
// writes one: Reload and location writes are refused.
func TestHostedClientGuards(t *testing.T) {
	rc := hostedRC(Session{"accessToken": "a2", "refreshToken": "r2"})
	if _, err := New(t.TempDir()+"/session.json", WithRefreshCoordinator(rc)); !errors.Is(err, ErrHostedSessionPath) {
		t.Fatalf("hosted client with a session path: %v", err)
	}
	if _, err := New("", WithRefreshCoordinator(nil)); err == nil {
		t.Fatal("nil coordinator accepted")
	}
	if _, err := New("", WithRefreshCoordinator(rc), WithRequestGate(nil)); err == nil {
		t.Fatal("nil gate accepted")
	}
	c, err := New("", WithRefreshCoordinator(rc), WithSession(Session{"accessToken": "a1", "refreshToken": "r1"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Reload(); !errors.Is(err, ErrHostedNoFile) {
		t.Fatalf("hosted Reload: %v", err)
	}
	lat, lng := 41.39, 2.17
	if _, err := c.SetLocation(LocationUpdate{Latitude: lat, Longitude: lng, CountryCode: "ES", CityCode: "BCN"}); !errors.Is(err, ErrHostedNoFile) {
		t.Fatalf("hosted SetLocation: %v", err)
	}
}

// A hosted refresh goes through the coordinator and leaves no file behind.
func TestHostedRefreshUsesCoordinatorAndNoFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var calls atomic.Int64
	rc := func(ctx context.Context, held Session, exchange func(context.Context, Session) (Session, error)) (Session, error) {
		calls.Add(1)
		if held["refreshToken"] != "r1" {
			t.Errorf("coordinator got %v", held["refreshToken"])
		}
		return Session{"accessToken": "a2", "refreshToken": "r2"}, nil
	}
	c, err := New("", WithRefreshCoordinator(rc), WithSession(Session{"accessToken": "a1", "refreshToken": "r1"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || c.Session()["accessToken"] != "a2" {
		t.Fatalf("coordinator calls %d, session %v", calls.Load(), c.Session()["accessToken"])
	}
	if ents, _ := os.ReadDir(home); len(ents) != 0 {
		t.Fatalf("hosted refresh wrote %d entries under HOME", len(ents))
	}
}

// The request gate runs before every request, the refresh exchange
// included, and a refusal never reaches the network.
func TestHostedRequestGateRunsBeforeNetwork(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	refused := errors.New("gate says no")
	rc := func(ctx context.Context, held Session, exchange func(context.Context, Session) (Session, error)) (Session, error) {
		return exchange(ctx, held)
	}
	c, err := New("", WithBaseURL(srv.URL), WithRefreshCoordinator(rc),
		WithRequestGate(func(context.Context) error { return refused }),
		WithSession(Session{"accessToken": "a1", "refreshToken": "r1"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(context.Background()); !errors.Is(err, refused) {
		t.Fatalf("refresh past a refusing gate: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("gate refused but %d requests reached the network", hits.Load())
	}
}

// A hosted client with a session writer stores a location change through it,
// applied to the session the writer hands over (not the client's copy), and
// leaves the client untouched when the writer fails. No file is written.
func TestHostedSetLocationUsesSessionWriter(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	rc := func(context.Context, Session, func(context.Context, Session) (Session, error)) (Session, error) {
		return nil, errors.New("unexpected refresh")
	}
	if _, err := New("", WithRefreshCoordinator(rc), WithSessionWriter(nil)); err == nil {
		t.Fatal("nil writer accepted")
	}
	stored := Session{"accessToken": "a-store", "refreshToken": "r-store", "location": map[string]any{"cityCode": "OLD", "label": "keep"}}
	var failWith error
	w := func(_ context.Context, apply func(Session) (Session, error)) (Session, error) {
		if failWith != nil {
			return nil, failWith
		}
		next, err := apply(stored)
		if err == nil {
			stored = next
		}
		return next, err
	}
	c, err := New("", WithRefreshCoordinator(rc), WithSessionWriter(w), WithSession(Session{"accessToken": "a-stale", "refreshToken": "r-stale"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetLocationContext(context.Background(), LocationUpdate{CityCode: "NEW", CountryCode: "ES", Latitude: 1.5, Longitude: 2.5}); err != nil {
		t.Fatal(err)
	}
	if stored["refreshToken"] != "r-store" || get(map[string]any(stored), "location").(map[string]any)["cityCode"] != "NEW" || get(map[string]any(stored), "location").(map[string]any)["label"] != "keep" {
		t.Fatalf("stored = %v", stored)
	}
	if s := c.Session(); s["refreshToken"] != "r-store" {
		t.Fatalf("client did not adopt the stored session: %v", s)
	}
	failWith = errors.New("store down")
	if _, err := c.SetLocationContext(context.Background(), LocationUpdate{CityCode: "BAD"}); err == nil {
		t.Fatal("a failed store write returned success")
	}
	if loc := c.Location(); loc["cityCode"] != "NEW" {
		t.Fatalf("failed write changed the client: %v", loc)
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatalf("hosted write left files: %v", entries)
	}
}

// A hosted location write must not hold the client's session lock while it
// waits for the store's lease. A refresh in flight on the same client holds
// that lease and needs the session lock to build its Glovo request: with the
// lock held across the writer, the two wait on each other until the store's
// busy timeout (found by the Postgres isolation suite, which saw 5 s stalls
// and "link is busy" answers under concurrent refresh + set_location).
func TestHostedSetLocationDoesNotHoldSessionLockWhileWaitingForTheLease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"a2","refreshToken":"r2","expiresIn":3600}`))
	}))
	defer srv.Close()

	var lease sync.Mutex // stands in for the store's refresh lease
	refresherHasLease := make(chan struct{})
	writerEntered := make(chan struct{})
	rc := func(ctx context.Context, held Session, exchange func(context.Context, Session) (Session, error)) (Session, error) {
		lease.Lock()
		defer lease.Unlock()
		close(refresherHasLease)
		// Give a concurrent location write the chance to reach the lease
		// first. If the client serialises it behind this refresh it never
		// gets there, which is the fix.
		select {
		case <-writerEntered:
			time.Sleep(50 * time.Millisecond)
		case <-time.After(300 * time.Millisecond):
		}
		return exchange(ctx, held)
	}
	writer := func(ctx context.Context, apply func(Session) (Session, error)) (Session, error) {
		close(writerEntered)
		lease.Lock()
		defer lease.Unlock()
		return apply(Session{"accessToken": "a2", "refreshToken": "r2"})
	}
	c, err := New("", WithBaseURL(srv.URL), WithRefreshCoordinator(rc), WithSessionWriter(writer),
		WithSession(Session{"accessToken": "a1", "refreshToken": "r1"}))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() { done <- c.Refresh(context.Background()) }()
	<-refresherHasLease
	go func() {
		_, err := c.SetLocationContext(context.Background(), LocationUpdate{CountryCode: "XX", CityCode: "TST", Latitude: "1", Longitude: "2"})
		done <- err
	}()
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("refresh and location write deadlocked on the session lock and the lease")
		}
	}
	if c.Location()["cityCode"] != "TST" {
		t.Fatalf("location = %v", c.Location())
	}
}
