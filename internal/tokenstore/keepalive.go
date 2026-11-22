package tokenstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/mctlhq/mctl-glovo/internal/glovoapi"
)

// Keepalive is the hosted session keepalive (design 4.5): once per interval
// and linked pid it makes the one authenticated read the link validator and
// glovo-home's keepalive make (the basket GET). Glovo refreshes the access
// token (about 20 minutes) when a read needs it, so a daily read keeps the
// refresh chain of a linked account that nobody calls from going stale
// unnoticed, and a chain that did die is found and reported (needs_relink)
// instead of at the user's next call.
//
// It adds no refresh, decrypt or rotation path of its own. It asks Sessions
// for the pid's client, exactly as a tool call does, and reads through it:
// the client's refresh runs through the Lease's coordinator (the row lease
// and the (link_id, version) fence of design 2.5), is audited by the store,
// and goes through the egress proxy like every other Glovo request.
//
// Kill switch: a round starts with Store.Running and re-checks before every
// pid. While the switch is on or unreadable it lists nothing, decrypts
// nothing and sends nothing. The store's own checks (Sessions.Client,
// decryptRow, the request gate) apply on top of that.
//
// Decrypt caps: the keepalive's decrypts carry a context marker only this
// package can set. decryptRow counts them in a separate window
// (Config.KeepaliveDecryptsPerHour) and in neither the per-pid cap, which
// suspends, nor the global cap, which trips the kill switch. Its own budget
// being spent only defers it. A background job at the minimum interval can
// therefore never suspend a pid or latch the kill switch.
//
// One goroutine runs the rounds, pids are handled one after another, and the
// schedule lives in memory: after a restart the first read of each pid falls
// at a random point within one interval.
type Keepalive struct {
	store    Store
	sessions *Sessions
	interval time.Duration
	log      *slog.Logger
	read     func(context.Context, *glovoapi.Client) error

	tick        time.Duration // time between rounds
	pace        time.Duration // pause between two pids of one round
	callTimeout time.Duration
	now         func() time.Time
	sleep       func(context.Context, time.Duration) error
	rand        func() float64 // uniform in [0, 1)

	sched  map[string]*kaState // by pid; owned by the Run goroutine
	killed bool                // last round saw the kill switch (for the log)
	done   chan struct{}
}

type kaState struct {
	next     time.Time
	failures int
}

// Bounds of GLOVO_C2_KEEPALIVE_INTERVAL. The minimum keeps the background
// load and the decrypt budget small; the maximum is the one interval that has
// operating evidence (glovo-home's daily default): a longer one would bet on
// an unmeasured refresh-chain lifetime.
const (
	MinKeepaliveInterval = time.Hour
	MaxKeepaliveInterval = 24 * time.Hour
)

const (
	keepaliveTick        = time.Minute
	keepalivePace        = time.Second
	keepaliveCallTimeout = 45 * time.Second
	keepaliveBackoffBase = 5 * time.Minute
	// keepaliveJitter spreads a steady-state schedule by +-10 percent so
	// that pids linked together do not stay in step.
	keepaliveJitter = 0.1
)

// KeepaliveConfig configures NewKeepalive.
type KeepaliveConfig struct {
	Store    Store
	Sessions *Sessions
	// Interval is the time between two reads of one pid, within
	// [MinKeepaliveInterval, MaxKeepaliveInterval].
	Interval time.Duration
	Logger   *slog.Logger
	// Read makes the authenticated read through the pid's client. Nil: the
	// basket GET.
	Read func(context.Context, *glovoapi.Client) error
}

