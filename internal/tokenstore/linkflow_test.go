package tokenstore_test

// End-to-end tests of the link flow (design 4.1): the linkflow HTTP
// handlers over a real tokenstore.Store, with memDB and the fake Vault
// transit from this package's tests. They live here, as an external test
// package, because the fakes are test files of package tokenstore.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mctlhq/mctl-glovo/internal/authn"
	"github.com/mctlhq/mctl-glovo/internal/consentgate"
	"github.com/mctlhq/mctl-glovo/internal/glovoapi"
	"github.com/mctlhq/mctl-glovo/internal/linkflow"
	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

const (
	pidOwner = "prn_owner0000000000000000000"
	pidOther = "prn_other0000000000000000000"
)

type fakeVerifier struct{}

func (fakeVerifier) Verify(_ context.Context, tok string) (*authn.Principal, error) {
	switch tok {
	case "tok-owner":
		return &authn.Principal{ID: pidOwner}, nil
	case "tok-other":
		return &authn.Principal{ID: pidOther}, nil
	case "tok-jwks-down":
		return nil, &authn.Error{Reason: authn.ReasonJWKSDown, Status: http.StatusServiceUnavailable}
	case "tok-not-admitted":
		return nil, &authn.Error{Reason: authn.ReasonNotAdmitted, Status: http.StatusForbidden}
	case "tok-admit-down":
		return nil, &authn.Error{Reason: authn.ReasonAdmitDown, Status: http.StatusServiceUnavailable}
	case "tok-not-allowed":
		return nil, &authn.Error{Reason: "not_allowed", Status: http.StatusForbidden}
	}
	return nil, &authn.Error{Reason: "bad_token", Status: http.StatusUnauthorized}
}

// fakeLinkVerifier stands for the link-audience verifier, the one verifier of
// every token-bearing link route: it knows only fresh link tokens. An MCP token (tok-*) is the wrong audience here, a stale
// or auth_time-less one is a fresh-login failure. The real claim checks are
// authn's own tests; the wiring of the two verifiers is TestLinkRoutesUse...
type fakeLinkVerifier struct{ calls atomic.Int64 }

func (f *fakeLinkVerifier) Verify(_ context.Context, tok string) (*authn.Principal, error) {
	f.calls.Add(1)
	switch tok {
	case "link-owner":
		return &authn.Principal{ID: pidOwner}, nil
	case "link-other":
		return &authn.Principal{ID: pidOther}, nil
	case "link-stale", "link-no-auth-time":
		return nil, &authn.Error{Reason: authn.ReasonFreshLogin, Status: http.StatusUnauthorized}
	case "link-jwks-down":
		return nil, &authn.Error{Reason: authn.ReasonJWKSDown, Status: http.StatusServiceUnavailable}
	case "link-not-admitted":
		return nil, &authn.Error{Reason: authn.ReasonNotAdmitted, Status: http.StatusForbidden}
	case "link-admit-down":
		return nil, &authn.Error{Reason: authn.ReasonAdmitDown, Status: http.StatusServiceUnavailable}
	case "link-not-allowed":
		return nil, &authn.Error{Reason: "not_allowed", Status: http.StatusForbidden}
	}
	return nil, &authn.Error{Reason: authn.ReasonAudience, Status: http.StatusUnauthorized}
}

type fakeConsent struct {
	mu       sync.Mutex
	versions map[string]string
	err      error
}

func (c *fakeConsent) RecordedConsent(_ context.Context, pid tokenstore.PID) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return "", c.err
	}
	v, ok := c.versions[pid.String()]
	if !ok {
		return "", linkflow.ErrNoConsent
	}
	return v, nil
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type env struct {
	t       *testing.T
	h       *tokenstore.LinkHarness
	mux     *http.ServeMux
	consent *fakeConsent
	logs    *syncBuf
	linkV   *fakeLinkVerifier

	validations atomic.Int64
	failGlovo   atomic.Bool
	gate        chan struct{} // when set, validation waits on it

	// seen collects every response body and header, for the marker scan.
	seenMu sync.Mutex
	seen   []string
}

func defaultCaps() tokenstore.Caps { return tokenstore.DefaultCaps() }

