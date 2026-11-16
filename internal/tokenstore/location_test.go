package tokenstore

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mctlhq/mctl-glovo/internal/glovoapi"
)

func leaseClient(t *testing.T, h *harness, pid string) *glovoapi.Client {
	t.Helper()
	l, err := h.st.Get(context.Background(), h.pid(pid))
	if err != nil {
		t.Fatal(err)
	}
	c, err := l.Client()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func storedCity(s glovoapi.Session) any {
	loc, _ := s["location"].(map[string]any)
	return loc["cityCode"]
}

func setCity(c *glovoapi.Client, city string) error {
	_, err := c.SetLocationContext(context.Background(), glovoapi.LocationUpdate{
		CountryCode: "XX", CityCode: city, Latitude: "1.5", Longitude: "2.5",
	})
	return err
}

// A hosted location change is stored in the encrypted session row of the
// caller's own link, keeps the rest of the session, and never touches
// another principal's row.
func TestHostedSetLocationIsStoredPerPID(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		h.link(pidA, testSession("A1"))
		h.link(pidB, testSession("B1"))
		before := h.stored(pidB)
		rowB := h.row(pidB)

		c := leaseClient(t, h, pidA)
		if err := setCity(c, "TST"); err != nil {
			t.Fatal(err)
		}
		got := h.stored(pidA)
		if storedCity(got) != "TST" {
			t.Fatalf("stored location = %v", got["location"])
		}
		if got["refreshToken"] != "refresh-A1" || got["deviceUrn"] != "urn:device:A1" {
			t.Fatalf("rest of the session lost: %v", got)
		}
		if c.Location()["cityCode"] != "TST" {
			t.Fatalf("client location = %v", c.Location())
		}
		if r := h.row(pidB); r.Version != rowB.Version {
			t.Fatalf("B row version moved %d -> %d", rowB.Version, r.Version)
		}
		if after := h.stored(pidB); after["location"] != nil || after["refreshToken"] != before["refreshToken"] {
			t.Fatalf("B session changed: %v", after)
		}
		// A fresh lease (another replica, or after a cache eviction) sees it.
		if got := leaseClient(t, h, pidA).Location()["cityCode"]; got != "TST" {
			t.Fatalf("new lease location = %v", got)
		}
		// Omitted fields clear, like upstream.
		if _, err := c.SetLocationContext(context.Background(), glovoapi.LocationUpdate{CityCode: "TS2"}); err != nil {
			t.Fatal(err)
		}
		if loc, _ := h.stored(pidA)["location"].(map[string]any); loc["countryCode"] != nil || loc["cityCode"] != "TS2" {
			t.Fatalf("cleared location = %v", loc)
		}
	})
}

// A location change lands on the session the row holds now: a replica with
// stale tokens must not write them back over a rotation.
func TestHostedSetLocationKeepsRotatedTokens(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		h.link(pidA, testSession("A1"))
		stale := leaseClient(t, h, pidA)
		fresh := leaseClient(t, h, pidA)
		if err := fresh.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		_, rotated := tokens(fresh)
		if rotated == "refresh-A1" {
			t.Fatal("refresh did not rotate")
		}
		if err := setCity(stale, "TST"); err != nil {
			t.Fatal(err)
		}
		got := h.stored(pidA)
		if got["refreshToken"] != rotated {
			t.Fatalf("stored refresh token %v, want the rotated %s", got["refreshToken"], rotated)
		}
		if storedCity(got) != "TST" {
			t.Fatalf("location not stored: %v", got["location"])
		}
		if _, r := tokens(stale); r != rotated {
			t.Fatalf("client kept stale token %s", r)
		}
	})
}

// A location that fails validation is refused, stores nothing, and is not a
// failed refresh: it never counts toward needs_relink.
func TestHostedSetLocationRefusedIsNotARefreshFailure(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		h.link(pidA, testSession("A1"))
		c := leaseClient(t, h, pidA)
		before := h.row(pidA)
		huge := strings.Repeat("x", 1<<20)
		for i := 0; i < 5; i++ {
			if err := setCity(c, huge); !errors.Is(err, ErrLocationRefused) {
				t.Fatalf("attempt %d: %v", i, err)
			}
		}
		after := h.row(pidA)
		if after.State != StateActive || after.RefreshFailures != 0 || after.Version != before.Version {
			t.Fatalf("row changed: %+v -> %+v", before, after)
		}
		if c.Location()["cityCode"] == huge {
			t.Fatalf("client kept a refused location: %v", c.Location())
		}
	})
}

// The kill switch stops location writes like every other path.
func TestHostedSetLocationHonoursKillSwitch(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		ctx := context.Background()
		h.link(pidA, testSession("A1"))
		c := leaseClient(t, h, pidA)
		if err := h.raw.SetKilled(ctx, true, "test"); err != nil {
			t.Fatal(err)
		}
		h.kill.mu.Lock()
		h.kill.okUntil = time.Time{}
		h.kill.mu.Unlock()
		if err := setCity(c, "TST"); !errors.Is(err, ErrKilled) {
			t.Fatalf("got %v, want ErrKilled", err)
		}
		if err := h.raw.SetKilled(ctx, false, "test"); err != nil {
			t.Fatal(err)
		}
		if storedCity(h.stored(pidA)) != nil {
			t.Fatal("a killed write was stored")
		}
	})
}