// NewKeepalive validates cfg. It does no I/O and starts nothing.
func NewKeepalive(cfg KeepaliveConfig) (*Keepalive, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("tokenstore: keepalive needs a store")
	case cfg.Sessions == nil:
		return nil, errors.New("tokenstore: keepalive needs sessions")
	case cfg.Interval < MinKeepaliveInterval || cfg.Interval > MaxKeepaliveInterval:
		return nil, fmt.Errorf("tokenstore: keepalive interval %s is outside [%s, %s]", cfg.Interval, MinKeepaliveInterval, MaxKeepaliveInterval)
	}
	k := &Keepalive{
		store:       cfg.Store,
		sessions:    cfg.Sessions,
		interval:    cfg.Interval,
		log:         cfg.Logger,
		read:        cfg.Read,
		tick:        keepaliveTick,
		pace:        keepalivePace,
		callTimeout: keepaliveCallTimeout,
		now:         time.Now,
		sleep:       sleepCtx,
		rand:        rand.Float64,
		sched:       map[string]*kaState{},
		done:        make(chan struct{}),
	}
	if k.log == nil {
		k.log = slog.New(slog.DiscardHandler)
	}
	if k.read == nil {
		k.read = func(ctx context.Context, c *glovoapi.Client) error {
			_, err := c.GetBaskets(ctx)
			return err
		}
	}
	return k, nil
}

// Done is closed when Run has returned.
func (k *Keepalive) Done() <-chan struct{} { return k.done }

// Run runs rounds until ctx is done. It waits one tick before the first
// round, so it never acts in the moment the service starts. Call it once.
func (k *Keepalive) Run(ctx context.Context) {
	defer close(k.done)
	for {
		if err := k.sleep(ctx, k.tick); err != nil {
			return
		}
		k.round(ctx)
	}
}

// keepaliveKey marks a context as the keepalive's. It is unexported: no
// other package can ask for the separate decrypt accounting.
type keepaliveKey struct{}

func withKeepalive(ctx context.Context) context.Context {
	return context.WithValue(ctx, keepaliveKey{}, true)
}

func isKeepalive(ctx context.Context) bool {
	v, _ := ctx.Value(keepaliveKey{}).(bool)
	return v
}

func (k *Keepalive) event(ctx context.Context, level slog.Level, link, outcome, reason string, extra ...slog.Attr) {
	attrs := []slog.Attr{slog.String("event", "keepalive"), slog.String("outcome", outcome)}
	if reason != "" {
		attrs = append(attrs, slog.String("reason", reason))
	}
	if link != "" {
		attrs = append(attrs, slog.String("link_id", link))
	}
	k.log.LogAttrs(ctx, level, "keepalive", append(attrs, extra...)...)
}

// markKilled logs the kill switch once per episode, not once per tick.
func (k *Keepalive) markKilled(ctx context.Context, killed bool) {
	if killed && !k.killed {
		k.event(ctx, slog.LevelInfo, "", "killed", "kill_switch")
	}
	k.killed = killed
}

