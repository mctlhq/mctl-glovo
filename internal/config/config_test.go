package config

import (
	"slices"
	"strings"
	"testing"
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