func newEnv(t *testing.T, caps tokenstore.Caps) *env {
	t.Helper()
	e := &env{t: t, consent: &fakeConsent{versions: map[string]string{pidOwner: linkflow.ConsentVersion, pidOther: linkflow.ConsentVersion}}, logs: &syncBuf{}}
	validate := func(ctx context.Context, s glovoapi.Session) error {
		e.validations.Add(1)
		if e.gate != nil {
			<-e.gate
		}
		if e.failGlovo.Load() {
			// What a validator must never leak: a piece of the session.
			return errors.New("glovo said no to " + s["accessToken"].(string))
		}
		return nil
	}
	mask := func(_ context.Context, pid string) (string, error) {
		if pid == pidOwner {
			return "o***@e***.com", nil
		}
		return "x***@e***.com", nil
	}
	e.h = tokenstore.NewLinkHarness(t, caps, validate, mask)
	e.linkV = &fakeLinkVerifier{}
	lf, err := linkflow.New(linkflow.Config{
		Enabled:      true,
		Store:        e.h.Store,
		LinkVerifier: e.linkV,
		Consent:      e.consent,
		Log:          slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.mux = http.NewServeMux()
	lf.Register(e.mux)
	return e
}

type resp struct {
	status int
	body   map[string]string
	raw    string
}

func (e *env) do(req *http.Request) resp {
	e.t.Helper()
	if req.RemoteAddr == "" || req.RemoteAddr == "192.0.2.1:1234" {
		req.RemoteAddr = "203.0.113.7:4444"
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	raw := rec.Body.String()
	var hdr strings.Builder
	_ = rec.Header().Write(&hdr)
	e.seenMu.Lock()
	e.seen = append(e.seen, raw, hdr.String())
	e.seenMu.Unlock()
	r := resp{status: rec.Code, raw: raw}
	_ = json.Unmarshal(rec.Body.Bytes(), &r.body)
	return r
}

func (e *env) issue(tok string) string {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/link/code", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	r := e.do(req)
	if r.status != http.StatusOK || r.body["code"] == "" {
		e.t.Fatalf("issue: %d %s", r.status, r.raw)
	}
	return r.body["code"]
}

// jsonReq builds a JSON POST. The preview and upload routes carry the owner's
// fresh link token by default; a test about authentication replaces it.
func jsonReq(path string, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if path == "/link/preview" || path == "/link/upload" || path == "/link/unlink" {
		req.Header.Set("Authorization", "Bearer link-owner")
	}
	return req
}

func previewBody(code string) string {
	b, _ := json.Marshal(map[string]any{"schema": linkflow.PreviewSchema, "code": code})
	return string(b)
}

func uploadBody(code string, sess map[string]any) string {
	b, _ := json.Marshal(map[string]any{"schema": linkflow.UploadSchema, "code": code, "session": sess})
	return string(b)
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// A repeated key is refused on preview too, before the rate limit.
func TestLinkPreviewRefusesDuplicateKeys(t *testing.T) {
	e := newEnv(t, defaultCaps())
	code := e.issue("link-owner")
	body := `{"schema":"` + linkflow.PreviewSchema + `","code":"wrong","code":"` + code + `"}`
	wantRefusal(t, e.do(jsonReq("/link/preview", body)), 400, "malformed_request")
}

func (e *env) preview(code string) resp { return e.do(jsonReq("/link/preview", previewBody(code))) }

func (e *env) upload(code string, sess map[string]any) resp {
	return e.do(jsonReq("/link/upload", uploadBody(code, sess)))
}

func session(tag string) map[string]any {
	return map[string]any{
		"accessToken":  "access-" + tag,
		"refreshToken": "refresh-" + tag,
		"deviceUrn":    "urn:device:" + tag,
		"customerId":   "cust-" + tag,
	}
}

func wantRefusal(t *testing.T, r resp, status int, code string) {
	t.Helper()
	if r.status != status || r.body["error"] != code {
		t.Fatalf("got %d %s, want %d %s", r.status, r.raw, status, code)
	}
}

// --- flag ----------------------------------------------------------------

// Off means no route at all, and no store is needed to say so.
func TestLinkFlagOffIs404(t *testing.T) {
	lf, err := linkflow.New(linkflow.Config{Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	lf.Register(mux)
	for _, rt := range []struct{ method, path string }{
		{"GET", "/link"}, {"POST", "/link/code"}, {"POST", "/link/preview"}, {"POST", "/link/upload"},
	} {
		req := httptest.NewRequest(rt.method, rt.path, strings.NewReader(previewBody("x")))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer link-owner")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d with the flag off", rt.method, rt.path, rec.Code)
		}
	}
	if _, err := linkflow.New(linkflow.Config{Enabled: true}); err == nil {
		t.Error("enabled without a store accepted")
	}
}

// --- the code ------------------------------------------------------------

func TestLinkHappyPathAndRelink(t *testing.T) {
	e := newEnv(t, defaultCaps())
	code := e.issue("link-owner")
	if r := e.preview(code); r.status != 200 || r.body["identity"] != "o***@e***.com" {
		t.Fatalf("preview: %d %s", r.status, r.raw)
	}
	// Preview does not burn the code.
	if r := e.upload(code, session("one")); r.status != 200 || r.body["result"] != "linked" || len(r.body) != 1 {
		t.Fatalf("upload: %d %s", r.status, r.raw)
	}
	row1, consent, ok := e.h.LinkRow(pidOwner)
	if !ok || consent != linkflow.ConsentVersion {
		t.Fatalf("row %v consent %q", ok, consent)
	}
	if got := e.h.Stored(pidOwner)["refreshToken"]; got != "refresh-one" {
		t.Fatalf("stored %v", got)
	}
	// A relink replaces the link: a new link_id and a new version.
	code2 := e.issue("link-owner")
	if r := e.upload(code2, session("two")); r.status != 200 {
		t.Fatalf("relink: %d %s", r.status, r.raw)
	}
	row2, _, _ := e.h.LinkRow(pidOwner)
	if row2.LinkID == row1.LinkID || row2.Version <= row1.Version {
		t.Fatalf("relink kept link_id/version: %s/%d -> %s/%d", row1.LinkID, row1.Version, row2.LinkID, row2.Version)
	}
	if got := e.h.Stored(pidOwner)["refreshToken"]; got != "refresh-two" {
		t.Fatalf("relink stored %v", got)
	}
}

func TestLinkPageShowsConsentAndCodeOnce(t *testing.T) {
	e := newEnv(t, defaultCaps())
	r := e.do(httptest.NewRequest(http.MethodGet, "/link", nil))
	if r.status != 200 || !strings.Contains(r.raw, linkflow.ConsentVersion) || strings.Contains(r.raw, "<script") {
		t.Fatalf("page: %d", r.status)
	}
	req := httptest.NewRequest(http.MethodPost, "/link/code", nil)
	req.Header.Set("Authorization", "Bearer link-owner")
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, linkflow.ConsentVersion) || strings.Contains(body, "<script") {
		t.Fatalf("code page: %d", rec.Code)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("code page is cacheable")
	}
	if e.h.Codes() != 1 {
		t.Fatalf("%d codes", e.h.Codes())
	}
}

func TestLinkNoConsentNoCode(t *testing.T) {
	cases := []struct {
		name    string
		set     func(c *fakeConsent)
		status  int
		errCode string
	}{
		{"no record", func(c *fakeConsent) { delete(c.versions, pidOwner) }, 403, "consent_required"},
		{"another text", func(c *fakeConsent) { c.versions[pidOwner] = "c2-link-v0" }, 403, "consent_required"},
		{"unreadable", func(c *fakeConsent) { c.err = errors.New("mctl-api down") }, 503, "unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, defaultCaps())
			tc.set(e.consent)
			req := httptest.NewRequest(http.MethodPost, "/link/code", nil)
			req.Header.Set("Authorization", "Bearer link-owner")
			wantRefusal(t, e.do(req), tc.status, tc.errCode)
			if e.h.Codes() != 0 {
				t.Fatalf("%d codes without consent", e.h.Codes())
			}
		})
	}
	// The production source until C2-1c never reports a consent.
	t.Run("unavailable source", func(t *testing.T) {
		e := newEnv(t, defaultCaps())
		lf, err := linkflow.New(linkflow.Config{Enabled: true, Store: e.h.Store, Consent: linkflow.UnavailableConsent{}})
		if err != nil {
			t.Fatal(err)
		}
		e.mux = http.NewServeMux()
		lf.Register(e.mux)
		req := httptest.NewRequest(http.MethodPost, "/link/code", nil)
		req.Header.Set("Authorization", "Bearer link-owner")
		wantRefusal(t, e.do(req), 503, "unavailable")
		if e.h.Codes() != 0 {
			t.Fatalf("%d codes", e.h.Codes())
		}
	})
}

func TestLinkCodeNeedsAVerifiedToken(t *testing.T) {
	e := newEnv(t, defaultCaps())
	for _, tc := range []struct {
		auth, query string
		status      int
		code        string
	}{
		{"", "", 401, "invalid_token"},
		{"Basic abc", "", 401, "invalid_token"},
		{"Bearer link-forged", "", 401, "invalid_token"},
		{"Bearer link-jwks-down", "", 503, "temporarily_unavailable"},
		{"Bearer link-not-allowed", "", 403, "access_denied"},
		{"Bearer link-owner", "?access_token=link-owner", 401, "invalid_token"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/link/code"+tc.query, nil)
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		wantRefusal(t, e.do(req), tc.status, tc.code)
	}
	if e.h.Codes() != 0 {
		t.Fatalf("%d codes", e.h.Codes())
	}
}

// POST /link/code takes the fresh link-audience token and nothing else (the
// helper requests its own code with the token it just logged in with). Every
// refusal mints nothing.
func TestLinkCodeTokenAndPreconditions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		token  string
		setup  func(e *env)
		status int
		code   string
	}{
		{"fresh link token", "link-owner", nil, 200, ""},
		{"MCP-audience token", "tok-owner", nil, 401, "invalid_token"},
		{"stale login", "link-stale", nil, 401, "fresh_login_required"},
		{"no auth_time", "link-no-auth-time", nil, 401, "fresh_login_required"},
		{"no token", "", nil, 401, "invalid_token"},
		{"no consent", "link-owner", func(e *env) { delete(e.consent.versions, pidOwner) }, 403, "consent_required"},
		{"killed", "link-owner", func(e *env) { e.h.SetKilled(true) }, 503, "linking_disabled"},
		{"rate limited", "link-owner", func(e *env) { e.issue("link-owner") }, 429, "rate_limited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caps := defaultCaps()
			if tc.code == "rate_limited" {
				caps.LinkAttemptsPerIPHour = 1 // spent by the setup's own request
			}
			e := newEnv(t, caps)
			if tc.setup != nil {
				tc.setup(e)
			}
			before := e.h.Codes()
			req := httptest.NewRequest(http.MethodPost, "/link/code", nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			r := e.do(req)
			if tc.code == "" {
				if r.status != 200 || r.body["code"] == "" || r.body["consent_version"] != linkflow.ConsentVersion {
					t.Fatalf("got %d %s", r.status, r.raw)
				}
				// The code is for the pid of the link token: its own preview works.
				if p := e.preview(r.body["code"]); p.status != 200 || p.body["identity"] != "o***@e***.com" {
					t.Fatalf("preview of the issued code: %d %s", p.status, p.raw)
				}
				return
			}
			wantRefusal(t, r, tc.status, tc.code)
			if e.h.Codes() != before {
				t.Fatalf("%d codes minted by a refused request", e.h.Codes()-before)
			}
		})
	}
}

func TestLinkExpiredCode(t *testing.T) {
	e := newEnv(t, defaultCaps())
	code := e.issue("link-owner")
	e.h.ExpireCodes()
	wantRefusal(t, e.preview(code), 400, "code_refused")
	wantRefusal(t, e.upload(code, session("x")), 400, "code_refused")
	if e.validations.Load() != 0 {
		t.Fatal("expired code reached Glovo")
	}
}

func TestLinkNewCodeInvalidatesOld(t *testing.T) {
	e := newEnv(t, defaultCaps())
	old := e.issue("link-owner")
	fresh := e.issue("link-owner")
	wantRefusal(t, e.preview(old), 400, "code_refused")
	wantRefusal(t, e.upload(old, session("x")), 400, "code_refused")
	if r := e.upload(fresh, session("y")); r.status != 200 {
		t.Fatalf("fresh code: %d %s", r.status, r.raw)
	}
}

// Two uploads with one code: exactly one wins.
func TestLinkCodeIsSingleUseUnderConcurrency(t *testing.T) {
	e := newEnv(t, defaultCaps())
	code := e.issue("link-owner")
	e.gate = make(chan struct{})
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]resp, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = e.upload(code, session("c"))
		}()
	}
	close(start)
	// Let the winner sit in validation until the loser has its answer.
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for e.validations.Load() == 0 && time.Now().Before(deadline) {
			runtime.Gosched()
		}
		close(e.gate)
	}()
	wg.Wait()
	ok, refused := 0, 0
	for _, r := range results {
		switch {
		case r.status == 200:
			ok++
		case r.status == 400 && r.body["error"] == "code_refused":
			refused++
		default:
			t.Fatalf("unexpected %d %s", r.status, r.raw)
		}
	}
	if ok != 1 || refused != 1 || e.validations.Load() != 1 {
		t.Fatalf("ok=%d refused=%d validations=%d", ok, refused, e.validations.Load())
	}
}

