package tokenstore

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestVaultAddressMustBeInCluster(t *testing.T) {
	tok := func(context.Context) (string, error) { return "t", nil }
	good := []string{
		"http://vault-active.vault.svc.cluster.local:8200",
		"https://vault.vault.svc.cluster.local",
		"http://vault-active.vault.svc.cluster.local:8200/",
	}
	for _, a := range good {
		if _, err := NewVaultTransit(TransitConfig{Addr: a, Token: tok}); err != nil {
			t.Errorf("%s refused: %v", a, err)
		}
	}
	bad := []string{
		"https://secrets.mctl.ai",
		"https://vault.mctl.ai:8200",
		"http://vault-active.vault.svc.cluster.local.evil.example",
		"http://svc.cluster.local",
		"http://vault.svc.cluster.local", // no namespace label
		"http://user:pw@vault-active.vault.svc.cluster.local:8200",
		"http://vault-active.vault.svc.cluster.local:8200/v1",
		"http://vault-active.vault.svc.cluster.local:8200?x=1",
		"ftp://vault-active.vault.svc.cluster.local",
		"http://127.0.0.1:8200",
		"http://vault-active..svc.cluster.local",
		"",
	}
	for _, a := range bad {
		if _, err := NewVaultTransit(TransitConfig{Addr: a, Token: tok}); !errors.Is(err, ErrTransitAddr) {
			t.Errorf("%q accepted: %v", a, err)
		}
	}
	if _, err := NewVaultTransit(TransitConfig{Addr: good[0]}); err == nil {
		t.Error("no token source accepted")
	}
}

func TestTransitRefusesRedirects(t *testing.T) {
	var hits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	vt, err := newVaultTransit(TransitConfig{Addr: redirector.URL, Token: func(context.Context) (string, error) { return "t", nil }}, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vt.Decrypt(context.Background(), []byte("c"), "vault:v1:AAAA"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("redirect: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("redirect followed")
	}
}

func TestKeyVersionOf(t *testing.T) {
	for ct, want := range map[string]int{"vault:v1:abc": 1, "vault:v12:x": 12} {
		if got, err := keyVersionOf(ct); err != nil || got != want {
			t.Errorf("%s: %d %v", ct, got, err)
		}
	}
	for _, ct := range []string{"", "vault:v0:x", "vault:v01:x", "vault:v1:", "vault:vx:y", "v1:abc"} {
		if _, err := keyVersionOf(ct); err == nil {
			t.Errorf("%q parsed", ct)
		}
	}
}

// Vault error bodies are never copied into errors: the fake echoes the
// request (plaintext, ciphertext, context) on every failure.
func TestTransitErrorsCarryNoBodies(t *testing.T) {
	ft := newFakeTransit(t)
	vt := ft.client(t)
	ctx := context.Background()
	const mk = "ZQXJ-TRANSIT"
	b64 := base64.StdEncoding.EncodeToString([]byte(mk))
	check := func(err error) {
		t.Helper()
		if err == nil {
			t.Fatal("no error")
		}
		s := fmt.Sprintf("%v %+v", err, err)
		if strings.Contains(s, mk) || strings.Contains(s, b64) || strings.Contains(s, "vault:v") || strings.Contains(s, "echo") {
			t.Fatalf("error carries a body: %s", s)
		}
	}
	ft.fail500.Store(true)
	_, err := vt.Encrypt(ctx, []byte(mk), []byte(mk))
	check(err)
	_, err = vt.Decrypt(ctx, []byte(mk), "vault:v1:"+mk)
	check(err)
	_, err = vt.Rewrap(ctx, []byte(mk), "vault:v1:"+mk)
	check(err)
	ft.fail500.Store(false)
	_, err = vt.Decrypt(ctx, []byte(mk), "vault:v1:"+b64)
	check(err)
}
