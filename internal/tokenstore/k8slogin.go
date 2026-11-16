package tokenstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// KubernetesLoginConfig configures NewKubernetesLogin (design 3.1).
type KubernetesLoginConfig struct {
	// Addr is the in-cluster Vault address; the same rule as
	// TransitConfig.Addr applies.
	Addr string
	// Role is the Kubernetes auth role, mctl-glovo.
	Role string
	// JWTPath is the projected ServiceAccount token. Zero means
	// /var/run/secrets/kubernetes.io/serviceaccount/token.
	JWTPath string
	// Timeout bounds the login request. Zero means 10 s.
	Timeout time.Duration
}

// DefaultSATokenPath is where Kubernetes projects the ServiceAccount token.
const DefaultSATokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token" // #nosec G101 -- a path, not a credential

var vaultRolePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// kubeLogin logs in to Vault with the ServiceAccount token and keeps the
// Vault token in memory only. It logs in again at 80% of the lease instead
// of renewing: a login needs no renew-self, and the role's token_max_ttl
// would force a login within the hour anyway. A failed login is never
// cached; the next call tries again.
type kubeLogin struct {
	base, role, jwtPath string
	http                *http.Client
	now                 func() time.Time

	mu    sync.Mutex
	token string
	until time.Time
}

// NewKubernetesLogin returns a TokenSource for VaultTransit.
func NewKubernetesLogin(cfg KubernetesLoginConfig) (TokenSource, error) {
	k, err := newKubeLogin(cfg, false)
	if err != nil {
		return nil, err
	}
	return k.Token, nil
}

func newKubeLogin(cfg KubernetesLoginConfig, allowLoopback bool) (*kubeLogin, error) {
	base, err := checkVaultAddr(cfg.Addr, allowLoopback)
	if err != nil {
		return nil, err
	}
	if !vaultRolePattern.MatchString(cfg.Role) {
		return nil, errors.New("tokenstore: invalid vault kubernetes role")
	}
	jwtPath := cfg.JWTPath
	if jwtPath == "" {
		jwtPath = DefaultSATokenPath
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &kubeLogin{
		base:    base,
		role:    cfg.Role,
		jwtPath: jwtPath,
		now:     time.Now,
		http: &http.Client{
			Transport: &http.Transport{
				Proxy:               nil,
				DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				TLSHandshakeTimeout: 5 * time.Second,
			},
			Timeout:       timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// Token implements TokenSource.
func (k *kubeLogin) Token(ctx context.Context) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.token != "" && k.now().Before(k.until) {
		return k.token, nil
	}
	k.token, k.until = "", time.Time{}
	tok, lease, err := k.login(ctx)
	if err != nil {
		return "", err
	}
	k.token = tok
	k.until = k.now().Add(lease * 8 / 10)
	return tok, nil
}

func (k *kubeLogin) login(ctx context.Context) (string, time.Duration, error) {
	jwt, err := os.ReadFile(k.jwtPath)
	if err != nil || len(bytes.TrimSpace(jwt)) == 0 {
		return "", 0, fmt.Errorf("%w: service account token unreadable", ErrUnavailable)
	}
	body, err := json.Marshal(map[string]string{"role": k.role, "jwt": strings.TrimSpace(string(jwt))})
	if err != nil {
		return "", 0, fmt.Errorf("%w: encode vault login", ErrUnavailable)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, k.base+"/v1/auth/kubernetes/login", bytes.NewReader(body))
	if err != nil {
		return "", 0, fmt.Errorf("%w: build vault login", ErrUnavailable)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := k.http.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("%w: vault login request failed", ErrUnavailable)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxVaultBody))
	if err != nil {
		return "", 0, fmt.Errorf("%w: vault login read failed", ErrUnavailable)
	}
	if resp.StatusCode != http.StatusOK {
		// The body is never echoed.
		return "", 0, fmt.Errorf("%w: vault login returned %d", ErrUnavailable, resp.StatusCode)
	}
	var out struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int64  `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.Auth.ClientToken == "" || out.Auth.LeaseDuration <= 0 {
		return "", 0, fmt.Errorf("%w: vault login malformed response", ErrUnavailable)
	}
	return out.Auth.ClientToken, time.Duration(out.Auth.LeaseDuration) * time.Second, nil
}