// --- step 4, every refusal in order ---------------------------------------

func TestLinkUploadBodyChecksComeFirst(t *testing.T) {
	e := newEnv(t, defaultCaps())
	code := e.issue("link-owner")
	big := session("big")
	big["padding"] = strings.Repeat("p", linkflow.MaxBody)
	cases := []struct {
		name, body, ctype string
		status            int
		errCode           string
	}{
		{"over 16 KiB", uploadBody(code, big), "application/json", 413, "request_too_large"},
		{"trailing JSON", uploadBody(code, session("t")) + `{}`, "application/json", 400, "malformed_request"},
		{"trailing garbage", uploadBody(code, session("t")) + ` x`, "application/json", 400, "malformed_request"},
		{"not JSON", `schema=1`, "application/json", 400, "malformed_request"},
		{"wrong media type", uploadBody(code, session("t")), "text/plain", 400, "malformed_request"},
		{"unknown key", `{"schema":"` + linkflow.UploadSchema + `","code":"` + code + `","session":{},"extra":1}`, "application/json", 400, "malformed_request"},
		{"key case", `{"Schema":"` + linkflow.UploadSchema + `","code":"` + code + `","session":{}}`, "application/json", 400, "malformed_request"},
		{"missing session", `{"schema":"` + linkflow.UploadSchema + `","code":"` + code + `"}`, "application/json", 400, "malformed_request"},
		{"wrong schema", `{"schema":"glovo-link-upload/v2","code":"` + code + `","session":{"a":"b"}}`, "application/json", 400, "malformed_request"},
		{"code not a string", `{"schema":"` + linkflow.UploadSchema + `","code":1,"session":{"a":"b"}}`, "application/json", 400, "malformed_request"},
		{"session not an object", `{"schema":"` + linkflow.UploadSchema + `","code":"` + code + `","session":"s"}`, "application/json", 400, "malformed_request"},
		{"array", `[]`, "application/json", 400, "malformed_request"},
		{"duplicate code key", `{"schema":"` + linkflow.UploadSchema + `","code":"` + code + `","code":"` + code + `","session":` + mustJSON(session("dup")) + `}`, "application/json", 400, "malformed_request"},
		{"duplicate schema key", `{"schema":"x","schema":"` + linkflow.UploadSchema + `","code":"` + code + `","session":` + mustJSON(session("dup")) + `}`, "application/json", 400, "malformed_request"},
		{"unclosed object", `{"schema":"` + linkflow.UploadSchema + `","code":"` + code + `","session":` + mustJSON(session("open")), "application/json", 400, "malformed_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/link/upload", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.ctype)
			req.Header.Set("Authorization", "Bearer link-owner")
			wantRefusal(t, e.do(req), tc.status, tc.errCode)
		})
	}
	// None of them reached the store: the code is still live.
	if e.validations.Load() != 0 {
		t.Fatal("a malformed body reached Glovo")
	}
	if r := e.preview(code); r.status != 200 {
		t.Fatalf("code burned by a malformed body: %d %s", r.status, r.raw)
	}
}

