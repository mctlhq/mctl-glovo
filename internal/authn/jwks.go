package authn

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"
)

const (
	// jwksTTL is how long a fetched key set is used before a refresh is
	// attempted.
	jwksTTL = 10 * time.Minute
	// jwksMaxStale is how long a key set may keep verifying after refreshes
	// started failing. Past it the cache is unusable and every request is
	// answered 503: a revoked key must not verify forever.
	jwksMaxStale = 24 * time.Hour
	// jwksMinRefetch bounds refreshes, successful or not, so a flood of
	// tokens with random kids cannot turn into a flood of JWKS fetches.
	jwksMinRefetch   = 30 * time.Second
	jwksMaxBytes     = 64 << 10
	jwksFetchTimeout = 5 * time.Second
)

// ErrJWKSUnavailable means no usable key set could be obtained: the JWKS was
// never fetched, every refresh failed past jwksMaxStale, or the refetch for
// an unknown kid failed. It is "could not observe", answered with 503, never
// a verdict on the token.
var ErrJWKSUnavailable = errors.New("authn: jwks unavailable")

type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	Kid string `json:"kid"`
	X   string `json:"x"`
	D   string `json:"d"`
	Alg string `json:"alg"`
	Use string `json:"use"`
}

type jwksCache struct {
	url    string
	client *http.Client
	now    func() time.Time
	// rsa admits RS256 public keys next to Ed25519 ones. Off for the mctl-api
	// verifier (EdDSA only); the service verifier turns it on.
	rsa bool

	// flight serializes fetches so concurrent callers share one.
	flight sync.Mutex

	mu          sync.Mutex
	keys        map[string]crypto.PublicKey
	loaded      bool
	fetchedAt   time.Time
	lastAttempt time.Time
	// lastOK is whether the most recent fetch succeeded.
	lastOK bool
}

func newJWKSCache(url string, client *http.Client, now func() time.Time, allowRSA bool) *jwksCache {
	if client == nil {
		client = &http.Client{Timeout: jwksFetchTimeout}
	}
	return &jwksCache{url: url, client: client, now: now, rsa: allowRSA}
}

// usable reports whether the cached set may still verify tokens.
func (c *jwksCache) usable() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loaded && c.now().Sub(c.fetchedAt) <= jwksMaxStale && len(c.keys) > 0
}

// fetch downloads and replaces the key set. A failed or malformed fetch
// leaves the previous set in place and returns an error.
func (c *jwksCache) fetch(ctx context.Context) error {
	c.mu.Lock()
	c.lastAttempt = c.now()
	c.lastOK = false
	c.mu.Unlock()

	// Detached from the caller: a client that disconnects must not cancel
	// the fetch and leave a failed attempt that throttles the next refetch.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), jwksFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return errors.New("fetch jwks: request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch jwks: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, jwksMaxBytes+1))
	if err != nil {
		return errors.New("fetch jwks: read failed")
	}
	if len(body) > jwksMaxBytes {
		return errors.New("fetch jwks: response too large")
	}
	keys, err := parseJWKS(body, c.rsa)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.keys = keys
	c.loaded = true
	c.lastOK = true
	c.fetchedAt = c.now()
	c.mu.Unlock()
	return nil
}

// parseJWKS keeps only public signing keys: Ed25519 and, with allowRSA,
// RSA of at least 2048 bits. A set that is not valid JSON, or that carries no
// usable key, is a failed read, not an empty set.
func parseJWKS(body []byte, allowRSA bool) (map[string]crypto.PublicKey, error) {
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, errors.New("fetch jwks: malformed")
	}
	keys := make(map[string]crypto.PublicKey, len(set.Keys))
	for i := range set.Keys {
		k := &set.Keys[i]
		if k.Kid == "" || k.D != "" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		switch {
		case k.Kty == "OKP" && k.Crv == "Ed25519":
			if k.Alg != "" && k.Alg != AlgEdDSA {
				continue
			}
			x, err := base64.RawURLEncoding.DecodeString(k.X)
			if err != nil || len(x) != ed25519.PublicKeySize {
				continue
			}
			keys[k.Kid] = ed25519.PublicKey(x)
		case allowRSA && k.Kty == "RSA":
			if k.Alg != "" && k.Alg != AlgRS256 {
				continue
			}
			n, err1 := base64.RawURLEncoding.DecodeString(k.N)
			e, err2 := base64.RawURLEncoding.DecodeString(k.E)
			if err1 != nil || err2 != nil || len(n) < 256 || len(e) == 0 || len(e) > 4 {
				continue
			}
			exp := new(big.Int).SetBytes(e)
			if !exp.IsInt64() || exp.Int64() < 3 {
				continue
			}
			keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exp.Int64())}
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("fetch jwks: no usable signing key")
	}
	return keys, nil
}

// refreshThrottled fetches unless another fetch ran within jwksMinRefetch.
// It reports whether a fetch was attempted and how it went.
func (c *jwksCache) refreshThrottled(ctx context.Context) (attempted bool, err error) {
	c.flight.Lock()
	defer c.flight.Unlock()
	c.mu.Lock()
	recent := !c.lastAttempt.IsZero() && c.now().Sub(c.lastAttempt) < jwksMinRefetch
	c.mu.Unlock()
	if recent {
		return false, nil
	}
	return true, c.fetch(ctx)
}

func (c *jwksCache) lookup(kid string) (crypto.PublicKey, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k, ok := c.keys[kid]
	return k, ok
}

// key returns the public key for kid.
//
//   - A stale set (older than jwksTTL) is refreshed; a failed refresh keeps
//     the old set while it is younger than jwksMaxStale.
//   - An unknown kid triggers one refetch. If that refetch could not run or
//     failed, the answer is ErrJWKSUnavailable; only a successful fetch that
//     still lacks the kid makes the kid unknown (ok == false, err == nil).
func (c *jwksCache) key(ctx context.Context, kid string) (crypto.PublicKey, bool, error) {
	c.mu.Lock()
	stale := !c.loaded || c.now().Sub(c.fetchedAt) > jwksTTL
	c.mu.Unlock()
	if stale {
		_, _ = c.refreshThrottled(ctx)
	}
	if !c.usable() {
		return nil, false, ErrJWKSUnavailable
	}
	if k, ok := c.lookup(kid); ok {
		return k, true, nil
	}
	attempted, err := c.refreshThrottled(ctx)
	if err != nil {
		return nil, false, ErrJWKSUnavailable
	}
	if !attempted {
		// A fetch ran moments ago. If it succeeded, the set is current and
		// the kid is genuinely unknown; if it failed, we cannot say.
		c.mu.Lock()
		current := c.lastOK
		c.mu.Unlock()
		if !current {
			return nil, false, ErrJWKSUnavailable
		}
	}
	if k, ok := c.lookup(kid); ok {
		return k, true, nil
	}
	return nil, false, nil
}