// The audit row says what happened: a location write is not a rotation.
func TestHostedSetLocationAuditReason(t *testing.T) {
	h := memHarness(t, Caps{})
	h.link(pidA, testSession("A1"))
	if err := setCity(leaseClient(t, h, pidA), "TST"); err != nil {
		t.Fatal(err)
	}
	var reasons []string
	h.mem.mu.Lock()
	for _, a := range h.mem.audit {
		if a.Event == "refresh" {
			reasons = append(reasons, a.Reason)
		}
	}
	h.mem.mu.Unlock()
	if len(reasons) != 1 || reasons[0] != "location_set" {
		t.Fatalf("refresh audit reasons = %v", reasons)
	}
}

// A location write is not a refresh: interleaved with counted refresh
// failures it must neither reset the counter nor stamp last_refresh_at, so
// the link still reaches needs_relink.
func TestHostedSetLocationKeepsRefreshFailureAccounting(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		ctx := context.Background()
		h.link(pidA, testSession("A1"))
		var lastRefresh sql.NullTime
		if pg, ok := h.raw.(*Postgres); ok {
			if err := pg.db.QueryRowContext(ctx, `SELECT last_refresh_at FROM glovo_links WHERE pid = $1`, pidA).Scan(&lastRefresh); err != nil {
				t.Fatal(err)
			}
		}
		failRefresh := func() error {
			l, _ := h.st.Get(ctx, h.pid(pidA))
			c, _ := l.Client()
			return c.Refresh(ctx)
		}
		h.glovo.noAccess.Store(true)
		for i := 0; i < 2; i++ {
			if err := failRefresh(); err == nil {
				t.Fatal("refresh succeeded")
			}
			if err := setCity(leaseClient(t, h, pidA), "TS"+strconv.Itoa(i)); err != nil {
				t.Fatal(err)
			}
			r := h.row(pidA)
			if r.RefreshFailures != i+1 {
				t.Fatalf("after failure %d and a location write: refresh_failures = %d", i+1, r.RefreshFailures)
			}
			if pg, ok := h.raw.(*Postgres); ok {
				var at sql.NullTime
				if err := pg.db.QueryRowContext(ctx, `SELECT last_refresh_at FROM glovo_links WHERE pid = $1`, pidA).Scan(&at); err != nil {
					t.Fatal(err)
				}
				if at != lastRefresh {
					t.Fatalf("location write moved last_refresh_at %v -> %v", lastRefresh, at)
				}
			}
		}
		if err := failRefresh(); !errors.Is(err, ErrNeedsRelink) {
			t.Fatalf("3rd counted failure: %v", err)
		}
		if st := h.row(pidA).State; st != StateNeedsRelink {
			t.Fatalf("state %s", st)
		}
	})
}

// An unconfirmed location commit is not retried by anyone, so it must not
// leave the row lease behind: the next write is not ErrBusy.
func TestHostedSetLocationUnconfirmedReleasesLease(t *testing.T) {
	forEachDB(t, func(t *testing.T, h *harness) {
		h.link(pidA, testSession("A1"))
		h.st.sleep = func(context.Context, time.Duration) error { return nil }
		c := leaseClient(t, h, pidA)
		h.db.set("CommitLease", -1)
		h.db.set("FindWrite", -1)
		if err := setCity(c, "TST"); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("unconfirmed write: %v", err)
		}
		h.db.set("CommitLease", 0)
		h.db.set("FindWrite", 0)
		if err := setCity(c, "TS2"); err != nil {
			t.Fatalf("write after an unconfirmed one: %v", err)
		}
		if storedCity(h.stored(pidA)) != "TS2" {
			t.Fatalf("stored = %v", h.stored(pidA)["location"])
		}
	})
}

// A pinned refresh superseded by another writer surfaces as the fixed
// ErrSuperseded on the location and refresh paths, not a raw internal error.
func TestHostedPendingConflictIsSuperseded(t *testing.T) {
	for _, path := range []string{"location", "refresh"} {
		t.Run(path, func(t *testing.T) {
			m := newMemDB()
			h := newHarness(t, m, m, nil, Caps{})
			ctx := context.Background()
			h.link(pidA, testSession("A1"))
			h.st.sleep = func(context.Context, time.Duration) error { return nil }
			c := leaseClient(t, h, pidA)
			h.db.set("CommitLease", -1)
			h.db.set("FindWrite", -1)
			if err := c.Refresh(ctx); err != nil {
				t.Fatal(err)
			}
			m.mu.Lock()
			m.links[pidA].Version += 1000
			m.mu.Unlock()
			h.db.set("CommitLease", 0)
			h.db.set("FindWrite", 0)
			var err error
			if path == "location" {
				err = setCity(c, "TST")
			} else {
				err = c.Refresh(ctx)
			}
			if !errors.Is(err, ErrSuperseded) {
				t.Fatalf("got %v, want ErrSuperseded", err)
			}
		})
	}
}