func TestLinkRateLimits(t *testing.T) {
	caps := defaultCaps()
	caps.LinkAttemptsPerIPHour = 3
	caps.LinkAttemptsGlobalHour = 5
	e := newEnv(t, caps)
	code := e.issue("link-owner") // attempt 1 from 203.0.113.7
	for i := 0; i < 2; i++ {
		if r := e.preview(code); r.status != 200 {
			t.Fatalf("preview %d: %d", i, r.status)
		}
	}
	// The per-IP cap comes before the kill switch and the redemption.
	e.h.SetKilled(true)
	wantRefusal(t, e.upload(code, session("r")), 429, "rate_limited")
	wantRefusal(t, e.preview(code), 429, "rate_limited")
	e.h.SetKilled(false)
	// The global cap counts every IP.
	other := func(ip string) resp {
		req := jsonReq("/link/preview", previewBody(code))
		req.RemoteAddr = ip + ":1"
		return e.do(req)
	}
	if r := other("198.51.100.1"); r.status != 200 {
		t.Fatalf("other ip: %d", r.status)
	}
	if r := other("198.51.100.2"); r.status != 200 {
		t.Fatalf("other ip 2: %d", r.status)
	}
	wantRefusal(t, other("198.51.100.3"), 429, "rate_limited")
	if e.validations.Load() != 0 {
		t.Fatal("a rate-limited upload reached Glovo")
	}
}

