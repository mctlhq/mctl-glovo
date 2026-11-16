package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

func TestC2CapsDefaultsMatchDesign(t *testing.T) {
	c, err := loadC2(env(fullEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if c.capacity != 10 || c.caps != tokenstore.DefaultCaps() {
		t.Fatalf("capacity %d caps %+v", c.capacity, c.caps)
	}
}

func TestC2CapsOffByDefaultReadsNothing(t *testing.T) {
	// A bad cap value must not matter while C2 is dark.
	if c, err := loadC2(env(map[string]string{"GLOVO_C2_CAPACITY": "0"})); err != nil || c.enabled {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestC2CapacityScalesGlobalCaps(t *testing.T) {
	m := fullEnv()
	m["GLOVO_C2_CAPACITY"] = "20"
	c, err := loadC2(env(m))
	if err != nil {
		t.Fatal(err)
	}
	d := tokenstore.DefaultCaps()
	if c.capacity != 20 || c.caps.GlobalRequestsPerMinute != 2*d.GlobalRequestsPerMinute ||
		c.caps.GlobalDecryptsPerHour != 2*d.GlobalDecryptsPerHour ||
		c.caps.LinkAttemptsGlobalHour != 2*d.LinkAttemptsGlobalHour ||
		c.caps.PIDRequestsPerMinute != d.PIDRequestsPerMinute {
		t.Fatalf("%d %+v", c.capacity, c.caps)
	}
}

func TestC2CapsRefuseBadValues(t *testing.T) {
	bad := []string{"", "0", "-1", "abc", "1.5", "21", "99999999999999999999", "0x10"}
	for _, v := range bad[1:] {
		m := fullEnv()
		m["GLOVO_C2_CAPACITY"] = v
		if _, err := loadC2(env(m)); err == nil || !strings.Contains(err.Error(), "GLOVO_C2_CAPACITY") {
			t.Errorf("capacity %q: %v", v, err)
		}
	}
	for _, e := range capEnvs {
		for _, v := range []string{"0", "-5", "x", "1e3", " ", "99999999999999999999"} {
			if v == " " {
				continue // whitespace-only trims to empty: Atoi fails, covered below
			}
			m := fullEnv()
			m[e.name] = v
			if _, err := loadC2(env(m)); err == nil || !strings.Contains(err.Error(), e.name) {
				t.Errorf("%s=%q: %v", e.name, v, err)
			}
		}
		m := fullEnv()
		m[e.name] = " "
		if _, err := loadC2(env(m)); err == nil {
			t.Errorf("%s blank accepted", e.name)
		}
		m[e.name] = itoa(e.max + 1)
		if _, err := loadC2(env(m)); err == nil {
			t.Errorf("%s above ceiling accepted", e.name)
		}
		m[e.name] = itoa(e.max)
		c, err := loadC2(env(m))
		if err != nil || e.get(c.caps) != e.max {
			t.Errorf("%s at ceiling: %v %d", e.name, err, e.get(c.caps))
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// withCaps is what buildLink calls: the configured caps, not the store's
// defaults, are what the store is built with.
func TestC2CapsReachTheStoreConfig(t *testing.T) {
	m := fullEnv()
	m["GLOVO_C2_CAP_PID_REQUESTS_PER_MINUTE"] = "7"
	c, err := loadC2(env(m))
	if err != nil {
		t.Fatal(err)
	}
	var cfg tokenstore.Config
	if err := withCaps(&cfg, c); err != nil {
		t.Fatal(err)
	}
	if cfg.Caps.PIDRequestsPerMinute != 7 || cfg.Caps == (tokenstore.Caps{}) {
		t.Fatalf("%+v", cfg.Caps)
	}
}

func TestC2WithCapsRefusesUnconfiguredCaps(t *testing.T) {
	var cfg tokenstore.Config
	if err := withCaps(&cfg, c2Config{enabled: true}); err == nil {
		t.Fatal("zero caps accepted")
	}
	if cfg.Caps != (tokenstore.Caps{}) {
		t.Fatal("caps set despite refusal")
	}
}
