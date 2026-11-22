package main

import (
	"testing"
	"time"
)

func TestKeepaliveOffByDefault(t *testing.T) {
	for _, v := range []string{"", "0", " 0 "} {
		e := fullEnv()
		if v != "" {
			e["GLOVO_C2_KEEPALIVE_INTERVAL"] = v
		}
		c, err := loadC2(env(e))
		if err != nil || c.keepalive != 0 {
			t.Errorf("%q: keepalive %v, err %v", v, c.keepalive, err)
		}
	}
	// With C2 off the variable is not even read.
	if c, err := loadC2(env(map[string]string{"GLOVO_C2_KEEPALIVE_INTERVAL": "garbage"})); err != nil || c.keepalive != 0 {
		t.Errorf("C2 off: %v %v", c.keepalive, err)
	}
}

func TestKeepaliveIntervalParsing(t *testing.T) {
	ok := map[string]time.Duration{"1h": time.Hour, "24h": 24 * time.Hour, "6h30m": 6*time.Hour + 30*time.Minute}
	for in, want := range ok {
		e := fullEnv()
		e["GLOVO_C2_KEEPALIVE_INTERVAL"] = in
		c, err := loadC2(env(e))
		if err != nil || c.keepalive != want {
			t.Errorf("%q: %v, %v", in, c.keepalive, err)
		}
	}
	for _, in := range []string{"59m", "1m", "25h", "24h1s", "-1h", "abc", "1", "86400", "true", "1d"} {
		e := fullEnv()
		e["GLOVO_C2_KEEPALIVE_INTERVAL"] = in
		if _, err := loadC2(env(e)); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}