// The kill switch refuses before the code is redeemed: the code survives.
func TestLinkKillSwitchRefusesBeforeRedemption(t *testing.T) {
	e := newEnv(t, defaultCaps())
	code := e.issue("link-owner")
	vault := e.h.VaultCalls()
	e.h.SetKilled(true)
	wantRefusal(t, e.upload(code, session("k")), 503, "linking_disabled")
	wantRefusal(t, e.preview(code), 503, "linking_disabled")
	req := httptest.NewRequest(http.MethodPost, "/link/code", nil)
	req.Header.Set("Authorization", "Bearer link-owner")
	wantRefusal(t, e.do(req), 503, "linking_disabled")
	if e.validations.Load() != 0 || e.h.VaultCalls() != vault {
		t.Fatalf("killed upload reached Glovo (%d) or Vault (%d)", e.validations.Load(), e.h.VaultCalls()-vault)
	}
	e.h.SetKilled(false)
	if r := e.upload(code, session("k")); r.status != 200 {
		t.Fatalf("code did not survive the kill: %d %s", r.status, r.raw)
	}
}

func TestLinkUnknownCode(t *testing.T) {
	e := newEnv(t, defaultCaps())
	for _, c := range []string{"not-a-code", strings.Repeat("A", 43)} {
		wantRefusal(t, e.upload(c, session("u")), 400, "code_refused")
		wantRefusal(t, e.preview(c), 400, "code_refused")
	}
	if e.validations.Load() != 0 {
		t.Fatal("unknown code reached Glovo")
	}
}

// A session without the required fields is refused after the redemption,
// before Glovo, and the code is burned.
func TestLinkSessionShapeBurnsCode(t *testing.T) {
	for _, field := range []string{"accessToken", "refreshToken", "deviceUrn"} {
		t.Run(field, func(t *testing.T) {
			e := newEnv(t, defaultCaps())
			code := e.issue("link-owner")
			s := session("s")
			delete(s, field)
			wantRefusal(t, e.upload(code, s), 422, "session_refused")
			if e.validations.Load() != 0 {
				t.Fatal("malformed session reached Glovo")
			}
			if _, _, ok := e.h.LinkRow(pidOwner); ok {
				t.Fatal("malformed session stored")
			}
			wantRefusal(t, e.upload(code, session("s")), 400, "code_refused")
		})
	}
	e := newEnv(t, defaultCaps())
	code := e.issue("link-owner")
	s := session("s")
	s["accessToken"] = strings.Repeat("a", 9<<10)
	wantRefusal(t, e.upload(code, s), 422, "session_refused")
}

// Glovo refusing the session: nothing stored, nothing encrypted, and the
// code is still burned.
func TestLinkGlovoValidationFailureStoresNothing(t *testing.T) {
	e := newEnv(t, defaultCaps())
	code := e.issue("link-owner")
	vault := e.h.VaultCalls()
	e.failGlovo.Store(true)
	wantRefusal(t, e.upload(code, session("g")), 422, "session_refused")
	if e.validations.Load() != 1 {
		t.Fatalf("%d validations", e.validations.Load())
	}
	if _, _, ok := e.h.LinkRow(pidOwner); ok {
		t.Fatal("refused session stored")
	}
	if e.h.VaultCalls() != vault {
		t.Fatal("refused session was encrypted")
	}
	e.failGlovo.Store(false)
	wantRefusal(t, e.upload(code, session("g")), 400, "code_refused")
}

// A relink keeps the old link until the new one is validated.
func TestLinkFailedRelinkKeepsOldLink(t *testing.T) {
	e := newEnv(t, defaultCaps())
	if r := e.upload(e.issue("link-owner"), session("a")); r.status != 200 {
		t.Fatal(r.raw)
	}
	before, _, _ := e.h.LinkRow(pidOwner)
	e.failGlovo.Store(true)
	wantRefusal(t, e.upload(e.issue("link-owner"), session("b")), 422, "session_refused")
	after, _, _ := e.h.LinkRow(pidOwner)
	if after.LinkID != before.LinkID || after.Version != before.Version {
		t.Fatal("a refused relink changed the link")
	}
}

// --- redaction -----------------------------------------------------------

