package glovotools

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mctlhq/mctl-glovo/internal/authn"
	"github.com/mctlhq/mctl-glovo/internal/childmcp"
	"github.com/mctlhq/mctl-glovo/internal/glovoapi"
	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

// Isolation harness for hosted mode (C2-2, design 9.5): one fake Glovo that
// records which access token each request carried, and a ClientSource with
// one distinguishable client per pid.

type seenReq struct{ auth, path, method string }

type fakeGlovoHosted struct {
	mu   sync.Mutex
	seen []seenReq
	srv  *httptest.Server
}

func newFakeGlovoHosted(t *testing.T) *fakeGlovoHosted {
	t.Helper()
	f := &fakeGlovoHosted{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, seenReq{r.Header.Get("Authorization"), r.URL.Path, r.Method})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGlovoHosted) auths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.seen))
	for i, s := range f.seen {
		out[i] = s.auth
	}
	return out
}

func (f *fakeGlovoHosted) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen)
}

// marker is a fake access token: a well-formed unexpired JWT whose
// signature part names the owner, so a request is attributable.
func marker(owner string) string {
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	pl := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":4102444800}`))
	return "Bearer " + hdr + "." + pl + "." + owner
}

type fakeSource struct {
	mu      sync.Mutex
	clients map[string]*glovoapi.Client
	err     error
	asked   []string
}

func (s *fakeSource) Client(_ context.Context, pid tokenstore.PID) (*glovoapi.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, pid.String())
	if s.err != nil {
		return nil, s.err
	}
	c, ok := s.clients[pid.String()]
	if !ok {
		return nil, tokenstore.ErrNotLinked
	}
	return c, nil
}

func (s *fakeSource) askedPIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.asked...)
}

func hostedClient(t *testing.T, base, owner string) *glovoapi.Client {
	t.Helper()
	return hostedClientGate(t, base, owner, func(context.Context) error { return nil })
}