// round does one pass: it lists the active links and reads each one that is
// due.
func (k *Keepalive) round(ctx context.Context) {
	if k.store.Running(ctx) != nil {
		k.markKilled(ctx, true)
		return
	}
	links, err := k.store.ListActive(ctx)
	if err != nil {
		if errors.Is(err, ErrKilled) {
			k.markKilled(ctx, true)
			return
		}
		if ctx.Err() == nil {
			// A listing that failed is not an empty list: nothing is dropped
			// from the schedule and nothing is concluded.
			k.event(ctx, slog.LevelWarn, "", "list_failed", "store_unavailable")
		}
		return
	}
	k.markKilled(ctx, false)

	now := k.now()
	live := make(map[string]bool, len(links))
	for _, l := range links {
		live[l.PID.id] = true
		if k.sched[l.PID.id] == nil {
			// First sight of this pid (boot or a new link): somewhere within
			// one interval, so a restart does not read every pid at once.
			k.sched[l.PID.id] = &kaState{next: now.Add(time.Duration(k.rand() * float64(k.interval)))}
		}
	}
	for pid := range k.sched {
		if !live[pid] {
			delete(k.sched, pid)
		}
	}
	first := true
	for _, l := range links {
		st := k.sched[l.PID.id]
		if st.next.After(now) {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if !first {
			if err := k.sleep(ctx, k.pace); err != nil {
				return
			}
		}
		first = false
		// Re-check between pids: a kill that lands mid-round stops the rest.
		if k.store.Running(ctx) != nil {
			k.markKilled(ctx, true)
			return
		}
		k.one(ctx, l, st)
	}
}

// one reads for one link and schedules its next read.
func (k *Keepalive) one(ctx context.Context, l ActiveLink, st *kaState) {
	cctx, cancel := context.WithTimeout(withKeepalive(WithRequestID(ctx, newID("kal_"))), k.callTimeout)
	defer cancel()

	client, err := k.sessions.Client(cctx, l.PID)
	if err == nil {
		err = k.read(cctx, client)
	}
	if err == nil {
		st.failures = 0
		k.event(ctx, slog.LevelInfo, l.LinkID, "ok", "")
		st.next = k.now().Add(k.jittered(k.interval, keepaliveJitter))
		return
	}
	if ctx.Err() != nil {
		return // shutting down: not a failure of the session
	}

	var ae *glovoapi.AuthError
	switch {
	case errors.Is(err, ErrKilled):
		// Not a failure of the session; the next round re-checks the switch.
		k.markKilled(ctx, true)
	case errors.Is(err, ErrNeedsRelink):
		// Glovo refused the refresh chain (the store already set the state
		// and audited refresh/glovo_rejected). The pid leaves the schedule;
		// the user must relink. This line is the one an alert matches.
		k.event(ctx, slog.LevelWarn, l.LinkID, "needs_relink", "chain_refused", slog.String("alert", "GlovoSessionNeedsRelink"))
		delete(k.sched, l.PID.id)
	case errors.Is(err, ErrSuspended):
		k.event(ctx, slog.LevelInfo, l.LinkID, "suspended", "state")
		delete(k.sched, l.PID.id)
	case errors.Is(err, ErrNotLinked):
		k.event(ctx, slog.LevelInfo, l.LinkID, "unlinked", "state")
		delete(k.sched, l.PID.id)
	case errors.Is(err, ErrIntegrity):
		k.fail(ctx, l, st, "integrity_error", "integrity")
	case errors.As(err, &ae):
		// The read itself was refused with a fresh token. The chain is not
		// shown dead (a refused refresh would be ErrNeedsRelink), so the
		// session is not touched; it is retried and reported.
		k.fail(ctx, l, st, "auth_rejected", "glovo_read_refused")
	default:
		// Busy lease, unavailable Vault or database, the keepalive's own
		// budget, proxy, network, deadline, 429 and 5xx: all transient. They
		// are retried with backoff and never read as a dead session.
		k.fail(ctx, l, st, "transient_error", transientReason(err))
	}
}

// transientReason is a fixed code for the log: never the error text, which
// could carry a piece of a dependency's response.
func transientReason(err error) string {
	switch {
	case errors.Is(err, ErrBusy):
		return "busy"
	case errors.Is(err, ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, ErrUnavailable):
		return "unavailable"
	case errors.Is(err, ErrSuperseded):
		return "superseded"
	case errors.Is(err, ErrSessionRefused):
		return "refresh_refused"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	default:
		return "other"
	}
}

// fail records a failure and schedules the retry: 5 minutes, doubling, never
// more than half an interval (and at most 30 minutes), with jitter.
func (k *Keepalive) fail(ctx context.Context, l ActiveLink, st *kaState, outcome, reason string) {
	st.failures++
	back := keepaliveBackoffBase << min(st.failures-1, 10)
	if limit := min(k.interval/2, 30*time.Minute); back > limit {
		back = limit
	}
	back = k.jittered(back, 2*keepaliveJitter)
	st.next = k.now().Add(back)
	level := slog.LevelInfo
	if st.failures >= 3 || outcome != "transient_error" {
		level = slog.LevelWarn
	}
	k.event(ctx, level, l.LinkID, outcome, reason, slog.Int("failures", st.failures), slog.Duration("retry_in", back))
}

// jittered returns d scaled by a factor in [1-frac, 1+frac).
func (k *Keepalive) jittered(d time.Duration, frac float64) time.Duration {
	return time.Duration(float64(d) * (1 + frac*(2*k.rand()-1)))
}