// Every path, driven with a session whose values are unique markers: no
// marker and no code may appear in a response, a header, a log line of
// either package, or the audit.
func TestLinkRedactionMarkerScan(t *testing.T) {
	caps := defaultCaps()
	caps.LinkAttemptsPerIPHour, caps.LinkAttemptsGlobalHour = 100, 100
	e := newEnv(t, caps)
	marker := func(tag string) map[string]any {
		return map[string]any{
			"accessToken":  "MARKER-ACCESS-" + tag,
			"refreshToken": "MARKER-REFRESH-" + tag,
			"deviceUrn":    "MARKER-DEVICE-" + tag,
			"customerId":   "MARKER-CUSTOMER-" + tag,
			"location":     map[string]any{"street": "MARKER-STREET-" + tag},
		}
	}
	var codes []string
	issue := func() string { c := e.issue("link-owner"); codes = append(codes, c); return c }

	// Success.
	e.upload(issue(), marker("ok"))
	// Glovo refuses (the validator's error carries the access token).
	e.failGlovo.Store(true)
	e.upload(issue(), marker("glovo"))
	e.failGlovo.Store(false)
	// Shape refused.
	s := marker("shape")
	s["refreshToken"] = map[string]any{"nested": "MARKER-NESTED-shape"}
	e.upload(issue(), s)
	// Malformed bodies carrying markers.
	c := issue()
	body := uploadBody(c, marker("trail"))
	e.do(jsonReq("/link/upload", body+`{"MARKER-TRAIL":1}`))
	e.do(jsonReq("/link/upload", `{"schema":"MARKER-SCHEMA","code":"`+c+`","session":`+`{"accessToken":"MARKER-ACCESS-bad"}}`))
	big := marker("big")
	big["pad"] = strings.Repeat("MARKER-PAD-", 2000)
	e.do(jsonReq("/link/upload", uploadBody(c, big)))
	// Used and unknown codes.
	e.upload(codes[0], marker("reuse"))
	e.upload("MARKER-CODE", marker("unknown"))
	e.preview("MARKER-CODE")
	// Killed.
	kc := issue()
	e.h.SetKilled(true)
	e.upload(kc, marker("killed"))
	e.h.SetKilled(false)

	e.seenMu.Lock()
	seen := strings.Join(e.seen, "\n")
	e.seenMu.Unlock()
	logs := e.logs.String() + "\n" + e.h.Logs() + "\n" + strings.Join(e.h.AuditEvents(), "\n")
	if strings.Contains(seen, "MARKER") {
		t.Errorf("a response carries a marker")
	}
	if strings.Contains(logs, "MARKER") {
		t.Errorf("a log line or audit row carries a marker")
	}
	for _, code := range codes {
		if strings.Contains(logs, code) {
			t.Errorf("a log line carries a link code")
		}
	}
	if !strings.Contains(e.logs.String(), `"outcome":"linked"`) {
		t.Fatal("the scan saw no log output; it would pass vacuously")
	}
}

// The response to an upload is "linked" and nothing else.
func TestLinkUploadAnswerCarriesNoSessionData(t *testing.T) {
	e := newEnv(t, defaultCaps())
	r := e.upload(e.issue("link-owner"), session("q"))
	if r.raw != `{"result":"linked"}`+"\n" {
		t.Fatalf("answer %q", r.raw)
	}
}

func TestLinkPreviewMaskUnavailable(t *testing.T) {
	e := newEnv(t, defaultCaps())
	h := tokenstore.NewLinkHarness(t, defaultCaps(), func(context.Context, glovoapi.Session) error { return nil }, nil)
	lf, err := linkflow.New(linkflow.Config{Enabled: true, Store: h.Store, LinkVerifier: e.linkV, Consent: e.consent})
	if err != nil {
		t.Fatal(err)
	}
	e.mux = http.NewServeMux()
	lf.Register(e.mux)
	code := e.issue("link-owner")
	wantRefusal(t, e.preview(code), 503, "unavailable")
	// Without a preview the helper never uploads, but the code itself is
	// still good: the store did not burn it.
	if r := e.upload(code, session("m")); r.status != 200 {
		t.Fatalf("%d %s", r.status, r.raw)
	}
}

// The upload re-checks the consent through the gate after the code is
// redeemed: a consent withdrawn after the code was issued stores nothing,
// and a consent that cannot be read refuses, never links.
func TestLinkUploadRechecksConsent(t *testing.T) {
	setup := func(t *testing.T) (*env, string) {
		e := newEnv(t, defaultCaps())
		g, err := consentgate.New(consentgate.Config{Source: e.consent, Version: linkflow.ConsentVersion})
		if err != nil {
			t.Fatal(err)
		}
		e.h.LinkConsent = g.LinkConsent
		code := e.issue("link-owner")
		return e, code
	}
	t.Run("held", func(t *testing.T) {
		e, code := setup(t)
		if r := e.upload(code, session("one")); r.status != 200 {
			t.Fatalf("%d %s", r.status, r.raw)
		}
	})
	t.Run("withdrawn after the code", func(t *testing.T) {
		e, code := setup(t)
		e.consent.mu.Lock()
		delete(e.consent.versions, pidOwner)
		e.consent.mu.Unlock()
		wantRefusal(t, e.upload(code, session("one")), 403, "consent_required")
		if _, _, ok := e.h.LinkRow(pidOwner); ok || e.validations.Load() != 0 {
			t.Fatalf("linked or validated without consent (validations %d)", e.validations.Load())
		}
		// The code is burned: consenting again needs a new code.
		e.consent.mu.Lock()
		e.consent.versions[pidOwner] = linkflow.ConsentVersion
		e.consent.mu.Unlock()
		wantRefusal(t, e.upload(code, session("one")), 400, "code_refused")
	})
	t.Run("unreadable", func(t *testing.T) {
		e, code := setup(t)
		e.consent.mu.Lock()
		e.consent.err = errors.New("mctl-api down")
		e.consent.mu.Unlock()
		wantRefusal(t, e.upload(code, session("one")), 503, "unavailable")
		if _, _, ok := e.h.LinkRow(pidOwner); ok || e.validations.Load() != 0 {
			t.Fatal("linked on a consent read error")
		}
	})
}

