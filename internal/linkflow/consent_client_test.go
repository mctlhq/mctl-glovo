package linkflow

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	c, calls := consentServer(t, 200, `{"principal_id":"`+testPID+`","state":"active","consent_version":"c2-link-v1","recorded_at":"2026-11-16T00:00:00Z"}`)
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
	for _, base := range []string{"", "http://api.mctl.ai", "https://", "https://api.mctl.ai/api", "https://u@api.mctl.ai", "https://api.mctl.ai?x=1"} {
		if _, err := NewAPIConsent(APIConsentConfig{BaseURL: base, TokenFile: "/t"}); err == nil {
			t.Errorf("%q accepted", base)
		}
	}
	if _, err := NewAPIConsent(APIConsentConfig{BaseURL: "https://api.mctl.ai"}); err == nil {
		t.Error("no token file accepted")
	}
	if _, err := NewAPIConsent(APIConsentConfig{BaseURL: "https://api.mctl.ai/", TokenFile: "/t"}); err != nil {
		t.Errorf("valid config refused: %v", err)
	}
}