func hostedClientGate(t *testing.T, base, owner string, gate func(context.Context) error, extra ...glovoapi.Option) *glovoapi.Client {
	t.Helper()
	noRefresh := func(context.Context, glovoapi.Session, func(context.Context, glovoapi.Session) (glovoapi.Session, error)) (glovoapi.Session, error) {
		return nil, errors.New("unexpected refresh")
	}
	opts := []glovoapi.Option{
		glovoapi.WithBaseURL(base),
		glovoapi.WithSession(glovoapi.Session{
			"accessToken":  marker(owner),
			"refreshToken": "refresh-" + owner,
			"customerId":   float64(1),
			"location":     map[string]any{"cityCode": "TST", "latitude": "1", "longitude": "2", "countryCode": "XX"},
		}),
		glovoapi.WithRefreshCoordinator(noRefresh),
		glovoapi.WithRequestGate(gate),
		glovoapi.WithClock(func() time.Time { return time.Unix(1_800_000_000, 0) }),
	}
	c, err := glovoapi.New("", append(opts, extra...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func pidCtx(id string) context.Context {
	return authn.WithPrincipal(context.Background(), &authn.Principal{ID: id})
}

type hostedEnv struct {
	b    *Backend
	fake *fakeGlovoHosted
	src  *fakeSource
}

func newHostedEnv(t *testing.T) *hostedEnv { return newHostedEnvWith(t, false) }

// newHostedEnvWith builds the hosted env with the basket write switch on or
// off: on enables it on the backend and on every client.
func newHostedEnvWith(t *testing.T, writes bool) *hostedEnv {
	t.Helper()
	fake := newFakeGlovoHosted(t)
	var extra []glovoapi.Option
	if writes {
		extra = append(extra, glovoapi.WithBasketWrites())
	}
	gate := func(context.Context) error { return nil }
	src := &fakeSource{clients: map[string]*glovoapi.Client{
		"prn_A": hostedClientGate(t, fake.srv.URL, "A", gate, extra...),
		"prn_B": hostedClientGate(t, fake.srv.URL, "B", gate, extra...),
	}}
	b, err := New(Config{Clients: src, BasketWrites: writes})
	if err != nil {
		t.Fatal(err)
	}
	return &hostedEnv{b: b, fake: fake, src: src}
}

const probeTool = "glovo_get_saved_locations" // makes one authenticated request

// Core invariant: each caller's calls run on that caller's own client.
func TestHostedCallsRunOnTheCallersOwnClient(t *testing.T) {
	e := newHostedEnv(t)
	for _, c := range []struct{ pid, owner string }{{"prn_A", "A"}, {"prn_B", "B"}, {"prn_A", "A"}} {
		before := e.fake.count()
		if _, err := e.b.Call(pidCtx(c.pid), probeTool, map[string]any{}); err != nil {
			t.Fatal(err)
		}
		got := e.fake.auths()[before:]
		if len(got) == 0 {
			t.Fatalf("%s: no request reached Glovo", c.pid)
		}
		for _, a := range got {
			if a != marker(c.owner) {
				t.Fatalf("%s: request carried %q, want the token of %s", c.pid, a, c.owner)
			}
		}
	}
}

// The pid comes from the verified principal only. Tool arguments, even ones
// named like a pid, must not select a session.
func TestHostedPIDNeverComesFromArguments(t *testing.T) {
	e := newHostedEnv(t)
	for _, key := range []string{"pid", "principal", "principal_id", "user", "user_id", "customer_id"} {
		before := len(e.src.askedPIDs())
		_, _ = e.b.Call(pidCtx("prn_A"), probeTool, map[string]any{key: "prn_B"})
		asked := e.src.askedPIDs()[before:]
		if len(asked) != 1 || asked[0] != "prn_A" {
			t.Fatalf("argument %q steered the lookup: asked %v", key, asked)
		}
	}
	for _, a := range e.fake.auths() {
		if a != marker("A") {
			t.Fatalf("a request carried %q while only A called", a)
		}
	}
}

// No verified principal: refused before any lookup or Glovo request, with
// no tool detail (not even argument validation) leaking to the caller.
func TestHostedRefusesWithoutAVerifiedPrincipal(t *testing.T) {
	e := newHostedEnv(t)
	bad := []context.Context{
		context.Background(),
		authn.WithPrincipal(context.Background(), nil),
		authn.WithPrincipal(context.Background(), &authn.Principal{}),
		authn.WithPrincipal(context.Background(), &authn.Principal{ID: "not-a-prn"}),
		authn.WithPrincipal(context.Background(), &authn.Principal{ID: "prn_A|prn_B"}),
	}
	for i, ctx := range bad {
		res, err := e.b.Call(ctx, probeTool, map[string]any{"match_text": 42}) // invalid args on purpose
		if err == nil || res != nil {
			t.Fatalf("case %d: got res=%v err=%v, want an error and no result", i, res, err)
		}
	}
	if n := len(e.src.askedPIDs()); n != 0 {
		t.Fatalf("source asked %d times without a principal", n)
	}
	if n := e.fake.count(); n != 0 {
		t.Fatalf("%d requests reached Glovo without a principal", n)
	}
}

// A failed lookup never falls through to another client, to a default, or to
// an empty success.
func TestHostedLookupFailuresRefuseAndNeverReachGlovo(t *testing.T) {
	cases := []struct {
		err      error
		wantErr  error  // returned error, when the call itself cannot run
		wantText string // isError text otherwise
	}{
		{tokenstore.ErrNotLinked, nil, "not linked"},
		{tokenstore.ErrNeedsRelink, nil, "Link your account again"},
		{tokenstore.ErrSuspended, nil, "suspended"},
		{tokenstore.ErrKilled, nil, "switched off"},
		{tokenstore.ErrRateLimited, nil, "rate limit"},
		{tokenstore.ErrBusy, nil, "busy"},
		{tokenstore.ErrSessionRefused, nil, "Link your account again"},
		{tokenstore.ErrUnavailable, childmcp.ErrUnavailable, ""},
		{tokenstore.ErrIntegrity, childmcp.ErrUnavailable, ""},
		{errors.New("postgres: password authentication failed for user x"), childmcp.ErrUnavailable, ""},
	}
	for _, c := range cases {
		e := newHostedEnv(t)
		e.src.err = c.err
		res, err := e.b.Call(pidCtx("prn_A"), probeTool, map[string]any{})
		if c.wantErr != nil {
			if !errors.Is(err, c.wantErr) || res != nil {
				t.Fatalf("%v: got res=%v err=%v, want %v", c.err, res, err, c.wantErr)
			}
		} else {
			if err != nil || res == nil || !res.IsError {
				t.Fatalf("%v: got res=%v err=%v, want an isError result", c.err, res, err)
			}
			if txt := resultText(res); !strings.Contains(txt, c.wantText) {
				t.Fatalf("%v: text %q lacks %q", c.err, txt, c.wantText)
			}
		}
		if n := e.fake.count(); n != 0 {
			t.Fatalf("%v: %d requests reached Glovo", c.err, n)
		}
		if err != nil && strings.Contains(err.Error(), "postgres") {
			t.Fatalf("dependency detail leaked: %v", err)
		}
	}
}

// A cancelled call is a context error, not a "not linked" answer.
func TestHostedCancelledLookupIsNotAnAnswer(t *testing.T) {
	e := newHostedEnv(t)
	e.src.err = context.Canceled
	ctx, cancel := context.WithCancel(pidCtx("prn_A"))
	cancel()
	res, err := e.b.Call(ctx, probeTool, map[string]any{})
	if res != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("got res=%v err=%v", res, err)
	}
}

// Concurrent callers never see each other's token (run with -race).
func TestHostedConcurrentCallersStayIsolated(t *testing.T) {
	e := newHostedEnv(t)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		pid, owner := "prn_A", "A"
		if i%2 == 1 {
			pid, owner = "prn_B", "B"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.b.Call(pidCtx(pid), probeTool, map[string]any{})
			_ = owner
		}()
	}
	wg.Wait()
	a, b := 0, 0
	for _, x := range e.fake.auths() {
		switch x {
		case marker("A"):
			a++
		case marker("B"):
			b++
		default:
			t.Fatalf("unknown token %q", x)
		}
	}
	if a != 20 || b != 20 {
		t.Fatalf("A=%d B=%d, want 20 each", a, b)
	}
}

// Hosted mode has no history cache directory: nothing is written to disk
// that two users could share.
func TestHostedHasNoHistoryCacheDir(t *testing.T) {
	e := newHostedEnv(t)
	if d := e.b.historyCache().Dir; d != "" {
		t.Fatalf("hosted history cache dir = %q, want empty (off)", d)
	}
}

func TestHostedConfigValidation(t *testing.T) {
	src := &fakeSource{}
	dir := t.TempDir()
	if _, err := New(Config{}); err == nil {
		t.Fatal("no SessionPath and no Clients must be refused")
	}
	if _, err := New(Config{Clients: src, SessionPath: filepath.Join(dir, "s.json")}); err == nil {
		t.Fatal("SessionPath and Clients together must be refused")
	}
	if _, err := New(Config{Clients: src, ClientOptions: []glovoapi.Option{glovoapi.WithBaseURL("http://x")}}); err == nil {
		t.Fatal("ClientOptions in hosted mode must be refused")
	}
	if _, err := os.Stat(filepath.Join(dir, "s.json")); err == nil {
		t.Fatal("construction wrote a session file")
	}
}

// Hosted mode serves the same tool set and keeps the no-checkout guard.
func TestHostedServesSameToolsAsFileMode(t *testing.T) {
	e := newHostedEnv(t)
	file := newHarness(t, loadSpec(t), caseSpec{})
	// Default hosted is read-only: the file-mode set minus the basket writes.
	// With the switch on it is the whole file-mode set.
	var wantNames []string
	for _, n := range file.backend.Names() {
		if !isBasketWriteTool(n) {
			wantNames = append(wantNames, n)
		}
	}
	got, want := strings.Join(e.b.Names(), ","), strings.Join(wantNames, ",")
	if on := newHostedEnvWith(t, true); strings.Join(on.b.Names(), ",") != strings.Join(file.backend.Names(), ",") {
		t.Fatalf("hosted with BasketWrites differs from file mode:\n%v", on.b.Names())
	}
	if got != want {
		t.Fatalf("tool sets differ:\n hosted: %s\n file:   %s", got, want)
	}
	for _, n := range e.b.Names() {
		if childmcp.Blocked(n) {
			t.Fatalf("blocked tool %q served", n)
		}
	}
}

// Sentinels that come back from inside the handler (request gate, refresh
// coordinator) get the same answers as at lookup time, and no internal
// dependency is named.
func TestHostedHandlerErrorsAreMappedLikeLookupErrors(t *testing.T) {
	cases := []struct {
		err      error
		wantErr  error
		wantText string
	}{
		{tokenstore.ErrUnavailable, childmcp.ErrUnavailable, ""},
		{tokenstore.ErrIntegrity, childmcp.ErrUnavailable, ""},
		{tokenstore.ErrSuperseded, childmcp.ErrUnavailable, ""},
		{tokenstore.ErrKilled, nil, "switched off"},
		{tokenstore.ErrRateLimited, nil, "rate limit"},
		{tokenstore.ErrNeedsRelink, nil, "Link your account again"},
		{tokenstore.ErrBusy, nil, "busy"},
		{tokenstore.ErrSessionRefused, nil, "Link your account again"},
		{fmt.Errorf("wrapped: %w", tokenstore.ErrRateLimited), nil, "rate limit"},
	}
	for _, c := range cases {
		fake := newFakeGlovoHosted(t)
		gateErr := c.err
		src := &fakeSource{clients: map[string]*glovoapi.Client{
			"prn_A": hostedClientGate(t, fake.srv.URL, "A", func(context.Context) error { return gateErr }),
		}}
		b, err := New(Config{Clients: src})
		if err != nil {
			t.Fatal(err)
		}
		res, err := b.Call(pidCtx("prn_A"), probeTool, map[string]any{})
		if c.wantErr != nil {
			if !errors.Is(err, c.wantErr) || res != nil {
				t.Fatalf("%v: got res=%v err=%v, want %v", c.err, res, err, c.wantErr)
			}
		} else {
			if err != nil || res == nil || !res.IsError || !strings.Contains(resultText(res), c.wantText) {
				t.Fatalf("%v: got res=%v err=%v, want isError text containing %q", c.err, res, err, c.wantText)
			}
			low := strings.ToLower(resultText(res))
			for _, leak := range []string{"token store", "tokenstore", "glovoapi"} {
				if strings.Contains(low, leak) {
					t.Fatalf("%v: text %q names %q", c.err, low, leak)
				}
			}
		}
		if n := fake.count(); n != 0 {
			t.Fatalf("%v: %d requests reached Glovo past the gate", c.err, n)
		}
	}
}

// A non-store handler error keeps its upstream text in hosted mode.
func TestHostedKeepsOtherHandlerErrorText(t *testing.T) {
	fake := newFakeGlovoHosted(t)
	src := &fakeSource{clients: map[string]*glovoapi.Client{
		"prn_A": hostedClientGate(t, fake.srv.URL, "A", func(context.Context) error { return errors.New("gate says no") }),
	}}
	b, err := New(Config{Clients: src})
	if err != nil {
		t.Fatal(err)
	}
	res, err := b.Call(pidCtx("prn_A"), probeTool, map[string]any{})
	if err != nil || res == nil || !res.IsError || !strings.Contains(resultText(res), "ate says no") {
		t.Fatalf("got res=%v err=%v", res, err)
	}
}

// A hosted client built without a session writer refuses a location change
// with a fixed text, without the package name; it never keeps an unsaved copy.
func TestHostedSetLocationWithoutWriterIsAFixedRefusal(t *testing.T) {
	e := newHostedEnv(t)
	res, err := e.b.Call(pidCtx("prn_A"), "glovo_set_location", map[string]any{
		"country_code": "XX", "city_code": "TST", "latitude": 1.5, "longitude": 2.5,
	})
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("got res=%v err=%v", res, err)
	}
	txt := resultText(res)
	if !strings.Contains(txt, "not available for this account") || strings.Contains(txt, "glovoapi") {
		t.Fatalf("text = %q", txt)
	}
	got, err := e.b.Call(pidCtx("prn_A"), "glovo_get_location", map[string]any{})
	if err != nil || strings.Contains(resultText(got), "1.5") {
		t.Fatalf("refused location leaked into the client: %v %v", got, err)
	}
}