// The link verifier's consent gate answers in the link flow's own terms.
func TestLinkCodeAdmissionRefusals(t *testing.T) {
	e := newEnv(t, defaultCaps())
	for tok, want := range map[string]struct {
		status int
		code   string
	}{
		"link-not-admitted": {403, "consent_required"},
		"link-admit-down":   {503, "temporarily_unavailable"},
		"link-not-allowed":  {403, "access_denied"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/link/code", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		wantRefusal(t, e.do(req), want.status, want.code)
	}
	if e.h.Codes() != 0 {
		t.Fatal("code issued")
	}
}

// --- fresh login and principal binding (mctl-glovo#42) ---------------------

// previewAndUploadWith sends both routes with the given Authorization value
// ("" sends none) and returns both answers.
func (e *env) previewAndUploadWith(code, auth string) (resp, resp) {
	e.t.Helper()
	pv := jsonReq("/link/preview", previewBody(code))
	up := jsonReq("/link/upload", uploadBody(code, session("x")))
	for _, r := range []*http.Request{pv, up} {
		r.Header.Del("Authorization")
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
	}
	return e.do(pv), e.do(up)
}

// Preview and upload take a fresh LINK token, nothing else: not no token, not
// an MCP-audience token (a connector token, a refresh-derived or GitHub-
// derived MCTL token all look like one), not a stale or auth_time-less link
// token, not a token in the query. Every refusal leaves the code usable.
func TestLinkPreviewAndUploadNeedAFreshLinkToken(t *testing.T) {
	for _, tc := range []struct {
		name, auth, query string
		status            int
		code              string
	}{
		{"no token", "", "", 401, "invalid_token"},
		{"not a bearer", "Basic abc", "", 401, "invalid_token"},
		{"the code alone is not enough", "", "", 401, "invalid_token"},
		{"MCP-audience token of the owner", "Bearer tok-owner", "", 401, "invalid_token"},
		{"forged", "Bearer link-forged", "", 401, "invalid_token"},
		{"stale login", "Bearer link-stale", "", 401, "fresh_login_required"},
		{"no auth_time", "Bearer link-no-auth-time", "", 401, "fresh_login_required"},
		{"keys unreachable", "Bearer link-jwks-down", "", 503, "temporarily_unavailable"},
		{"token in the query", "Bearer link-owner", "?access_token=link-owner", 401, "invalid_token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, defaultCaps())
			code := e.issue("link-owner")
			for _, path := range []string{"/link/preview", "/link/upload"} {
				body := previewBody(code)
				if path == "/link/upload" {
					body = uploadBody(code, session("x"))
				}
				req := httptest.NewRequest(http.MethodPost, path+tc.query, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				if tc.auth != "" {
					req.Header.Set("Authorization", tc.auth)
				}
				wantRefusal(t, e.do(req), tc.status, tc.code)
			}
			if e.validations.Load() != 0 {
				t.Fatal("a refused token reached Glovo")
			}
			if _, _, ok := e.h.LinkRow(pidOwner); ok {
				t.Fatal("a refused token produced a link")
			}
			// The code was not touched: a good fresh token still uses it.
			if r := e.preview(code); r.status != 200 {
				t.Fatalf("code burned by a refused token: %d %s", r.status, r.raw)
			}
		})
	}
}

// The link audience is the only audience: no link route falls back to an
// MCP-audience token, and a missing link verifier refuses the code route too.
func TestLinkRoutesUseTheirOwnVerifier(t *testing.T) {
	e := newEnv(t, defaultCaps())
	code := e.issue("link-owner")
	// An MCP-audience token cannot mint a code.
	req := httptest.NewRequest(http.MethodPost, "/link/code", nil)
	req.Header.Set("Authorization", "Bearer tok-owner")
	wantRefusal(t, e.do(req), 401, "invalid_token")
	// A missing link verifier refuses; it never falls back to the MCP one.
	lf, err := linkflow.New(linkflow.Config{Enabled: true, Store: e.h.Store, Consent: e.consent})
	if err != nil {
		t.Fatal(err)
	}
	e.mux = http.NewServeMux()
	lf.Register(e.mux)
	codes := e.h.Codes()
	for _, tok := range []string{"link-owner", "tok-owner"} {
		pv, up := e.previewAndUploadWith(code, "Bearer "+tok)
		wantRefusal(t, pv, 503, "unavailable")
		wantRefusal(t, up, 503, "unavailable")
		req := httptest.NewRequest(http.MethodPost, "/link/code", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		wantRefusal(t, e.do(req), 503, "unavailable")
	}
	if e.validations.Load() != 0 || e.h.Codes() != codes {
		t.Fatal("reached Glovo or minted a code without a link verifier")
	}
}

// A code is for the principal it was issued to. Another principal's FRESH,
// valid link token gets the unknown-code answer, sees no identity, stores
// nothing and does not burn the owner's code.
func TestLinkCodeOfAnotherPrincipalIsRefused(t *testing.T) {
	e := newEnv(t, defaultCaps())
	code := e.issue("link-owner")
	pv, up := e.previewAndUploadWith(code, "Bearer link-other")
	wantRefusal(t, pv, 400, "code_refused")
	wantRefusal(t, up, 400, "code_refused")
	if strings.Contains(pv.raw, "***") {
		t.Fatalf("a stranger was shown an identity: %s", pv.raw)
	}
	if _, _, ok := e.h.LinkRow(pidOther); ok {
		t.Fatal("a link was stored for the stranger")
	}
	if _, _, ok := e.h.LinkRow(pidOwner); ok || e.validations.Load() != 0 {
		t.Fatal("the stranger's upload linked or reached Glovo")
	}
	if r := e.preview(code); r.status != 200 || r.body["identity"] != "o***@e***.com" {
		t.Fatalf("the owner's preview after the attempt: %d %s", r.status, r.raw)
	}
	if r := e.upload(code, session("mine")); r.status != 200 {
		t.Fatalf("the owner's upload after the attempt: %d %s", r.status, r.raw)
	}
	if got := e.h.Stored(pidOwner)["refreshToken"]; got != "refresh-mine" {
		t.Fatalf("stored %v", got)
	}
}

// A code works once, and a replay by the owner changes nothing.
func TestLinkReplayIsRefused(t *testing.T) {
	e := newEnv(t, defaultCaps())
	code := e.issue("link-owner")
	if r := e.upload(code, session("one")); r.status != 200 {
		t.Fatalf("%d %s", r.status, r.raw)
	}
	wantRefusal(t, e.upload(code, session("two")), 400, "code_refused")
	wantRefusal(t, e.preview(code), 400, "code_refused")
	if got := e.h.Stored(pidOwner)["refreshToken"]; got != "refresh-one" {
		t.Fatalf("replay overwrote the link: %v", got)
	}
}

// The identity is shown only while the consent holds now; a consent that
// cannot be read shows nothing.
func TestLinkPreviewNeedsLiveConsent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		set    func(c *fakeConsent)
		status int
		code   string
	}{
		{"withdrawn", func(c *fakeConsent) { delete(c.versions, pidOwner) }, 403, "consent_required"},
		{"another text", func(c *fakeConsent) { c.versions[pidOwner] = "c2-link-v0" }, 403, "consent_required"},
		{"unreadable", func(c *fakeConsent) { c.err = errors.New("mctl-api down") }, 503, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, defaultCaps())
			code := e.issue("link-owner")
			e.consent.mu.Lock()
			tc.set(e.consent)
			e.consent.mu.Unlock()
			r := e.preview(code)
			wantRefusal(t, r, tc.status, tc.code)
			if strings.Contains(r.raw, "***") {
				t.Fatalf("identity shown without consent: %s", r.raw)
			}
		})
	}
}

