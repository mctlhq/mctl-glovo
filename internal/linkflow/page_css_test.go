package linkflow

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mctlhq/mctl-glovo/internal/authn"
	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

type fakeStore struct{ tokenstore.Store }

func (fakeStore) LinkAttempt(string) error { return nil }
func (fakeStore) CreateLinkCode(context.Context, tokenstore.PID, string) (string, time.Time, error) {
	return "TESTCODE-1234", time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC), nil
}

type fakeConsent struct{}

func (fakeConsent) RecordedConsent(context.Context, tokenstore.PID) (string, error) {
	return ConsentVersion, nil
}

type fakeVerifier struct{}

func (fakeVerifier) Verify(context.Context, string) (*authn.Principal, error) {
	return &authn.Principal{ID: "prn_01TESTTESTTESTTESTTESTTEST"}, nil
}

func newMux(t *testing.T) *http.ServeMux {
	t.Helper()
	h, err := New(Config{Enabled: true, Store: fakeStore{}, Consent: fakeConsent{}, LinkVerifier: fakeVerifier{}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.Register(mux)
	return mux
}

// pageRoutes are every route that answers an HTML page.
func pageResponses(t *testing.T) map[string]*httptest.ResponseRecorder {
	t.Helper()
	mux := newMux(t)
	out := map[string]*httptest.ResponseRecorder{}
	get := httptest.NewRequest(http.MethodGet, "/link", nil)
	out["GET /link"] = httptest.NewRecorder()
	mux.ServeHTTP(out["GET /link"], get)
	post := httptest.NewRequest(http.MethodPost, "/link/code", strings.NewReader(`{}`))
	post.Header.Set("Accept", "text/html")
	post.Header.Set("Authorization", "Bearer x")
	out["POST /link/code (html)"] = httptest.NewRecorder()
	mux.ServeHTTP(out["POST /link/code (html)"], post)
	return out
}

var styleRe = regexp.MustCompile(`(?s)<style>(.*?)</style>`)

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// The CSP hash must be that of the exact bytes served in <style>, computed
// independently of pageCSP here, for every page route.
func TestPagesCSPHashesTheServedStyle(t *testing.T) {
	for name, rec := range pageResponses(t) {
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", name, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		ms := styleRe.FindAllStringSubmatch(body, -1)
		if len(ms) != 1 {
			t.Fatalf("%s: want exactly one <style>, got %d", name, len(ms))
		}
		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "style-src "+hashOf(ms[0][1])+";") {
			t.Errorf("%s: CSP %q does not carry the hash of the served style (%s)", name, csp, hashOf(ms[0][1]))
		}
		if ms[0][1] != pageCSS {
			t.Errorf("%s: served style differs from pageCSS", name)
		}
	}
}

func TestPagesCSPStaysStrict(t *testing.T) {
	for name, rec := range pageResponses(t) {
		csp := rec.Header().Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'none'", "frame-ancestors 'none'", "base-uri 'none'", "form-action 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP %q lacks %q", name, csp, want)
			}
		}
		for _, bad := range []string{"unsafe-inline", "unsafe-eval", "nonce-", "script-src", "*", "data:", "https:"} {
			if strings.Contains(csp, bad) {
				t.Errorf("%s: CSP %q contains %q", name, csp, bad)
			}
		}
		if n := strings.Count(csp, "style-src"); n != 1 {
			t.Errorf("%s: want one style-src, got %d", name, n)
		}
		h := rec.Header()
		for k, v := range map[string]string{
			"Cache-Control": "no-store", "X-Content-Type-Options": "nosniff",
			"X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer",
		} {
			if h.Get(k) != v {
				t.Errorf("%s: %s = %q, want %q", name, k, h.Get(k), v)
			}
		}
		if got := h.Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Errorf("%s: Content-Type %q", name, got)
		}
	}
}

// A CSS edit that does not go through the hash path must turn this red: the
// CSP built for an edited stylesheet differs from the one in force.
func TestCSPFollowsTheStylesheet(t *testing.T) {
	if pageCSP != cspForStyle(pageCSS) {
		t.Fatal("pageCSP is not derived from pageCSS")
	}
	if cspForStyle(pageCSS+" ") == pageCSP {
		t.Fatal("a changed stylesheet yields the same CSP")
	}
}

