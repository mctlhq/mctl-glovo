package glovoapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"strings"
	"testing"
	"time"
)

func validatorFor(t *testing.T, f *fakeGlovo, p *testProxy) func(context.Context, Session) error {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(f.srv.Certificate())
	v, err := NewSessionValidator(
		WithBaseURL(f.srv.URL),
		WithDefaultLocation(testLocation()),
		WithClock(func() time.Time { return testNow }),
		WithProxy(p.url(t)),
		withProxyTLSConfig(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// The validating call is one basket GET, tunnelled through the proxy.
func TestSessionValidatorGoesThroughProxy(t *testing.T) {
	f, remotes := newFakeTLS(t)
	p := newTestProxy(t)
	validate := validatorFor(t, f, p)
	if err := validate(context.Background(), testSession()); err != nil {
		t.Fatal(err)
	}
	if n := f.count("GET", "/v1/authenticated/customers/4242/baskets"); n != 1 {
		t.Fatalf("%d basket GETs, want 1", n)
	}
	if len(f.requests()) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(f.requests()))
	}
	host := strings.TrimPrefix(f.srv.URL, "https://")
	for _, l := range p.log() {
		if l != "CONNECT "+host {
			t.Errorf("proxy log %q, want only CONNECT %s", l, host)
		}
	}
	for _, r := range remotes() {
		if !p.dialledFrom(r) {
			t.Errorf("upstream connection from %s was not opened by the proxy", r)
		}
	}
}

// A rejected session is refused with the fixed error and is never
// refreshed: the stored session must be the uploaded one.
func TestSessionValidatorNeverRefreshes(t *testing.T) {
	f, _ := newFakeTLS(t)
	p := newTestProxy(t)
	validate := validatorFor(t, f, p)
	f.on("GET", "/v1/authenticated/customers/4242/baskets", reply{401, `{"error":"secret-glovo-detail"}`})
	err := validate(context.Background(), testSession())
	if !errors.Is(err, ErrValidationFailed) || strings.Contains(err.Error(), "secret-glovo-detail") {
		t.Fatalf("err = %v", err)
	}
	if n := f.count("POST", "/oauth/refresh"); n != 0 {
		t.Fatalf("%d refreshes during validation", n)
	}
	// An expired access token would need a refresh first: refused too.
	err = validate(context.Background(), expiredSession())
	if !errors.Is(err, ErrValidationFailed) {
		t.Fatalf("expired session: %v", err)
	}
	if n := f.count("POST", "/oauth/refresh"); n != 0 {
		t.Fatalf("%d refreshes for an expired session", n)
	}
}

// Without a proxy there is no validator at all.
func TestSessionValidatorRequiresProxy(t *testing.T) {
	if _, err := NewSessionValidator(WithBaseURL("https://glovo.invalid")); !errors.Is(err, ErrProxyRequired) {
		t.Fatalf("err = %v, want ErrProxyRequired", err)
	}
}

// The caller's map is not handed to the client.
func TestSessionValidatorDoesNotMutateInput(t *testing.T) {
	f, _ := newFakeTLS(t)
	p := newTestProxy(t)
	validate := validatorFor(t, f, p)
	s := testSession()
	before := len(s)
	if err := validate(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if len(s) != before {
		t.Fatalf("session changed: %d -> %d keys", before, len(s))
	}
}
