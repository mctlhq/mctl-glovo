package linkflow

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mctlhq/mctl-glovo/internal/authn"
	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

const (
	testPID     = "prn_01HZZZZZZZZZZZZZZZZZZZZZZA"
	testToken   = "svc-token-not-a-secret" //nolint:gosec // fake test value
	otherPID    = "prn_01HZZZZZZZZZZZZZZZZZZZZZZB"
	readPathPID = ConsentReadPath + testPID
)

func pidOf(s string) tokenstore.PID { return tokenstore.PIDFrom(&authn.Principal{ID: s}) }

func consentServer(t *testing.T, status int, body string) (*APIConsent, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.URL.Path != readPathPID || r.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(w, "unexpected request", http.StatusTeapot)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	tf := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tf, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewAPIConsent(APIConsentConfig{BaseURL: srv.URL, TokenFile: tf, Client: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return c, &calls
}

func TestAPIConsentActive(t *testing.T) {
	c, calls := consentServer(t, 200, `{"principal_id":"`+testPID+`","state":"active","consent_version":"`+ConsentVersion+`","recorded_at":"2026-11-16T00:00:00Z"}`)
	v, err := c.RecordedConsent(context.Background(), pidOf(testPID))
	if err != nil || v != ConsentVersion || *calls != 1 {
		t.Fatalf("= %q %v (calls %d)", v, err, *calls)
	}
}

// Only 200 state none is ErrNoConsent.
func TestAPIConsentNone(t *testing.T) {
	c, _ := consentServer(t, 200, `{"principal_id":"`+testPID+`","state":"none"}`)
	if _, err := c.RecordedConsent(context.Background(), pidOf(testPID)); !errors.Is(err, ErrNoConsent) {
		t.Fatalf("= %v, want ErrNoConsent", err)
	}
}

// Every other answer is ErrConsentUnavailable: never a consent, never "none".
func TestAPIConsentEverythingElseIsUnavailable(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		body   string
	}{
		"503":                 {503, `{"code":"glovo_consent_unavailable"}`},
		"401":                 {401, `{"error":"not_authorized"}`},
		"404 unmounted":       {404, `{"principal_id":"` + testPID + `","state":"none"}`},
		"429":                 {429, ``},
		"malformed":           {200, `{"state":`},
		"empty":               {200, ``},
		"unknown state":       {200, `{"principal_id":"` + testPID + `","state":"pending"}`},
		"active, no version":  {200, `{"principal_id":"` + testPID + `","state":"active"}`},
		"none with a version": {200, `{"principal_id":"` + testPID + `","state":"none","consent_version":"c2-link-v1"}`},
		"another principal":   {200, `{"principal_id":"` + otherPID + `","state":"active","consent_version":"c2-link-v1"}`},
		"no principal echoed": {200, `{"state":"active","consent_version":"c2-link-v1"}`},
		"oversized":           {200, `{"principal_id":"` + testPID + `","state":"none"}` + strings.Repeat(" ", maxConsentBody)},
	} {
		src, _ := consentServer(t, c.status, c.body)
		v, err := src.RecordedConsent(context.Background(), pidOf(testPID))
		if v != "" || !errors.Is(err, ErrConsentUnavailable) || errors.Is(err, ErrNoConsent) {
			t.Errorf("%s: = %q %v, want ErrConsentUnavailable", name, v, err)
		}
		if err != nil && strings.Contains(err.Error(), testToken) {
			t.Errorf("%s: the token reached the error", name)
		}
	}
}

func TestAPIConsentTransportAndTokenFailures(t *testing.T) {
	c, calls := consentServer(t, 200, `{"principal_id":"`+testPID+`","state":"active","consent_version":"c2-link-v1"}`)
	ctx := context.Background()

	if _, err := c.RecordedConsent(ctx, tokenstore.PID{}); !errors.Is(err, ErrConsentUnavailable) || *calls != 0 {
		t.Fatalf("zero pid = %v (calls %d)", err, *calls)
	}
	missing := *c
	missing.tokenFile = filepath.Join(t.TempDir(), "absent")
	if _, err := missing.RecordedConsent(ctx, pidOf(testPID)); !errors.Is(err, ErrConsentUnavailable) || *calls != 0 {
		t.Fatalf("missing token file = %v", err)
	}
	empty := *c
	empty.tokenFile = filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty.tokenFile, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := empty.RecordedConsent(ctx, pidOf(testPID)); !errors.Is(err, ErrConsentUnavailable) || *calls != 0 {
		t.Fatalf("empty token file = %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.RecordedConsent(cctx, pidOf(testPID)); !errors.Is(err, ErrConsentUnavailable) {
		t.Fatalf("cancelled = %v", err)
	}
}

func TestNewAPIConsentValidates(t *testing.T) {
	if _, err := NewAPIConsent(APIConsentConfig{BaseURL: "https://api.mctl.ai"}); err == nil {
		t.Error("no token file accepted")
	}
	for _, tf := range []string{"token", "./token", "/a/../token", "/a//token"} {
		if _, err := NewAPIConsent(APIConsentConfig{BaseURL: "https://api.mctl.ai", TokenFile: tf}); err == nil {
			t.Errorf("token file %q accepted", tf)
		}
	}
}

func TestNewAPIConsentBaseURL(t *testing.T) {
	accepted := []string{
		"https://api.mctl.ai",
		"https://api.mctl.ai/",
		"HTTPS://API.MCTL.AI",
		"https://api.mctl.ai:8443",
		"https://10.0.0.1",
		"https://[::1]:8443",
		"http://mctl-api.svc",
		"http://mctl-api.ns.svc",
		"http://mctl-api.ns.svc:8080",
		"http://mctl-api.ns.svc.cluster.local:8080",
		"http://mctl-api.svc.cluster.local/",
		"HTTP://MCTL-API.NS.SVC",
		"http://Mctl-Api.Ns.Svc.Cluster.Local",
	}
	for _, base := range accepted {
		if _, err := NewAPIConsent(APIConsentConfig{BaseURL: base, TokenFile: "/t"}); err != nil {
			t.Errorf("%q refused: %v", base, err)
		}
	}
	refused := []string{
		"", "ftp://api.mctl.ai", "api.mctl.ai", "https://", "http://", "//api.mctl.ai",
		"http://api.mctl.ai", "http://localhost", "http://localhost:8080",
		"http://127.0.0.1:8080", "http://10.0.0.1", "http://[::1]:8080", "http://[::ffff:10.0.0.1]",
		"http://1.2.3.4.svc", "http://evil.svc.example.com", "http://evilsvc", "http://evil-svc",
		"http://svc", "http://.svc", "http://a..svc", "http://svc.cluster.local",
		"http://a.b.c.svc", "http://a.b.c.svc.cluster.local", "http://mctl-api.svc.",
		"http://mctl-api.svc.cluster.local.evil.com", "http://mctl-api.svc.evil.com",
		"http://mctl_api.svc", "http://-a.svc", "http://a b.svc",
		"https://api.mctl.ai/api", "https://u@api.mctl.ai", "https://u:p@api.mctl.ai",
		"http://u@mctl-api.ns.svc", "http://mctl-api.ns.svc@evil.com", "http://evil.com#@x.svc",
		"https://api.mctl.ai?x=1", "https://api.mctl.ai?", "http://mctl-api.ns.svc?x=1",
		"https://api.mctl.ai#f", "http://mctl-api.ns.svc#f", "mailto:x@api.mctl.ai",
	}
	for _, base := range refused {
		_, err := NewAPIConsent(APIConsentConfig{BaseURL: base, TokenFile: "/t"})
		if err == nil {
			t.Errorf("%q accepted", base)
			continue
		}
		if base != "" && strings.Contains(err.Error(), base) {
			t.Errorf("%q echoed in error %q", base, err)
		}
	}
}

func TestReadTokenRefusesNonRegularFiles(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	for name, path := range map[string]string{"fifo": fifo, "directory": dir} {
		done := make(chan error, 1)
		go func() { _, err := readToken(path); done <- err }()
		select {
		case err := <-done:
			if !errors.Is(err, ErrConsentUnavailable) {
				t.Errorf("%s token path = %v, want ErrConsentUnavailable", name, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s token path blocked past the deadline", name)
		}
	}
	// A symlink to a regular token file is still a regular file.
	real := filepath.Join(dir, "real")
	if err := os.WriteFile(real, []byte(testToken), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if tok, err := readToken(link); err != nil || tok != testToken {
		t.Errorf("symlinked regular file = %q, %v", tok, err)
	}
}

// A redirect is never followed: a 3xx fails closed even when its target
// would answer with a consent.
func TestAPIConsentDoesNotFollowRedirects(t *testing.T) {
	target := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			target++
			_, _ = w.Write([]byte(`{"principal_id":"` + testPID + `","state":"active","consent_version":"c2-link-v1"}`))
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	tf := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tf, []byte(testToken), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewAPIConsent(APIConsentConfig{BaseURL: srv.URL, TokenFile: tf, Client: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	v, err := c.RecordedConsent(context.Background(), pidOf(testPID))
	if v != "" || !errors.Is(err, ErrConsentUnavailable) || target != 0 {
		t.Fatalf("302 = %q %v (target hit %d), want ErrConsentUnavailable and no hop", v, err, target)
	}
}
