package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mctlhq/mctl-glovo/internal/consentgate"
	"github.com/mctlhq/mctl-glovo/internal/linkflow"
	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func fullEnv() map[string]string {
	return map[string]string{
		"GLOVO_C2_ENABLED":            "true",
		"GLOVO_C2_ALLOWED_PRINCIPALS": "prn_owner",
		"GLOVO_DATABASE_URL":          "postgres://u@db.invalid/glovo",
		"GLOVO_VAULT_ADDR":            "http://vault-active.vault.svc.cluster.local:8200",
		"GLOVO_EGRESS_PROXY":          "http://glovo-egress.mctl-glovo.svc.cluster.local:3128",
	}
}

// Off by default: no route, and nothing else is read or built.
func TestC2OffByDefault(t *testing.T) {
	for _, v := range []string{"", "false"} {
		read := 0
		c, err := loadC2(func(k string) string {
			read++
			if k == "GLOVO_C2_ENABLED" {
				return v
			}
			t.Fatalf("read %s with the flag off", k)
			return ""
		})
		if err != nil || c.enabled || read != 1 {
			t.Fatalf("%q: %+v %v", v, c, err)
		}
		h, closeFn, err := buildLink(context.Background(), c, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		closeFn()
		mux := http.NewServeMux()
		h.Register(mux)
		for _, p := range []string{"/link", "/link/code", "/link/preview", "/link/upload"} {
			for _, m := range []string{"GET", "POST"} {
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, httptest.NewRequest(m, p, strings.NewReader("{}")))
				if rec.Code != http.StatusNotFound {
					t.Errorf("%s %s = %d with the flag off", m, p, rec.Code)
				}
			}
		}
	}
}

func TestC2FlagTypoIsAnError(t *testing.T) {
	for _, v := range []string{"1", "TRUE", "yes", "on", " true"} {
		if _, err := loadC2(env(map[string]string{"GLOVO_C2_ENABLED": v})); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
}

func TestC2OnNeedsEverything(t *testing.T) {
	if c, err := loadC2(env(fullEnv())); err != nil || !c.enabled || c.vaultRole != "mctl-glovo" {
		t.Fatalf("%+v %v", c, err)
	}
	for _, k := range []string{"GLOVO_C2_ALLOWED_PRINCIPALS", "GLOVO_DATABASE_URL", "GLOVO_VAULT_ADDR", "GLOVO_EGRESS_PROXY"} {
		m := fullEnv()
		delete(m, k)
		if _, err := loadC2(env(m)); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("without %s: %v", k, err)
		}
	}
	m := fullEnv()
	m["GLOVO_C2_ALLOWED_PRINCIPALS"] = "owner"
	if _, err := loadC2(env(m)); err == nil {
		t.Error("non-prn principal accepted")
	}
	m = fullEnv()
	m["GLOVO_LINK_TRUSTED_PROXIES"] = "10.0.0.0/8, nonsense"
	if _, err := loadC2(env(m)); err == nil {
		t.Error("bad CIDR accepted")
	}
}

// The consent route is configured whole or not at all; neither set keeps
// the fail-closed source.
func TestC2ConsentAPIConfig(t *testing.T) {
	if c, err := loadC2(env(fullEnv())); err != nil || c.consentAPI != "" || c.consentTokenFile != "" {
		t.Fatalf("unset = %+v %v", c, err)
	}
	for _, k := range []string{"GLOVO_CONSENT_API_URL", "GLOVO_CONSENT_TOKEN_FILE"} {
		m := fullEnv()
		m[k] = "x"
		if _, err := loadC2(env(m)); err == nil {
			t.Errorf("only %s accepted", k)
		}
	}
	m := fullEnv()
	m["GLOVO_CONSENT_API_URL"] = "https://api.mctl.ai"
	m["GLOVO_CONSENT_TOKEN_FILE"] = "/var/run/secrets/mctl-api/token"
	if c, err := loadC2(env(m)); err != nil || c.consentAPI != "https://api.mctl.ai" || c.consentTokenFile != "/var/run/secrets/mctl-api/token" {
		t.Fatalf("both = %+v %v", c, err)
	}
}