// fakeSessionStore stands in for the token store row of one pid.
type fakeSessionStore struct {
	mu   sync.Mutex
	sess glovoapi.Session
	err  error
}

func (f *fakeSessionStore) write(_ context.Context, apply func(glovoapi.Session) (glovoapi.Session, error)) (glovoapi.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	next, err := apply(f.sess)
	if err != nil {
		return nil, err
	}
	f.sess = next
	return next, nil
}

func hostedWriterClient(t *testing.T, base, owner string, st *fakeSessionStore) *glovoapi.Client {
	t.Helper()
	c, err := glovoapi.New("",
		glovoapi.WithBaseURL(base),
		glovoapi.WithSession(st.sess),
		glovoapi.WithRefreshCoordinator(func(context.Context, glovoapi.Session, func(context.Context, glovoapi.Session) (glovoapi.Session, error)) (glovoapi.Session, error) {
			return nil, errors.New("unexpected refresh")
		}),
		glovoapi.WithSessionWriter(st.write),
		glovoapi.WithClock(func() time.Time { return time.Unix(1_800_000_000, 0) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func writerSession(owner string) glovoapi.Session {
	return glovoapi.Session{
		"accessToken": marker(owner), "refreshToken": "refresh-" + owner, "customerId": float64(1),
		"location": map[string]any{"cityCode": "OLD", "latitude": "1", "longitude": "2", "countryCode": "XX"},
	}
}

// glovo_set_location on a hosted client stores through the caller's own
// session writer: the other principal's location does not move.
func TestHostedSetLocationIsPerPID(t *testing.T) {
	fake := newFakeGlovoHosted(t)
	stA := &fakeSessionStore{sess: writerSession("A")}
	stB := &fakeSessionStore{sess: writerSession("B")}
	src := &fakeSource{clients: map[string]*glovoapi.Client{
		"prn_A": hostedWriterClient(t, fake.srv.URL, "A", stA),
		"prn_B": hostedWriterClient(t, fake.srv.URL, "B", stB),
	}}
	b, err := New(Config{Clients: src})
	if err != nil {
		t.Fatal(err)
	}
	res, err := b.Call(pidCtx("prn_A"), "glovo_set_location", map[string]any{
		"country_code": "ES", "city_code": "NEW", "latitude": 41.5, "longitude": 2.5,
	})
	if err != nil || res == nil || res.IsError {
		t.Fatalf("set: res=%v err=%v", res, err)
	}
	if txt := resultText(res); !strings.Contains(txt, "NEW") {
		t.Fatalf("result %q", txt)
	}
	if loc, _ := stA.sess["location"].(map[string]any); loc["cityCode"] != "NEW" {
		t.Fatalf("A stored %v", stA.sess["location"])
	}
	if loc, _ := stB.sess["location"].(map[string]any); loc["cityCode"] != "OLD" {
		t.Fatalf("B's stored location moved: %v", stB.sess["location"])
	}
	got, err := b.Call(pidCtx("prn_B"), "glovo_get_location", map[string]any{})
	if err != nil || !strings.Contains(resultText(got), "OLD") || strings.Contains(resultText(got), "NEW") {
		t.Fatalf("B reads %v %v", got, err)
	}
	if n := fake.count(); n != 0 {
		t.Fatalf("a location change made %d requests to Glovo", n)
	}
}

// A failed or refused store write is an error, never a success, and the
// client keeps its previous location. Store sentinels from a write map like
// any other hosted failure, ErrSessionRefused (the refresh coordinator can
// return it mid-call) included.
func TestHostedSetLocationStoreFailures(t *testing.T) {
	cases := []struct {
		name string
		err  error
		text string // substring of the isError text; empty = unavailable error
	}{
		{"refused", tokenstore.ErrLocationRefused, "not accepted"},
		{"killed", tokenstore.ErrKilled, "switched off"},
		{"session refused", tokenstore.ErrSessionRefused, "refused your session"},
		{"unavailable", tokenstore.ErrUnavailable, ""},
		{"integrity", tokenstore.ErrIntegrity, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := newFakeGlovoHosted(t)
			st := &fakeSessionStore{sess: writerSession("A"), err: c.err}
			src := &fakeSource{clients: map[string]*glovoapi.Client{"prn_A": hostedWriterClient(t, fake.srv.URL, "A", st)}}
			b, err := New(Config{Clients: src})
			if err != nil {
				t.Fatal(err)
			}
			res, err := b.Call(pidCtx("prn_A"), "glovo_set_location", map[string]any{
				"country_code": "ES", "city_code": "NEW", "latitude": 41.5, "longitude": 2.5,
			})
			if c.text == "" {
				if !errors.Is(err, childmcp.ErrUnavailable) {
					t.Fatalf("want unavailable, got res=%v err=%v", res, err)
				}
			} else if err != nil || res == nil || !res.IsError || !strings.Contains(resultText(res), c.text) {
				t.Fatalf("got res=%v err=%v", res, err)
			}
			got, _ := b.Call(pidCtx("prn_A"), "glovo_get_location", map[string]any{})
			if strings.Contains(resultText(got), "NEW") {
				t.Fatalf("a failed write changed the client: %s", resultText(got))
			}
		})
	}
}

func (f *fakeGlovoHosted) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.seen))
	for i, s := range f.seen {
		out[i] = s.method + " " + s.path
	}
	return out
}
