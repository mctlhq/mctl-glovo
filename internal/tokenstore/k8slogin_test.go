package tokenstore

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakeVaultLogin struct {
	srv    *httptest.Server
	logins atomic.Int64
	status atomic.Int64
	lease  atomic.Int64
	gotJWT atomic.Value
}

func newFakeVaultLogin(t *testing.T) *fakeVaultLogin {
	t.Helper()
	f := &fakeVaultLogin{}
	f.status.Store(200)
	f.lease.Store(900)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/auth/kubernetes/login" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["role"] != "mctl-glovo" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.gotJWT.Store(body["jwt"])
		n := f.logins.Add(1)
		if s := int(f.status.Load()); s != 200 {
			w.WriteHeader(s)
			_, _ = w.Write([]byte(`{"errors":["echo ` + body["jwt"] + `"]}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{
			"client_token": "hvs.test-" + strconv.FormatInt(n, 10), "lease_duration": f.lease.Load(),
		}})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func writeJWT(t *testing.T, v string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(v+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestKubernetesLoginCachesUntil80Percent(t *testing.T) {
	f := newFakeVaultLogin(t)
	k, err := newKubeLogin(KubernetesLoginConfig{Addr: f.srv.URL, Role: "mctl-glovo", JWTPath: writeJWT(t, "sa-jwt-1")}, true)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_000_000, 0)
	k.now = func() time.Time { return now }
	ctx := context.Background()
	t1, err := k.Token(ctx)
	if err != nil || t1 == "" {
		t.Fatal(t1, err)
	}
	if got := f.gotJWT.Load(); got != "sa-jwt-1" {
		t.Fatalf("jwt sent = %v", got)
	}
	now = now.Add(719 * time.Second) // 80% of 900 s is 720 s
	if t2, _ := k.Token(ctx); t2 != t1 || f.logins.Load() != 1 {
		t.Fatalf("logged in again before 80%%: %d logins", f.logins.Load())
	}
	now = now.Add(2 * time.Second)
	if t3, _ := k.Token(ctx); t3 == t1 || f.logins.Load() != 2 {
		t.Fatalf("no login after 80%%: %d logins", f.logins.Load())
	}
}

func TestKubernetesLoginFailureIsNotCached(t *testing.T) {
	f := newFakeVaultLogin(t)
	k, err := newKubeLogin(KubernetesLoginConfig{Addr: f.srv.URL, Role: "mctl-glovo", JWTPath: writeJWT(t, "sa-jwt-secret")}, true)
	if err != nil {
		t.Fatal(err)
	}
	f.status.Store(403)
	_, err = k.Token(context.Background())
	if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "sa-jwt-secret") {
		t.Fatalf("err = %v", err)
	}
	f.status.Store(200)
	if tok, err := k.Token(context.Background()); err != nil || tok == "" {
		t.Fatalf("recovery: %q %v", tok, err)
	}
	if f.logins.Load() != 2 {
		t.Fatalf("%d logins, want 2", f.logins.Load())
	}
	f.lease.Store(0)
	k.token = ""
	if _, err := k.Token(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("zero lease accepted: %v", err)
	}
}

func TestKubernetesLoginRefusesOutsideCluster(t *testing.T) {
	for _, addr := range []string{"https://secrets.mctl.ai", "http://127.0.0.1:8200"} {
		if _, err := NewKubernetesLogin(KubernetesLoginConfig{Addr: addr, Role: "mctl-glovo"}); !errors.Is(err, ErrTransitAddr) {
			t.Errorf("%s: %v", addr, err)
		}
	}
	if _, err := NewKubernetesLogin(KubernetesLoginConfig{Addr: "http://vault-active.vault.svc.cluster.local:8200", Role: "Bad Role"}); err == nil {
		t.Error("bad role accepted")
	}
	k, err := newKubeLogin(KubernetesLoginConfig{Addr: "http://127.0.0.1:1", Role: "mctl-glovo", JWTPath: filepath.Join(t.TempDir(), "missing")}, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Token(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing jwt: %v", err)
	}
}
