package glovoapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
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
