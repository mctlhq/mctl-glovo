package tokenstore

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Transit is the subset of Vault transit the store uses: encrypt, decrypt
// and rewrap with one derived key. It has no method that reads, rotates,
// exports or configures a key.
type Transit interface {
	Encrypt(ctx context.Context, keyContext, plaintext []byte) (string, error)
	Decrypt(ctx context.Context, keyContext []byte, ciphertext string) ([]byte, error)
	Rewrap(ctx context.Context, keyContext []byte, ciphertext string) (string, error)
}

// Fixed Vault object names (design 2.2). They are not configurable: the
// app policy grants exactly these three paths.
const (
	TransitMount = "glovo-transit"
	TransitKey   = "glovo-session"
)

// TokenSource returns the current Vault token. The production source logs
// in with the projected ServiceAccount token (Kubernetes auth, C2-1b) and
// keeps the Vault token in memory only.
type TokenSource func(ctx context.Context) (string, error)

// TransitConfig configures a VaultTransit.
type TransitConfig struct {
	// Addr is the in-cluster Vault address, for example
	// http://vault-active.vault.svc.cluster.local:8200. Anything that is
	// not http(s)://<host>.svc.cluster.local[:port] is refused, so
	// secrets.mctl.ai and every other address outside the cluster are.
	Addr  string
	Token TokenSource
	// Timeout bounds each Vault request. Zero means 10 s.
	Timeout time.Duration
}

// VaultTransit is a Transit over Vault's HTTP API, standard library only.
type VaultTransit struct {
	base  string
	token TokenSource
	http  *http.Client
}

// ErrTransitAddr is returned for a Vault address outside the cluster.
var ErrTransitAddr = errors.New("tokenstore: vault address must be http(s)://<service>.<namespace>.svc.cluster.local[:port]")

const maxVaultBody = 1 << 20

// NewVaultTransit validates cfg and returns a client.
func NewVaultTransit(cfg TransitConfig) (*VaultTransit, error) {
	return newVaultTransit(cfg, false)
}

// newVaultTransit with allowLoopback accepts a loopback httptest server.
// Only tests in this package pass true.
func newVaultTransit(cfg TransitConfig, allowLoopback bool) (*VaultTransit, error) {
	base, err := checkVaultAddr(cfg.Addr, allowLoopback)
	if err != nil {
		return nil, err
	}
	if cfg.Token == nil {
		return nil, errors.New("tokenstore: vault token source is required")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	tr := &http.Transport{
		// Never the environment's proxy: plaintext sessions travel on
		// encrypt and decrypt.
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second,
		MaxIdleConns:        4,
		IdleConnTimeout:     90 * time.Second,
	}
	return &VaultTransit{
		base:  base,
		token: cfg.Token,
		http: &http.Client{
			Transport: tr,
			Timeout:   timeout,
			// A redirect would carry the token and the plaintext elsewhere.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func checkVaultAddr(addr string, allowLoopback bool) (string, error) {
	u, err := url.Parse(addr)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", ErrTransitAddr
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", ErrTransitAddr
	}
	if u.Path != "" && u.Path != "/" {
		return "", ErrTransitAddr
	}
	host := strings.ToLower(u.Hostname())
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return "", ErrTransitAddr
		}
	}
	ok := false
	if labels := strings.Split(host, "."); len(labels) >= 5 && strings.HasSuffix(host, ".svc.cluster.local") {
		ok = true
		for _, l := range labels {
			if l == "" {
				ok = false
			}
		}
	}
	if !ok && allowLoopback {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			ok = true
		}
	}
	if !ok {
		return "", ErrTransitAddr
	}
	return u.Scheme + "://" + u.Host, nil
}

type transitData struct {
	Ciphertext string `json:"ciphertext"`
	Plaintext  string `json:"plaintext"`
}

// errTransitRejected is a 400 from transit: Vault refused the input
// (wrong context, forged or malformed ciphertext). It maps to ErrIntegrity.
var errTransitRejected = errors.New("transit rejected the ciphertext")