// On, the build refuses a Vault outside the cluster and a bad proxy before
// it opens anything.
func TestC2OnRefusesBadDependencies(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for k, v := range map[string]string{
		"GLOVO_VAULT_ADDR":   "https://secrets.mctl.ai",
		"GLOVO_EGRESS_PROXY": "https://proxy.invalid:3128",
	} {
		m := fullEnv()
		m[k] = v
		c, err := loadC2(env(m))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := buildLink(context.Background(), c, log); err == nil {
			t.Errorf("%s=%s accepted", k, v)
		}
	}
}

// With mctl-api's consent route configured, consent is the access gate and
// the allowlist is optional (an extra restriction when set). Without it the
// allowlist is required, and with neither the service does not boot.
func TestC2AccessModes(t *testing.T) {
	withAPI := func() map[string]string {
		m := fullEnv()
		m["GLOVO_CONSENT_API_URL"] = "https://api.mctl.ai"
		m["GLOVO_CONSENT_TOKEN_FILE"] = "/var/run/secrets/mctl-api/token"
		return m
	}
	// Consent API, allowlist unset: boots, consent admits.
	m := withAPI()
	delete(m, "GLOVO_C2_ALLOWED_PRINCIPALS")
	c, err := loadC2(env(m))
	if err != nil || len(c.allowed) != 0 {
		t.Fatalf("consent without allowlist: %+v %v", c, err)
	}
	src, admit, err := c2Access(c)
	if err != nil || !admit {
		t.Fatalf("consent mode: %v admit=%v", err, admit)
	}
	if _, ok := src.(*linkflow.APIConsent); !ok {
		t.Fatalf("source %T", src)
	}
	// Consent API and allowlist: both bind.
	c, err = loadC2(env(withAPI()))
	if err != nil || len(c.allowed) != 1 {
		t.Fatalf("consent with allowlist: %+v %v", c, err)
	}
	if _, admit, err := c2Access(c); err != nil || !admit {
		t.Fatalf("consent+allowlist: %v %v", admit, err)
	}
	// No consent API: the allowlist alone, and the source never consents.
	c, err = loadC2(env(fullEnv()))
	if err != nil {
		t.Fatal(err)
	}
	src, admit, err = c2Access(c)
	if err != nil || admit {
		t.Fatalf("allowlist mode: %v admit=%v", err, admit)
	}
	if _, ok := src.(linkflow.UnavailableConsent); !ok {
		t.Fatalf("source %T", src)
	}
	// Neither: no boot.
	m = fullEnv()
	delete(m, "GLOVO_C2_ALLOWED_PRINCIPALS")
	if _, err := loadC2(env(m)); err == nil || !strings.Contains(err.Error(), "GLOVO_C2_ALLOWED_PRINCIPALS") {
		t.Fatalf("neither configured booted: %v", err)
	}
	if _, _, err := c2Access(c2Config{enabled: true}); err == nil {
		t.Fatal("c2Access with neither configured")
	}
}

type nopAdmitter struct{}

func (nopAdmitter) Admit(context.Context, string) error { return nil }

func TestC2VerifierConfig(t *testing.T) {
	c := c2Config{allowed: []string{"prn_owner"}}
	v := verifierConfig(c, nopAdmitter{}, true)
	if v.Admitter == nil || !v.RequireGrant || len(v.AllowedPrincipals) != 1 {
		t.Fatalf("consent mode: %+v", v)
	}
	if v := verifierConfig(c, nopAdmitter{}, false); v.Admitter != nil || !v.RequireGrant {
		t.Fatalf("allowlist mode: %+v", v)
	}
}

// The upload consent re-check is wired from the gate.
func TestC2StoreRechecksConsent(t *testing.T) {
	g, err := consentgate.New(consentgate.Config{Source: linkflow.UnavailableConsent{}, Version: linkflow.ConsentVersion})
	if err != nil {
		t.Fatal(err)
	}
	var cfg tokenstore.Config
	withLinkConsent(&cfg, g)
	if cfg.LinkConsent == nil {
		t.Fatal("no upload consent check")
	}
	if err := cfg.LinkConsent(context.Background(), tokenstore.PID{}); err == nil {
		t.Fatal("upload consent check passed with no consent")
	}
}
