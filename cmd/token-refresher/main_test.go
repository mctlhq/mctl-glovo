package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envFor(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func baseEnv(t *testing.T) (map[string]string, string) {
	t.Helper()
	dir := t.TempDir()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	kf := filepath.Join(dir, "key")
	if err := os.WriteFile(kf, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"ZITADEL_ISSUER": "https://auth.example.test", "ZITADEL_USER_ID": "u", "ZITADEL_KEY_ID": "k",
		"ZITADEL_SCOPE": "openid", "KEY_FILE": kf, "TOKEN_FILE": filepath.Join(dir, "token"),
	}, dir
}

func TestConfigErrorsExitTwoWithoutLeaking(t *testing.T) {
	for name, mut := range map[string]func(map[string]string){
		"http issuer": func(m map[string]string) { m["ZITADEL_ISSUER"] = "http://auth.example.test" },
		"no scope":    func(m map[string]string) { delete(m, "ZITADEL_SCOPE") },
		"bad mode":    func(m map[string]string) { m["TOKEN_FILE_MODE"] = "rw" },
		"key garbage": func(m map[string]string) { _ = os.WriteFile(m["KEY_FILE"], []byte("TOPSECRETKEY"), 0o600) },
		"no key file": func(m map[string]string) { m["KEY_FILE"] = "/nonexistent/key" },
	} {
		t.Run(name, func(t *testing.T) {
			m, _ := baseEnv(t)
			mut(m)
			var out, errb bytes.Buffer
			if code := run(context.Background(), envFor(m), &out, &errb); code != 2 {
				t.Fatalf("exit %d, want 2 (stderr %q)", code, errb.String())
			}
			if strings.Contains(out.String()+errb.String(), "TOPSECRET") || strings.Contains(out.String()+errb.String(), "PRIVATE") {
				t.Fatal("output leaks key material")
			}
		})
	}
}

func TestCancelledContextExitsZeroAndClearsStaleToken(t *testing.T) {
	m, _ := baseEnv(t)
	if err := os.WriteFile(m["TOKEN_FILE"], []byte("stale"), 0o644); err != nil { //nolint:gosec // test file
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errb bytes.Buffer
	if code := run(ctx, envFor(m), &out, &errb); code != 0 {
		t.Fatalf("exit %d (stderr %q)", code, errb.String())
	}
	if _, err := os.Stat(m["TOKEN_FILE"]); err == nil {
		t.Fatal("stale token file survived start")
	}
}
