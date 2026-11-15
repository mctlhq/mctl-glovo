package gateway

import (
	"crypto/sha256"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func landingServer(t *testing.T) *httptest.Server {
	t.Helper()
	g, err := New(Config{
		Resource: resource, AuthorizationServer: issuer, Verifier: stubVerifier{},
		MCP: http.NotFoundHandler(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	g.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestLandingLanguage(t *testing.T) {
	ts := landingServer(t)
	cases := []struct {
		name string
		path string
		hdr  map[string]string
		want string
	}{
		{"default", "/", nil, "en"},
		{"accept-language es", "/", map[string]string{"Accept-Language": "es-ES,es;q=0.9,en;q=0.8"}, "es"},
		{"accept-language skips unknown", "/", map[string]string{"Accept-Language": "de-DE, ru;q=0.7"}, "ru"},
		{"accept-language q=0 refused", "/", map[string]string{"Accept-Language": "ru;q=0, es"}, "es"},
		{"query beats header", "/?lang=ru", map[string]string{"Accept-Language": "es"}, "ru"},
		{"cookie beats header", "/", map[string]string{"Accept-Language": "es", "Cookie": langCookie + "=ru"}, "ru"},
		{"unknown query ignored", "/?lang=xx", map[string]string{"Accept-Language": "es"}, "es"},
		{"bad cookie ignored", "/", map[string]string{"Cookie": langCookie + "=<x>"}, "en"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, body := do(t, "GET", ts.URL+c.path, c.hdr, "")
			if r.StatusCode != 200 {
				t.Fatalf("status %d", r.StatusCode)
			}
			if got := r.Header.Get("Content-Language"); got != c.want {
				t.Fatalf("Content-Language %q, want %q", got, c.want)
			}
			if !strings.Contains(body, `<html lang="`+c.want+`">`) {
				t.Fatalf("html lang is not %q", c.want)
			}
			if !strings.Contains(body, landingTexts[c.want].Title) {
				t.Fatalf("title for %q missing", c.want)
			}
		})
	}
}

func TestLandingRemembersExplicitLanguage(t *testing.T) {
	ts := landingServer(t)
	r, _ := do(t, "GET", ts.URL+"/?lang=es", nil, "")
	c := r.Header.Get("Set-Cookie")
	for _, want := range []string{langCookie + "=es", "HttpOnly", "Secure", "SameSite=Lax"} {
		if !strings.Contains(c, want) {
			t.Fatalf("Set-Cookie %q lacks %q", c, want)
		}
	}
	r, _ = do(t, "GET", ts.URL+"/", map[string]string{"Accept-Language": "es"}, "")
	if c := r.Header.Get("Set-Cookie"); c != "" {
		t.Fatalf("cookie set without ?lang: %q", c)
	}
}

func TestLandingCSPAdmitsOnlyItsStyle(t *testing.T) {
	ts := landingServer(t)
	r, body := do(t, "GET", ts.URL+"/", nil, "")
	csp := r.Header.Get("Content-Security-Policy")
	sum := sha256.Sum256([]byte(landingCSS))
	if !strings.Contains(csp, "style-src 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") {
		t.Fatalf("CSP %q does not pin the stylesheet hash", csp)
	}
	if !strings.HasPrefix(csp, "default-src 'none';") || strings.Contains(csp, "unsafe") {
		t.Fatalf("CSP too loose: %q", csp)
	}
	// The hash is of landingCSS; the template must embed it unchanged or
	// the browser drops the styles.
	if !strings.Contains(body, "<style>"+landingCSS+"</style>") {
		t.Fatal("rendered <style> differs from the hashed CSS")
	}
	if strings.Contains(body, "<script") {
		t.Fatal("landing must not carry scripts")
	}
	if r.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatal("security headers missing")
	}
}

func TestLandingShowsEndpointAndNoCheckout(t *testing.T) {
	ts := landingServer(t)
	for _, l := range landingLangs {
		_, body := do(t, "GET", ts.URL+"/?lang="+l, nil, "")
		if !strings.Contains(body, "claude mcp add --transport http glovo "+resource) {
			t.Fatalf("%s: connect command missing", l)
		}
		if !strings.Contains(body, landingTexts[l].Never[0]) {
			t.Fatalf("%s: no-checkout statement missing", l)
		}
	}
}

func TestLandingRoutes(t *testing.T) {
	ts := landingServer(t)
	if r, _ := do(t, "GET", ts.URL+"/other", nil, ""); r.StatusCode != http.StatusNotFound {
		t.Fatalf("/other: %d, want 404", r.StatusCode)
	}
	if r, _ := do(t, "POST", ts.URL+"/", nil, ""); r.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /: %d, want 405", r.StatusCode)
	}
	r, body := do(t, "HEAD", ts.URL+"/", nil, "")
	if r.StatusCode != 200 || body != "" {
		t.Fatalf("HEAD /: %d with %d bytes", r.StatusCode, len(body))
	}
	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.Host = "evil.example"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("foreign host: %d, want 421", resp.StatusCode)
	}
}
