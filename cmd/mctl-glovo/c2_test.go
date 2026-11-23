package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mctlhq/mctl-glovo/internal/config"
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
		"GLOVO_C2_AUDIT_KEY":          testAuditKey,
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
	for _, k := range []string{"GLOVO_C2_ALLOWED_PRINCIPALS", "GLOVO_C2_AUDIT_KEY", "GLOVO_DATABASE_URL", "GLOVO_VAULT_ADDR", "GLOVO_EGRESS_PROXY"} {
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

// The consent route is validated at boot: scheme http or https, no userinfo,
// path or query, and an absolute clean token path.
func TestC2ConsentAPIBootValidation(t *testing.T) {
	ok := func(url, tf string) error {
		m := fullEnv()
		m["GLOVO_CONSENT_API_URL"] = url
		m["GLOVO_CONSENT_TOKEN_FILE"] = tf
		_, err := loadC2(env(m))
		return err
	}
	for _, u := range []string{"http://mctl-api.ns.svc.cluster.local", "https://api.mctl.ai", "http://mctl-api.ns.svc:8080/"} {
		if err := ok(u, "/var/run/secrets/mctl-api/token"); err != nil {
			t.Errorf("%q refused: %v", u, err)
		}
	}
	for _, u := range []string{"ftp://api.mctl.ai", "api.mctl.ai", "https://u:p@api.mctl.ai", "https://api.mctl.ai?x=1", "https://api.mctl.ai/api", "https://api.mctl.ai#f"} {
		if err := ok(u, "/var/run/secrets/mctl-api/token"); err == nil {
			t.Errorf("url %q accepted", u)
		}
	}
	for _, tf := range []string{"token", "./token", "/var/run/../token", "/var/run//token"} {
		if err := ok("https://api.mctl.ai", tf); err == nil {
			t.Errorf("token file %q accepted", tf)
		}
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

// GLOVO_LINK_RESOURCE and GLOVO_LINK_MAX_AUTH_AGE: unset leaves the link
// login off (preview and upload refuse), and a wrong value stops the boot
// instead of falling back to the MCP audience or a longer age.
func TestC2LinkLoginConfig(t *testing.T) {
	e := fullEnv()
	c, err := loadC2(env(e))
	if err != nil || c.linkResource != "" || c.linkMaxAuthAge != 5*time.Minute {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	e["GLOVO_LINK_RESOURCE"] = "https://glovo.mctl.ai/link"
	e["GLOVO_LINK_MAX_AUTH_AGE"] = "15m"
	if c, err = loadC2(env(e)); err != nil || c.linkResource != "https://glovo.mctl.ai/link" || c.linkMaxAuthAge != 15*time.Minute {
		t.Fatalf("configured: %+v %v", c, err)
	}
	for name, kv := range map[string][2]string{
		"resource is the MCP resource": {"GLOVO_LINK_RESOURCE", config.Resource},
		"resource not https":           {"GLOVO_LINK_RESOURCE", "http://glovo.mctl.ai/link"},
		"resource not a URL":           {"GLOVO_LINK_RESOURCE", "glovo.mctl.ai/link"},
		"resource with a query":        {"GLOVO_LINK_RESOURCE", "https://glovo.mctl.ai/link?x=1"},
		"resource with userinfo":       {"GLOVO_LINK_RESOURCE", "https://u@glovo.mctl.ai/link"},
		"age over the cap":             {"GLOVO_LINK_MAX_AUTH_AGE", "16m"},
		"age zero":                     {"GLOVO_LINK_MAX_AUTH_AGE", "0s"},
		"age negative":                 {"GLOVO_LINK_MAX_AUTH_AGE", "-1m"},
		"age not a duration":           {"GLOVO_LINK_MAX_AUTH_AGE", "300"},
	} {
		bad := fullEnv()
		bad[kv[0]] = kv[1]
		if _, err := loadC2(env(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The link verifier is a different audience from the MCP one, needs no grant,
// and carries the freshness bound; the MCP verifier is untouched.
func TestC2LinkVerifierConfig(t *testing.T) {
	e := fullEnv()
	e["GLOVO_LINK_RESOURCE"] = "https://glovo.mctl.ai/link"
	c, err := loadC2(env(e))
	if err != nil {
		t.Fatal(err)
	}
	link := linkVerifierConfig(c, nil, false)
	mcp := verifierConfig(c, nil, false)
	if link.Resource != "https://glovo.mctl.ai/link" || link.Resource == mcp.Resource ||
		link.RequireGrant || link.MaxAuthAge != 5*time.Minute || link.Issuer != mcp.Issuer {
		t.Fatalf("link %+v mcp %+v", link, mcp)
	}
	if mcp.MaxAuthAge != 0 || !mcp.RequireGrant || mcp.Resource != config.Resource {
		t.Fatalf("the MCP verifier changed: %+v", mcp)
	}
}

// The store is wired with mctl-api's masked-identity lookup only when the
// consent route is configured; without it the preview refuses. The upload
// re-check is wired in both modes.
func TestC2StoreConfigWiresMaskedIdentity(t *testing.T) {
	c, err := loadC2(env(fullEnv()))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gate, err := consentgate.New(consentgate.Config{Source: linkflow.UnavailableConsent{}, Version: linkflow.ConsentVersion})
	if err != nil {
		t.Fatal(err)
	}
	none := storeConfig(c, nil, nil, nil, nil, linkflow.UnavailableConsent{}, gate, log)
	if none.MaskIdentity != nil || none.LinkConsent == nil {
		t.Fatalf("allowlist mode: mask=%v linkConsent=%v", none.MaskIdentity != nil, none.LinkConsent != nil)
	}
	api, err := linkflow.NewAPIConsent(linkflow.APIConsentConfig{BaseURL: "https://api.example", TokenFile: "/etc/mctl/consent-reader"}) //nolint:gosec // fake test path, not a credential
	if err != nil {
		t.Fatal(err)
	}
	with := storeConfig(c, nil, nil, nil, nil, api, gate, log)
	if with.MaskIdentity == nil || with.LinkConsent == nil {
		t.Fatalf("consent mode: mask=%v linkConsent=%v", with.MaskIdentity != nil, with.LinkConsent != nil)
	}
}

// GLOVO_C2_REQUIRE_ACCESS: unset, "" and "false" are off, "true" is on,
// anything else stops the boot, and "true" without the consent route (which
// carries the access state) is refused, not ignored.
func TestC2RequireAccessFlag(t *testing.T) {
	consentEnv := func() map[string]string {
		m := fullEnv()
		m["GLOVO_CONSENT_API_URL"] = "https://api.mctl.ai"
		m["GLOVO_CONSENT_TOKEN_FILE"] = "/var/run/secrets/mctl-api/token"
		return m
	}
	if c, err := loadC2(env(consentEnv())); err != nil || c.requireAccess {
		t.Fatalf("unset: requireAccess=%v %v", c.requireAccess, err)
	}
	for v, want := range map[string]bool{"": false, "false": false, "true": true} {
		m := consentEnv()
		m["GLOVO_C2_REQUIRE_ACCESS"] = v
		c, err := loadC2(env(m))
		if err != nil || c.requireAccess != want {
			t.Errorf("%q: requireAccess=%v %v, want %v", v, c.requireAccess, err, want)
			continue
		}
		// The flag reaches the one source every gate reads.
		src, admit, err := c2Access(c)
		if err != nil || !admit {
			t.Fatalf("%q: c2Access admit=%v %v", v, admit, err)
		}
		api, ok := src.(*linkflow.APIConsent)
		if !ok || api.RequiresAccess() != want {
			t.Errorf("%q: the consent source requires access = %v, want %v", v, ok && api.RequiresAccess(), want)
		}
	}
	for _, v := range []string{"1", "0", "TRUE", "True", "yes", "on", " true", "true ", "false ", "FALSE", "t"} {
		m := consentEnv()
		m["GLOVO_C2_REQUIRE_ACCESS"] = v
		if _, err := loadC2(env(m)); err == nil || !strings.Contains(err.Error(), "GLOVO_C2_REQUIRE_ACCESS") {
			t.Errorf("%q accepted: %v", v, err)
		}
	}
	// Allowlist-only mode has no access state to read.
	m := fullEnv()
	m["GLOVO_C2_REQUIRE_ACCESS"] = "true"
	if _, err := loadC2(env(m)); err == nil || !strings.Contains(err.Error(), "GLOVO_C2_REQUIRE_ACCESS") {
		t.Errorf("the flag without the consent route: %v", err)
	}
	if _, _, err := c2Access(c2Config{enabled: true, allowed: []string{"prn_owner"}, requireAccess: true}); err == nil {
		t.Error("c2Access built an allowlist-only gate with requireAccess set")
	}
	m["GLOVO_C2_REQUIRE_ACCESS"] = "false"
	if c, err := loadC2(env(m)); err != nil || c.requireAccess {
		t.Errorf("false without the consent route: %v", err)
	}
}
