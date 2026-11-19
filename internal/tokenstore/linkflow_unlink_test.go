package tokenstore_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mctlhq/mctl-glovo/internal/linkflow"
	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

func unlinkBody() string { return mustJSON(map[string]any{"schema": linkflow.UnlinkSchema}) }

func (e *env) link(tok, linkTok string, sess map[string]any) {
	e.t.Helper()
	code := e.issue(tok)
	req := jsonReq("/link/upload", uploadBody(code, sess))
	req.Header.Set("Authorization", "Bearer "+linkTok)
	if r := e.do(req); r.status != 200 {
		e.t.Fatalf("link: %d %s", r.status, r.raw)
	}
}

func (e *env) unlink(body string) resp { return e.do(jsonReq("/link/unlink", body)) }

func ptr(pid string) tokenstore.PID {
	// The only constructor outside the package takes a verified principal.
	p, _ := (fakeVerifier{}).Verify(context.Background(), map[string]string{pidOwner: "tok-owner", pidOther: "tok-other"}[pid])
	return tokenstore.PIDFrom(p)
}

// The exit: after a real Unlink the stored session is gone, a read says
// "not linked" (never a cached session), the other user is untouched, and the
// owner can link again.
func TestLinkUnlinkRemovesTheSessionAndOnlyThat(t *testing.T) {
	e := newEnv(t, defaultCaps())
	e.link("tok-owner", "link-owner", session("owner"))
	e.link("tok-other", "link-other", session("other"))
	if _, err := e.h.Store.Get(context.Background(), ptr(pidOwner)); err != nil {
		t.Fatalf("get before unlink: %v", err)
	}
	r := e.unlink(unlinkBody())
	if r.status != 200 || r.body["result"] != "unlinked" || len(r.body) != 1 {
		t.Fatalf("unlink: %d %s", r.status, r.raw)
	}
	if _, _, ok := e.h.LinkRow(pidOwner); ok {
		t.Fatal("owner row survived the unlink")
	}
	if _, err := e.h.Store.Get(context.Background(), ptr(pidOwner)); !errors.Is(err, tokenstore.ErrNotLinked) {
		t.Fatalf("get after unlink = %v, want ErrNotLinked", err)
	}
	if _, _, ok := e.h.LinkRow(pidOther); !ok {
		t.Fatal("another user's link was removed")
	}
	if got := e.h.Stored(pidOther)["refreshToken"]; got != "refresh-other" {
		t.Fatalf("other session changed: %v", got)
	}
	// Unlinking again is not an error that hides anything: the row is gone.
	if r := e.unlink(unlinkBody()); r.status != 200 {
		t.Fatalf("second unlink: %d %s", r.status, r.raw)
	}
	e.link("tok-owner", "link-owner", session("again"))
	if got := e.h.Stored(pidOwner)["refreshToken"]; got != "refresh-again" {
		t.Fatalf("relink after unlink: %v", got)
	}
}

// Deletion never depends on the thing that might be broken: the kill switch
// is on, consent is gone, and Vault is not called.
func TestLinkUnlinkWorksUnderKillAndWithoutConsentOrVault(t *testing.T) {
	e := newEnv(t, defaultCaps())
	e.link("tok-owner", "link-owner", session("owner"))
	e.h.SetKilled(true)
	e.consent.mu.Lock()
	delete(e.consent.versions, pidOwner)
	e.consent.err = errors.New("consent service down")
	e.consent.mu.Unlock()
	vault := e.h.VaultCalls()
	if r := e.unlink(unlinkBody()); r.status != 200 {
		t.Fatalf("unlink under kill: %d %s", r.status, r.raw)
	}
	if _, _, ok := e.h.LinkRow(pidOwner); ok {
		t.Fatal("row survived")
	}
	if e.h.VaultCalls() != vault {
		t.Fatal("unlink called Vault")
	}
}

