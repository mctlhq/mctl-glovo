package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

// Capacity bounds (owner decision 2026-11-16): a friends cohort of 10-20.
const (
	defaultC2Capacity = 10
	maxC2Capacity     = 20
)

// capEnv names one tunable cap: its env var, the ceiling an operator may
// set, and the field it fills. The ceilings are about twice the value that
// 20 users need, so a typo (an extra zero) is refused rather than turned
// into an effectively uncapped service.
type capEnv struct {
	name string
	max  int
	set  func(*tokenstore.Caps, int)
	get  func(tokenstore.Caps) int
}

var capEnvs = []capEnv{
	{"GLOVO_C2_CAP_PID_REQUESTS_PER_MINUTE", 300, func(c *tokenstore.Caps, v int) { c.PIDRequestsPerMinute = v }, func(c tokenstore.Caps) int { return c.PIDRequestsPerMinute }},
	{"GLOVO_C2_CAP_PID_REQUESTS_PER_DAY", 10000, func(c *tokenstore.Caps, v int) { c.PIDRequestsPerDay = v }, func(c tokenstore.Caps) int { return c.PIDRequestsPerDay }},
	{"GLOVO_C2_CAP_GLOBAL_REQUESTS_PER_MINUTE", 600, func(c *tokenstore.Caps, v int) { c.GlobalRequestsPerMinute = v }, func(c tokenstore.Caps) int { return c.GlobalRequestsPerMinute }},
	{"GLOVO_C2_CAP_PID_DECRYPTS_PER_HOUR", 120, func(c *tokenstore.Caps, v int) { c.PIDDecryptsPerHour = v }, func(c tokenstore.Caps) int { return c.PIDDecryptsPerHour }},
	{"GLOVO_C2_CAP_GLOBAL_DECRYPTS_PER_HOUR", 600, func(c *tokenstore.Caps, v int) { c.GlobalDecryptsPerHour = v }, func(c tokenstore.Caps) int { return c.GlobalDecryptsPerHour }},
	{"GLOVO_C2_CAP_LINK_ATTEMPTS_PER_IP_HOUR", 60, func(c *tokenstore.Caps, v int) { c.LinkAttemptsPerIPHour = v }, func(c tokenstore.Caps) int { return c.LinkAttemptsPerIPHour }},
	{"GLOVO_C2_CAP_LINK_ATTEMPTS_GLOBAL_HOUR", 300, func(c *tokenstore.Caps, v int) { c.LinkAttemptsGlobalHour = v }, func(c tokenstore.Caps) int { return c.LinkAttemptsGlobalHour }},
	{"GLOVO_C2_CAP_REFRESH_FAILURES_IN_A_ROW", 10, func(c *tokenstore.Caps, v int) { c.RefreshFailuresInARow = v }, func(c tokenstore.Caps) int { return c.RefreshFailuresInARow }},
}

// parseCapInt reads a positive decimal integer. Empty is the caller's
// concern; anything else that is not a plain positive integer is an error.
func parseCapInt(name, s string) (int, error) {
	s = strings.TrimSpace(s)
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not an integer", name, s)
	}
	if v <= 0 {
		return 0, fmt.Errorf("%s: %d must be positive (no limit is not a configuration)", name, v)
	}
	return v, nil
}

// capsForCapacity scales the cohort-wide caps with the cohort size. Ten
// users reproduce tokenstore.DefaultCaps exactly; per-pid caps do not scale.
func capsForCapacity(n int) tokenstore.Caps {
	c := tokenstore.DefaultCaps()
	c.GlobalRequestsPerMinute = 12 * n
	c.GlobalDecryptsPerHour = 10 * n
	c.LinkAttemptsGlobalHour = 5 * n
	return c
}

// loadC2Caps returns the capacity and the caps for it. GLOVO_C2_CAPACITY
// defaults to 10 and may be 1..20. Each GLOVO_C2_CAP_* overrides one cap
// and must be a positive integer within its ceiling. Any malformed, zero,
// negative or oversized value is an error: the service must not start
// rather than start uncapped or with a silently different limit.
func loadC2Caps(getenv func(string) string) (int, tokenstore.Caps, error) {
	capacity := defaultC2Capacity
	if s := getenv("GLOVO_C2_CAPACITY"); s != "" {
		v, err := parseCapInt("GLOVO_C2_CAPACITY", s)
		if err != nil {
			return 0, tokenstore.Caps{}, err
		}
		if v > maxC2Capacity {
			return 0, tokenstore.Caps{}, fmt.Errorf("GLOVO_C2_CAPACITY: %d exceeds %d (a larger cohort is a new owner decision)", v, maxC2Capacity)
		}
		capacity = v
	}
	caps := capsForCapacity(capacity)
	for _, e := range capEnvs {
		s := getenv(e.name)
		if s == "" {
			continue
		}
		v, err := parseCapInt(e.name, s)
		if err != nil {
			return 0, tokenstore.Caps{}, err
		}
		if v > e.max {
			return 0, tokenstore.Caps{}, fmt.Errorf("%s: %d exceeds the ceiling %d", e.name, v, e.max)
		}
		e.set(&caps, v)
	}
	return capacity, caps, nil
}