// Consent withdrawn between the preview and the store, with a positive answer
// still cached by the gate: the upload reads the source again, refuses, and
// stores nothing. (The cache is what the token check uses; the upload must not.)
func TestLinkUploadIgnoresACachedConsent(t *testing.T) {
	e := newEnv(t, defaultCaps())
	g, err := consentgate.New(consentgate.Config{Source: e.consent, Version: linkflow.ConsentVersion})
	if err != nil {
		t.Fatal(err)
	}
	e.h.LinkConsent = g.LinkConsent
	code := e.issue("link-owner")
	if r := e.preview(code); r.status != 200 {
		t.Fatalf("preview: %d %s", r.status, r.raw)
	}
	if err := g.Admit(context.Background(), pidOwner); err != nil { // the cache is warm
		t.Fatal(err)
	}
	e.consent.mu.Lock()
	delete(e.consent.versions, pidOwner)
	e.consent.mu.Unlock()
	wantRefusal(t, e.upload(code, session("late")), 403, "consent_required")
	if _, _, ok := e.h.LinkRow(pidOwner); ok || e.validations.Load() != 0 {
		t.Fatalf("linked on a cached consent (validations %d)", e.validations.Load())
	}
}

// No refusal or answer of the new checks carries a token, a code, a pid or a
// session.
func TestLinkNewRefusalsLeakNothing(t *testing.T) {
	e := newEnv(t, defaultCaps())
	code := e.issue("link-owner")
	var seen []string
	for _, tok := range []string{"link-stale", "link-other", "tok-owner", "link-forged"} {
		pv, up := e.previewAndUploadWith(code, "Bearer "+tok)
		seen = append(seen, pv.raw, up.raw)
	}
	seen = append(seen, e.logs.String())
	for _, s := range seen {
		for _, secret := range []string{code, pidOwner, pidOther, "link-stale", "link-other", "tok-owner", "link-forged", "access-x"} {
			if strings.Contains(s, secret) {
				t.Fatalf("%q appears in %q", secret, s)
			}
		}
	}
}
