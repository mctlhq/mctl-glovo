package linkflow

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestClientIP(t *testing.T) {
	h := &Handler{cfg: Config{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}}
	for _, tc := range []struct {
		name, remote string
		xff          []string
		want         string
	}{
		{"untrusted peer ignores XFF", "203.0.113.9:1", []string{"198.51.100.1"}, "203.0.113.9"},
		{"trusted peer, one hop", "10.1.2.3:1", []string{"198.51.100.1"}, "198.51.100.1"},
		{"spoofed left entry ignored", "10.1.2.3:1", []string{"1.1.1.1, 198.51.100.1"}, "198.51.100.1"},
		{"trusted chain skipped", "10.1.2.3:1", []string{"198.51.100.1, 10.9.9.9"}, "198.51.100.1"},
		{"two headers", "10.1.2.3:1", []string{"1.1.1.1", "198.51.100.2"}, "198.51.100.2"},
		{"garbage hop fails closed", "10.1.2.3:1", []string{"not-an-ip"}, ""},
		{"no XFF from trusted peer", "10.1.2.3:1", nil, ""},
		{"only trusted hops", "10.1.2.3:1", []string{"10.9.9.9, 10.8.8.8"}, ""},
		{"empty XFF from trusted peer", "10.1.2.3:1", []string{""}, ""},
		{"mapped v4", "[::ffff:203.0.113.9]:1", nil, "203.0.113.9"},
		{"bad remote", "nonsense", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/link/preview", nil)
			r.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := h.clientIP(r); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	// Without trusted proxies, XFF is never believed.
	h0 := &Handler{}
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "10.1.2.3:1"
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	if got := h0.clientIP(r); got != "10.1.2.3" {
		t.Fatalf("got %q", got)
	}
}
