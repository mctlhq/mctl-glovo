package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func load(t *testing.T, env map[string]string, environ ...string) (*Config, error) {
	t.Helper()
	base := map[string]string{"GLOVO_HOME_NODE": "/n", "GLOVO_HOME_SERVER": "/s"}
	for k, v := range env {
		base[k] = v
	}
	return Load(func(k string) string { return base[k] }, environ)
}

func TestListenLoopbackOnly(t *testing.T) {
	for addr, ok := range map[string]bool{
		"": true, "127.0.0.1:8787": true, "[::1]:8787": true, "localhost:1": true,
		":8787": false, "0.0.0.0:8787": false, "192.168.1.5:8787": false, "example.com:80": false,
	} {
		_, err := load(t, map[string]string{"GLOVO_HOME_LISTEN": addr})
		if (err == nil) != ok {
			t.Errorf("%q: err=%v, want ok=%v", addr, err, ok)
		}
	}
	if _, err := load(t, map[string]string{"GLOVO_HOME_LISTEN": "0.0.0.0:8787", "GLOVO_HOME_ALLOW_NON_LOOPBACK": "true"}); err != nil {
		t.Fatalf("opt-in refused: %v", err)
	}
}

func TestAllowlist(t *testing.T) {
	c, err := load(t, map[string]string{"GLOVO_HOME_ALLOWED_PRINCIPALS": " prn_a, ,prn_b "})
	if err != nil || !slices.Equal(c.AllowedPrincipals, []string{"prn_a", "prn_b"}) {
		t.Fatalf("%v %v", c, err)
	}
	c, err = load(t, nil)
	if err != nil || len(c.AllowedPrincipals) != 0 {
		t.Fatalf("unset: %v %v", c, err)
	}
	if _, err := load(t, map[string]string{"GLOVO_HOME_ALLOWED_PRINCIPALS": "alice"}); err == nil {
		t.Fatal("non-prn id accepted")
	}
}

func TestKeepaliveAndAlert(t *testing.T) {
	c, err := load(t, map[string]string{"HOME": "/h"})
	if err != nil || c.Keepalive != 24*time.Hour || c.SessionPath != "/h/.glovo/session.json" || c.ReloadMarker != "/h/.glovo/session.json.reload" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	c, err = load(t, map[string]string{"GLOVO_HOME_KEEPALIVE": "0", "GLOVO_SESSION_PATH": "/s.json"})
	if err != nil || c.Keepalive != 0 || c.SessionPath != "/s.json" {
		t.Fatalf("off: %+v %v", c, err)
	}
	for _, bad := range []map[string]string{
		{"GLOVO_HOME_KEEPALIVE": "10s"}, {"GLOVO_HOME_KEEPALIVE": "-1h"}, {"GLOVO_HOME_KEEPALIVE": "daily"},
		{"GLOVO_HOME_ALERT_CMD": "notify.sh"}, {"GLOVO_HOME_ALERT_CMD": "curl x | sh"},
	} {
		if _, err := load(t, bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestRequired(t *testing.T) {
	if _, err := Load(func(string) string { return "" }, nil); err == nil {
		t.Fatal("missing node/server accepted")
	}
}

func TestChildEnv(t *testing.T) {
	c, err := load(t, nil,
		"HOME=/h", "PATH=/p", "GLOVO_SESSION_PATH=/s.json", "GLOVO_HOME_ALLOWED_PRINCIPALS=prn_x",
		"AWS_SECRET_ACCESS_KEY=x", "PLAYWRIGHT_BROWSERS_PATH=/b", "GH_TOKEN=y")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(c.ChildEnv, " ")
	if got != "HOME=/h PATH=/p GLOVO_SESSION_PATH=/s.json PLAYWRIGHT_BROWSERS_PATH=/b" {
		t.Fatalf("child env = %s", got)
	}
}

func TestChildStderrOptInAndExtraTools(t *testing.T) {
	c, err := load(t, nil)
	if err != nil || c.ChildStderr || len(c.ExtraTools) != 0 {
		t.Fatalf("defaults: stderr=%v extra=%v %v", c.ChildStderr, c.ExtraTools, err)
	}
	c, err = load(t, map[string]string{"GLOVO_HOME_CHILD_STDERR": "inherit", "GLOVO_HOME_EXTRA_TOOLS": "glovo_new_read, glovo_other"})
	if err != nil || !c.ChildStderr || len(c.ExtraTools) != 2 || c.ExtraTools[0] != "glovo_new_read" {
		t.Fatalf("opt-in: stderr=%v extra=%v %v", c.ChildStderr, c.ExtraTools, err)
	}
}

func TestBackendSelection(t *testing.T) {
	c, err := load(t, map[string]string{"HOME": "/h"})
	if err != nil || c.Backend != BackendSkill || c.Node != "/n" {
		t.Fatalf("default: %+v %v", c, err)
	}
	c, err = load(t, map[string]string{"HOME": "/h", "GLOVO_HOME_BACKEND": "skill"})
	if err != nil || c.Backend != BackendSkill {
		t.Fatalf("skill: %+v %v", c, err)
	}
	// Native needs no child, and never keeps a child command around.
	env := func(k string) string {
		return map[string]string{"HOME": "/h", "GLOVO_HOME_BACKEND": "native", "GOOGLE_MAPS_API_KEY": "k"}[k]
	}
	c, err = Load(env, nil)
	if err != nil || c.Backend != BackendNative || c.Node != "" || c.Server != "" || c.SessionPath != "/h/.glovo/session.json" || c.GoogleMapsAPIKey != "k" {
		t.Fatalf("native: %+v %v", c, err)
	}
	c, err = load(t, map[string]string{"HOME": "/h", "GLOVO_HOME_BACKEND": "native"})
	if err != nil || c.Node != "" || c.Server != "" {
		t.Fatalf("native with node set: %+v %v", c, err)
	}
	// The skill backend does not read the Google key: glovo-skill gets its
	// environment from ChildEnv only.
	c, err = load(t, map[string]string{"HOME": "/h", "GOOGLE_MAPS_API_KEY": "k"})
	if err != nil || c.GoogleMapsAPIKey != "" {
		t.Fatalf("skill google key: %+v %v", c, err)
	}
	for _, bad := range []map[string]string{
		{"HOME": "/h", "GLOVO_HOME_BACKEND": "both"},
		{"HOME": "/h", "GLOVO_HOME_BACKEND": "Native"},
		{"GLOVO_HOME_BACKEND": "native"}, // no session path
	} {
		if _, err := load(t, bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if _, err := Load(func(k string) string { return map[string]string{"GLOVO_HOME_BACKEND": "skill"}[k] }, nil); err == nil {
		t.Error("skill without node/server accepted")
	}
}