func (v *VaultTransit) call(ctx context.Context, op string, body map[string]string) (transitData, error) {
	tok, err := v.token(ctx)
	if err != nil || tok == "" {
		return transitData{}, fmt.Errorf("%w: vault token unavailable", ErrUnavailable)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return transitData{}, fmt.Errorf("%w: encode transit request", ErrUnavailable)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.base+"/v1/"+TransitMount+"/"+op+"/"+TransitKey, bytes.NewReader(raw))
	if err != nil {
		return transitData{}, fmt.Errorf("%w: build transit request", ErrUnavailable)
	}
	req.Header.Set("X-Vault-Token", tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := v.http.Do(req)
	if err != nil {
		// *url.Error names the URL only; still, report a fixed text.
		return transitData{}, fmt.Errorf("%w: vault %s request failed", ErrUnavailable, op)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxVaultBody))
	if err != nil {
		return transitData{}, fmt.Errorf("%w: vault %s read failed", ErrUnavailable, op)
	}
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusBadRequest && op != "encrypt":
		return transitData{}, errTransitRejected
	default:
		// The body is never echoed: Vault errors are not ours to log.
		return transitData{}, fmt.Errorf("%w: vault %s returned %d", ErrUnavailable, op, resp.StatusCode)
	}
	var out struct {
		Data transitData `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return transitData{}, fmt.Errorf("%w: vault %s malformed response", ErrUnavailable, op)
	}
	return out.Data, nil
}

// Encrypt implements Transit.
func (v *VaultTransit) Encrypt(ctx context.Context, keyContext, plaintext []byte) (string, error) {
	d, err := v.call(ctx, "encrypt", map[string]string{
		"plaintext": base64.StdEncoding.EncodeToString(plaintext),
		"context":   base64.StdEncoding.EncodeToString(keyContext),
	})
	if err != nil {
		return "", err
	}
	if _, err := keyVersionOf(d.Ciphertext); err != nil {
		return "", fmt.Errorf("%w: vault encrypt malformed ciphertext", ErrUnavailable)
	}
	return d.Ciphertext, nil
}

// Decrypt implements Transit.
func (v *VaultTransit) Decrypt(ctx context.Context, keyContext []byte, ciphertext string) ([]byte, error) {
	d, err := v.call(ctx, "decrypt", map[string]string{
		"ciphertext": ciphertext,
		"context":    base64.StdEncoding.EncodeToString(keyContext),
	})
	if err != nil {
		return nil, err
	}
	pt, err := base64.StdEncoding.DecodeString(d.Plaintext)
	if err != nil {
		return nil, fmt.Errorf("%w: vault decrypt malformed plaintext", ErrUnavailable)
	}
	return pt, nil
}

// Rewrap implements Transit.
func (v *VaultTransit) Rewrap(ctx context.Context, keyContext []byte, ciphertext string) (string, error) {
	d, err := v.call(ctx, "rewrap", map[string]string{
		"ciphertext": ciphertext,
		"context":    base64.StdEncoding.EncodeToString(keyContext),
	})
	if err != nil {
		return "", err
	}
	if _, err := keyVersionOf(d.Ciphertext); err != nil {
		return "", fmt.Errorf("%w: vault rewrap malformed ciphertext", ErrUnavailable)
	}
	return d.Ciphertext, nil
}

// keyVersionOf parses N from "vault:vN:...".
func keyVersionOf(ct string) (int, error) {
	rest, ok := strings.CutPrefix(ct, "vault:v")
	if !ok {
		return 0, errors.New("no vault prefix")
	}
	num, body, ok := strings.Cut(rest, ":")
	if !ok || body == "" {
		return 0, errors.New("no key version")
	}
	n, err := strconv.Atoi(num)
	if err != nil || n < 1 || strconv.Itoa(n) != num {
		return 0, errors.New("bad key version")
	}
	return n, nil
}

// keyContext is the transit derivation context of a row (design 2.3).
func keyContext(pid PID, linkID string) []byte {
	return []byte("mctl-glovo/session/v1|" + pid.id + "|" + linkID)
}
