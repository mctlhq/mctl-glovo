// Package config reads glovo-home's configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// Fixed identities. They are not configurable: a gateway that accepted a
// different issuer or audience would no longer be the glovo.mctl.ai resource.
const (
	Resource = "https://glovo.mctl.ai/mcp"
	Issuer   = "https://api.mctl.ai"
	JWKSURL  = "https://api.mctl.ai/.well-known/jwks.json"
)

// Config is glovo-home's configuration.
type Config struct {
	Listen            string
	AllowedPrincipals []string
	Node              string
	Server            string
	ChildEnv          []string
	ExtraBlocked      []string
	CallTimeout       time.Duration
	ChildStderr       bool
}

// childEnvKeys are inherited by the child verbatim when set. Everything
// else is dropped, apart from GLOVO_* variables that are not GLOVO_HOME_*.
var childEnvKeys = []string{"HOME", "PATH", "USER", "LOGNAME", "LANG", "LC_ALL", "TMPDIR", "TZ"}

// Load reads the configuration. getenv is os.Getenv; environ is os.Environ.
func Load(getenv func(string) string, environ []string) (*Config, error) {
	c := &Config{
		Listen:      getenv("GLOVO_HOME_LISTEN"),
		Node:        getenv("GLOVO_HOME_NODE"),
		Server:      getenv("GLOVO_HOME_SERVER"),
		CallTimeout: 3 * time.Minute,
		ChildStderr: getenv("GLOVO_HOME_CHILD_STDERR") != "discard",
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8787"
	}
	if err := checkLoopback(c.Listen, getenv("GLOVO_HOME_ALLOW_NON_LOOPBACK") == "true"); err != nil {
		return nil, err
	}
	if c.Node == "" || c.Server == "" {
		return nil, errors.New("GLOVO_HOME_NODE and GLOVO_HOME_SERVER are required")
	}
	c.AllowedPrincipals = splitList(getenv("GLOVO_HOME_ALLOWED_PRINCIPALS"))
	for _, p := range c.AllowedPrincipals {
		if !strings.HasPrefix(p, "prn_") {
			return nil, fmt.Errorf("GLOVO_HOME_ALLOWED_PRINCIPALS: %q is not a prn_ principal id", p)
		}
	}
	c.ExtraBlocked = splitList(getenv("GLOVO_HOME_BLOCKED_TOOLS"))
	if v := getenv("GLOVO_HOME_CALL_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("GLOVO_HOME_CALL_TIMEOUT: %q is not a positive duration", v)
		}
		c.CallTimeout = d
	}
	c.ChildEnv = childEnv(environ)
	return c, nil
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// checkLoopback refuses a listen address that is not loopback unless the
// operator opted in. An empty host (":8787") binds every interface.
func checkLoopback(addr string, allow bool) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("GLOVO_HOME_LISTEN: %w", err)
	}
	if _, err := strconv.Atoi(port); err != nil {
		return fmt.Errorf("GLOVO_HOME_LISTEN: bad port %q", port)
	}
	if allow {
		return nil
	}
	if host == "localhost" {
		return nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() {
		return fmt.Errorf("GLOVO_HOME_LISTEN %q is not a loopback address; set GLOVO_HOME_ALLOW_NON_LOOPBACK=true to allow it", addr)
	}
	return nil
}

func childEnv(environ []string) []string {
	out := []string{}
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(k, "GLOVO_HOME_"):
		case strings.HasPrefix(k, "GLOVO_"), strings.HasPrefix(k, "PLAYWRIGHT_"):
			out = append(out, kv)
		default:
			for _, want := range childEnvKeys {
				if k == want {
					out = append(out, kv)
				}
			}
		}
	}
	return out
}
