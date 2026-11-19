package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

const operatorTarget = "prn_target0000000000000000000"

type recordingEnder struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (r *recordingEnder) Unlink(_ context.Context, pid tokenstore.PID, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "unlink:"+pid.String()+":"+reason)
	return r.err
}

func (r *recordingEnder) Erase(_ context.Context, pid tokenstore.PID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "erase:"+pid.String())
	return r.err
}

func (r *recordingEnder) n() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

type accountEnv struct {
	mux   http.Handler
	ender *recordingEnder
	flip  *recordingFlipper
	logs  *syncBuf
	audit *syncBuf
}

func newAccountEnv(t *testing.T, withAccounts bool) *accountEnv {
	t.Helper()
	e := &accountEnv{ender: &recordingEnder{}, flip: &recordingFlipper{}, logs: &syncBuf{}, audit: &syncBuf{}}
	r, err := newControlRoutes(fakeServiceVerifier{}, e.flip, slog.New(slog.NewJSONHandler(e.logs, nil)), slog.New(slog.NewJSONHandler(e.audit, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if withAccounts {
		r.WithAccounts(e.ender)
	}
	mux := http.NewServeMux()
	r.Register(mux)
	e.mux = mux
	return e
}

func (e *accountEnv) do(method, path, token, ctype, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func TestControlAccountRoutesEndTheNamedLink(t *testing.T) {
	for _, tc := range []struct{ path, want, call string }{
		{unlinkPath, `{"result":"unlinked"}`, "unlink:" + operatorTarget + ":operator_request"},
		{erasePath, `{"result":"erased"}`, "erase:" + operatorTarget},
	} {
		e := newAccountEnv(t, true)
		rec := e.do("POST", tc.path, allowedCred, "application/json", `{"principal":"`+operatorTarget+`"}`, nil)
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != tc.want {
			t.Fatalf("%s: %d %q", tc.path, rec.Code, rec.Body.String())
		}
		if len(e.ender.calls) != 1 || e.ender.calls[0] != tc.call {
			t.Fatalf("%s: calls %v", tc.path, e.ender.calls)
		}
		if e.flip.n() != 0 {
			t.Fatal("an account route touched the kill switch")
		}
	}
}

// Without WithAccounts the routes do not exist, and the kill switch route is
// unchanged.
func TestControlAccountRoutesAreOffUnlessWired(t *testing.T) {
	e := newAccountEnv(t, false)
	for _, p := range []string{unlinkPath, erasePath} {
		if rec := e.do("POST", p, allowedCred, "application/json", `{"principal":"`+operatorTarget+`"}`, nil); rec.Code != 404 && rec.Code != 405 {
			t.Fatalf("%s mounted without accounts: %d", p, rec.Code)
		}
	}
	if rec := e.do("PUT", controlPath, allowedCred, "application/json", `{"killed":true}`, nil); rec.Code != 200 {
		t.Fatalf("kill switch broke: %d", rec.Code)
	}
}

// Same identity gate as the kill switch: nothing is ended for a caller that
// is not an allowed machine subject, or that came through a proxy.
func TestControlAccountRoutesRefuseTheWrongCaller(t *testing.T) {
	body := `{"principal":"` + operatorTarget + `"}`
	for _, p := range []string{unlinkPath, erasePath} {
		e := newAccountEnv(t, true)
		for _, tc := range []struct {
			name, tok string
			hdr       map[string]string
			status    int
		}{
			{"no token", "", nil, 401},
			{"bad signature", "svc-token-bad", nil, 401},
			{"subject not allowed", "svc-token-unknown-subject", nil, 403},
			{"jwks down", "svc-token-jwks-down", nil, 503},
			{"unclassified", "svc-token-unclassified", nil, 503},
			{"via proxy", allowedCred, map[string]string{"X-Forwarded-For": "203.0.113.9"}, 404},
		} {
			rec := e.do("POST", p, tc.tok, "application/json", body, tc.hdr)
			if rec.Code != tc.status {
				t.Fatalf("%s %s: %d, want %d", p, tc.name, rec.Code, tc.status)
			}
		}
		if e.ender.n() != 0 {
			t.Fatalf("%s: a refused caller ended a link: %v", p, e.ender.calls)
		}
	}
}

func TestControlAccountRoutesStrictBody(t *testing.T) {
	for _, p := range []string{unlinkPath, erasePath} {
		e := newAccountEnv(t, true)
		for name, tc := range map[string]struct {
			ctype, body string
			status      int
		}{
			"missing principal":   {"application/json", `{}`, 400},
			"null principal":      {"application/json", `{"principal":null}`, 400},
			"not a prn id":        {"application/json", `{"principal":"someone@example.com"}`, 400},
			"empty principal":     {"application/json", `{"principal":""}`, 400},
			"unknown field":       {"application/json", `{"principal":"` + operatorTarget + `","all":true}`, 400},
			"two objects":         {"application/json", `{"principal":"` + operatorTarget + `"}{}`, 400},
			"not json":            {"application/json", `prn_x`, 400},
			"wrong content type":  {"text/plain", `{"principal":"` + operatorTarget + `"}`, 415},
			"too large":           {"application/json", `{"principal":"` + strings.Repeat("a", 2000) + `"}`, 413},
			"array of principals": {"application/json", `{"principal":["` + operatorTarget + `"]}`, 400},
		} {
			rec := e.do("POST", p, allowedCred, tc.ctype, tc.body, nil)
			if rec.Code != tc.status {
				t.Fatalf("%s %s: %d %q, want %d", p, name, rec.Code, rec.Body.String(), tc.status)
			}
		}
		if e.ender.n() != 0 {
			t.Fatalf("%s: a refused body ended a link: %v", p, e.ender.calls)
		}
	}
}

// A store that cannot delete is never "ok", and neither audit nor log carry
// the principal, the subject or the token.
func TestControlAccountFailureIsNeverASuccessAndLogsAreClean(t *testing.T) {
	e := newAccountEnv(t, true)
	e.ender.err = tokenstore.ErrUnavailable
	for _, p := range []string{unlinkPath, erasePath} {
		rec := e.do("POST", p, allowedCred, "application/json", `{"principal":"`+operatorTarget+`"}`, nil)
		if rec.Code != 503 || !strings.Contains(rec.Body.String(), "state_unknown") || strings.Contains(rec.Body.String(), "unlinked") || strings.Contains(rec.Body.String(), "erased") {
			t.Fatalf("%s: %d %q", p, rec.Code, rec.Body.String())
		}
	}
	e.ender.err = nil
	e.do("POST", unlinkPath, allowedCred, "application/json", `{"principal":"`+operatorTarget+`"}`, nil)
	e.do("POST", unlinkPath, "svc-token-bad", "application/json", `{"principal":"`+operatorTarget+`"}`, nil)
	e.do("POST", erasePath, allowedCred, "application/json", `{"principal":"`+operatorTarget+`"}`, nil)
	e.do("POST", erasePath, "svc-token-bad", "application/json", `{"principal":"`+operatorTarget+`"}`, nil)
	for _, out := range []string{e.logs.String(), e.audit.String()} {
		for _, secret := range []string{operatorTarget, secretSubject, "svc-token"} {
			if strings.Contains(out, secret) {
				t.Fatalf("log carries %q:\n%s", secret, out)
			}
		}
	}
	for _, ev := range []string{"account_unlink", "account_erase", "account_unlink_refused", "account_erase_refused"} {
		if !strings.Contains(e.audit.String(), ev) {
			t.Fatalf("audit has no %s line:\n%s", ev, e.audit.String())
		}
	}
}

// Only POST: a GET or PUT never ends anything.
func TestControlAccountRoutesPostOnly(t *testing.T) {
	e := newAccountEnv(t, true)
	for _, m := range []string{"GET", "PUT", "DELETE"} {
		for _, p := range []string{unlinkPath, erasePath} {
			if rec := e.do(m, p, allowedCred, "application/json", `{"principal":"`+operatorTarget+`"}`, nil); rec.Code != 405 {
				t.Fatalf("%s %s: %d", m, p, rec.Code)
			}
		}
	}
	if e.ender.n() != 0 {
		t.Fatal("non-POST ended a link")
	}
}