// Only a fresh link-audience login may unlink: an MCP token, a stale login,
// no token and a token in the query all leave the row alone.
func TestLinkUnlinkNeedsAFreshLinkToken(t *testing.T) {
	e := newEnv(t, defaultCaps())
	e.link("tok-owner", "link-owner", session("owner"))
	for _, tc := range []struct {
		name, auth string
		status     int
		code       string
	}{
		{"mcp token", "Bearer tok-owner", 401, "invalid_token"},
		{"stale login", "Bearer link-stale", 401, "fresh_login_required"},
		{"no token", "", 401, "invalid_token"},
		{"idp down", "Bearer link-jwks-down", 503, "temporarily_unavailable"},
	} {
		req := jsonReq("/link/unlink", unlinkBody())
		req.Header.Del("Authorization")
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		wantRefusal(t, e.do(req), tc.status, tc.code)
		if _, _, ok := e.h.LinkRow(pidOwner); !ok {
			t.Fatalf("%s: row removed", tc.name)
		}
	}
	req := jsonReq("/link/unlink?access_token=link-owner", unlinkBody())
	req.Header.Del("Authorization")
	wantRefusal(t, e.do(req), 401, "invalid_token")
	if _, _, ok := e.h.LinkRow(pidOwner); !ok {
		t.Fatal("query-token request removed the row")
	}
}

// The pid is the verified one. A body that names another principal, carries
// extra fields or a wrong schema is refused and removes nothing.
func TestLinkUnlinkCannotTargetAnotherPrincipal(t *testing.T) {
	e := newEnv(t, defaultCaps())
	e.link("tok-owner", "link-owner", session("owner"))
	e.link("tok-other", "link-other", session("other"))
	for _, body := range []string{
		mustJSON(map[string]any{"schema": linkflow.UnlinkSchema, "principal": pidOther}),
		mustJSON(map[string]any{"schema": linkflow.UnlinkSchema, "pid": pidOther}),
		mustJSON(map[string]any{"schema": linkflow.PreviewSchema}),
		mustJSON(map[string]any{}),
		`not json`,
		`{"schema":"` + linkflow.UnlinkSchema + `","schema":"` + linkflow.UnlinkSchema + `"}`,
	} {
		wantRefusal(t, e.unlink(body), 400, "malformed_request")
	}
	if _, _, ok := e.h.LinkRow(pidOwner); !ok {
		t.Fatal("a refused body removed the owner row")
	}
	// link-other unlinks only other.
	req := jsonReq("/link/unlink", unlinkBody())
	req.Header.Set("Authorization", "Bearer link-other")
	if r := e.do(req); r.status != 200 {
		t.Fatalf("other unlink: %d %s", r.status, r.raw)
	}
	if _, _, ok := e.h.LinkRow(pidOwner); !ok {
		t.Fatal("other's unlink removed the owner row")
	}
	if _, _, ok := e.h.LinkRow(pidOther); ok {
		t.Fatal("other row survived its own unlink")
	}
}

type failingUnlinkStore struct{ tokenstore.Store }

func (failingUnlinkStore) Unlink(context.Context, tokenstore.PID, string) error {
	return tokenstore.ErrUnavailable
}

// A store that cannot delete never answers "unlinked".
func TestLinkUnlinkFailureIsNeverASuccess(t *testing.T) {
	e := newEnv(t, defaultCaps())
	lf, err := linkflow.New(linkflow.Config{
		Enabled: true, Store: failingUnlinkStore{e.h.Store}, Verifier: fakeVerifier{},
		LinkVerifier: e.linkV, Consent: e.consent, Log: slog.New(slog.NewJSONHandler(e.logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	lf.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, jsonReq("/link/unlink", unlinkBody()))
	if rec.Code != 503 || strings.Contains(rec.Body.String(), "unlinked") || !strings.Contains(rec.Body.String(), `"unavailable"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

// The flag off means no unlink route either.
func TestLinkUnlinkFlagOffIs404(t *testing.T) {
	lf, _ := linkflow.New(linkflow.Config{Enabled: false})
	mux := http.NewServeMux()
	lf.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, jsonReq("/link/unlink", unlinkBody()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d", rec.Code)
	}
}