func TestPagesHaveNoScriptNoInlineStyleNoExternal(t *testing.T) {
	attr := regexp.MustCompile(`(?i)\sstyle\s*=`)
	handler := regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	for name, rec := range pageResponses(t) {
		body := rec.Body.String()
		low := strings.ToLower(body)
		if strings.Contains(low, "<script") || strings.Contains(low, "javascript:") {
			t.Errorf("%s: contains script", name)
		}
		if attr.MatchString(body) {
			t.Errorf("%s: contains a style attribute, which the hash-only CSP blocks", name)
		}
		if handler.MatchString(body) {
			t.Errorf("%s: contains an inline event handler", name)
		}
		for _, bad := range []string{"<link", "<img", "<iframe", "http://", "https://", "@import", "url(", "@font-face"} {
			if strings.Contains(low, bad) {
				t.Errorf("%s: contains external reference %q", name, bad)
			}
		}
	}
}

func TestPageCSSIsTemplateSafe(t *testing.T) {
	if strings.Contains(pageCSS, "{{") || strings.Contains(pageCSS, "</style") {
		t.Fatal("pageCSS would be read by the template parser or close the element")
	}
	if !strings.Contains(pageCSS, "prefers-color-scheme:dark") || !strings.Contains(pageCSS, "focus-visible") {
		t.Fatal("pageCSS lost the dark theme or the focus style")
	}
}

func TestPageTextUnchanged(t *testing.T) {
	recs := pageResponses(t)
	g := recs["GET /link"].Body.String()
	for _, want := range append([]string{"Link your Glovo account", "How linking works", "glovo-link", "Consent text version"}, htmlEscaped(ConsentText)...) {
		if !strings.Contains(g, want) {
			t.Errorf("GET /link lost text %q", want)
		}
	}
	c := recs["POST /link/code (html)"].Body.String()
	for _, want := range []string{"Your one-time link code", "TESTCODE-1234", "2030-01-02T03:04:05Z", "Never pass a code as a command-line argument."} {
		if !strings.Contains(c, want) {
			t.Errorf("code page lost text %q", want)
		}
	}
	if strings.Contains(g, "TESTCODE") {
		t.Error("the plain page shows a code")
	}
}

func htmlEscaped(in []string) []string {
	out := make([]string, len(in))
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;")
	for i, s := range in {
		out[i] = r.Replace(s)
	}
	return out
}

// JSON answers keep the no-style CSP: only HTML pages get the hash.
func TestJSONRoutesKeepNoStyleCSP(t *testing.T) {
	mux := newMux(t)
	req := httptest.NewRequest(http.MethodPost, "/link/code", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer x")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if csp := rec.Header().Get("Content-Security-Policy"); strings.Contains(csp, "style-src") {
		t.Errorf("JSON answer carries style-src: %q", csp)
	}
}

// Contrast: every text pair of the stylesheet reaches AA in both themes.
func TestPageContrast(t *testing.T) {
	light, dark := cssVars(t, pageCSS, false), cssVars(t, pageCSS, true)
	pairs := []struct {
		fg, bg string
		min    float64
	}{
		{"--fg", "--bg", 4.5}, {"--muted", "--bg", 4.5}, {"--fg", "--card", 4.5},
		{"--accent-text", "--bg", 4.5}, {"--accent-text", "--card", 4.5},
		{"--band-fg", "--band", 4.5}, {"--fg", "--code-bg", 4.5},
		{"--accent", "--card", 3}, {"--focus", "--bg", 3}, {"--focus", "--card", 3},
	}
	for name, m := range map[string]map[string]string{"light": light, "dark": dark} {
		for _, p := range pairs {
			a, b := hexRGB(t, m[p.fg]), hexRGB(t, m[p.bg])
			if r := contrast(a, b); r < p.min {
				t.Errorf("%s: %s on %s is %.2f:1, want >= %.1f", name, p.fg, p.bg, r, p.min)
			}
		}
	}
}
