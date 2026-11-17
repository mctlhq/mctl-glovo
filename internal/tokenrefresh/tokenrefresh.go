// Package tokenrefresh keeps a ZITADEL machine-user access token in a file.
//
// It signs a JWT-profile assertion (RFC 7523, RS256) with the machine user's
// private key, exchanges it at {issuer}/oauth/v2/token and writes the access
// token to a file atomically, so a consumer that re-reads the file on every
// call (internal/linkflow's consent client) never sees a partial token.
//
// Failure policy (fail closed):
//   - a failed refresh keeps the previous token only until its own expiry;
//   - at expiry the token file is REMOVED (not truncated: a missing file is
//     "unavailable" to every consumer, an empty one could be misread) and Run
//     returns ErrExpired, so the container exits non-zero and restarts rather
//     than serving a dead credential;
//   - on start any token file left by an earlier run is removed, because its
//     expiry is unknown.
//
// Nothing secret is logged or returned in an error: not the key, the
// assertion, the token, or the response body.
package tokenrefresh

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	grantType = "urn:ietf:params:oauth:grant-type:jwt-bearer"

	maxResponse   = 64 << 10 // token endpoint response bound
	maxKeyFile    = 64 << 10
	maxTokenBytes = 8 << 10 // the consumer refuses larger files

	assertionTTL   = 5 * time.Minute
	attemptTimeout = 15 * time.Second

	minRenew = 5 * time.Second
	maxRenew = 6 * time.Hour

	backoffStart = time.Second
	backoffMax   = 5 * time.Minute
)

// ErrExpired is returned by Run when the last token expired and no refresh
// succeeded. The token file has been removed by then.
var ErrExpired = errors.New("tokenrefresh: token expired and could not be refreshed")

// tokenPattern is what the consumer accepts as a bearer token.
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]+$`)

// Config is the refresher's configuration.
type Config struct {
	Issuer    string // https://auth.mctl.ai
	UserID    string // machine user id (iss = sub of the assertion)
	KeyID     string // JWT header kid
	Scope     string
	KeyFile   string
	TokenFile string // absolute, clean
	ReadyFile string // optional: exists while a valid token is on disk
	FileMode  os.FileMode

	// AllowLoopbackHTTP permits an http:// issuer on a loopback address. It
	// is for tests and is never set from the environment.
	AllowLoopbackHTTP bool
}

// Clock abstracts time so renewal timing is testable.
type Clock interface {
	Now() time.Time
	// Sleep waits d or until ctx is done.
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Refresher runs the refresh loop.
type Refresher struct {
	cfg    Config
	key    *rsa.PrivateKey
	tokURL string
	client *http.Client
	clock  Clock
	log    *slog.Logger
	jitter func() float64 // in [0,1)
}

// Options are test seams; the zero value is production behaviour.
type Options struct {
	Clock  Clock
	Logger *slog.Logger
	Jitter func() float64
	Client *http.Client // transport override for tests; redirects stay refused
}

// ConfigError marks an unrecoverable configuration problem.
type ConfigError struct{ msg string }

func (e *ConfigError) Error() string { return e.msg }

func cfgErr(format string, a ...any) error { return &ConfigError{msg: fmt.Sprintf(format, a...)} }

// New validates the configuration and loads the key. Errors are ConfigErrors
// and never contain key material.
func New(cfg Config, opt Options) (*Refresher, error) {
	u, err := url.Parse(cfg.Issuer)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, cfgErr("tokenrefresh: issuer must be a bare http(s) origin")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !cfg.AllowLoopbackHTTP || !isLoopback(u.Hostname()) {
			return nil, cfgErr("tokenrefresh: issuer must be https")
		}
	default:
		return nil, cfgErr("tokenrefresh: issuer must be https")
	}
	issuer := u.Scheme + "://" + u.Host
	cfg.Issuer = issuer
	if cfg.Scope == "" {
		return nil, cfgErr("tokenrefresh: scope is required")
	}
	if strings.ContainsAny(cfg.Scope, "\r\n") {
		return nil, cfgErr("tokenrefresh: scope is malformed")
	}
	if cfg.TokenFile == "" || !filepath.IsAbs(cfg.TokenFile) || filepath.Clean(cfg.TokenFile) != cfg.TokenFile {
		return nil, cfgErr("tokenrefresh: token file must be a clean absolute path")
	}
	if cfg.ReadyFile != "" && (!filepath.IsAbs(cfg.ReadyFile) || filepath.Clean(cfg.ReadyFile) != cfg.ReadyFile || cfg.ReadyFile == cfg.TokenFile) {
		return nil, cfgErr("tokenrefresh: ready file must be a clean absolute path other than the token file")
	}
	if cfg.KeyFile == "" {
		return nil, cfgErr("tokenrefresh: key file is required")
	}
	if cfg.FileMode == 0 {
		cfg.FileMode = 0o644
	}
	if cfg.FileMode&^0o666 != 0 {
		return nil, cfgErr("tokenrefresh: file mode must be a permission set without special bits")
	}
	key, userID, keyID, err := loadKey(cfg.KeyFile, cfg.UserID, cfg.KeyID)
	if err != nil {
		return nil, err
	}
	cfg.UserID, cfg.KeyID = userID, keyID

	r := &Refresher{cfg: cfg, key: key, tokURL: issuer + "/oauth/v2/token", clock: opt.Clock, log: opt.Logger, jitter: opt.Jitter}
	if r.clock == nil {
		r.clock = realClock{}
	}
	if r.log == nil {
		r.log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if r.jitter == nil {
		r.jitter = mrand.Float64
	}
	var base http.RoundTripper
	if opt.Client != nil {
		base = opt.Client.Transport
	}
	if base == nil {
		// No proxy from the environment: the assertion goes straight to the issuer.
		base = &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: attemptTimeout,
			MaxIdleConns:          1,
			IdleConnTimeout:       30 * time.Second,
		}
	}
	r.client = &http.Client{
		Transport:     base,
		Timeout:       attemptTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return r, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type zitadelKey struct {
	UserID string `json:"userId"`
	KeyID  string `json:"keyId"`
	Key    string `json:"key"`
}

// loadKey reads the key file: either the PEM itself (the Vault field "key")
// or the JSON ZITADEL emits ({"userId","keyId","key"}). Ids from the
// environment must agree with ids in the JSON. Errors carry no key content.
func loadKey(path, envUser, envKey string) (*rsa.PrivateKey, string, string, error) {
	f, err := os.Open(path) //nolint:gosec // operator-configured path
	if err != nil {
		return nil, "", "", cfgErr("tokenrefresh: key file unreadable")
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxKeyFile+1))
	if err != nil || len(raw) > maxKeyFile {
		return nil, "", "", cfgErr("tokenrefresh: key file unreadable or too large")
	}
	user, keyID, pemBytes := envUser, envKey, raw
	if t := strings.TrimSpace(string(raw)); strings.HasPrefix(t, "{") {
		var zk zitadelKey
		if json.Unmarshal([]byte(t), &zk) != nil || zk.Key == "" {
			return nil, "", "", cfgErr("tokenrefresh: key file is not a valid key document")
		}
		if (envUser != "" && zk.UserID != "" && envUser != zk.UserID) || (envKey != "" && zk.KeyID != "" && envKey != zk.KeyID) {
			return nil, "", "", cfgErr("tokenrefresh: user or key id disagree with the key file")
		}
		if user == "" {
			user = zk.UserID
		}
		if keyID == "" {
			keyID = zk.KeyID
		}
		pemBytes = []byte(zk.Key)
	}
	if user == "" || keyID == "" {
		return nil, "", "", cfgErr("tokenrefresh: user id and key id are required")
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, "", "", cfgErr("tokenrefresh: key is not PEM")
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		var k any
		k, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		if err == nil {
			var ok bool
			if key, ok = k.(*rsa.PrivateKey); !ok {
				err = errors.New("not rsa")
			}
		}
	default:
		err = errors.New("unsupported")
	}
	if err != nil || key == nil {
		return nil, "", "", cfgErr("tokenrefresh: key is not a usable RSA private key")
	}
	if key.N.BitLen() < 2048 {
		return nil, "", "", cfgErr("tokenrefresh: RSA key is shorter than 2048 bits")
	}
	return key, user, keyID, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// assertion builds and signs the JWT-profile assertion.
func (r *Refresher) assertion(now time.Time) (string, error) {
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": r.cfg.KeyID})
	claims, _ := json.Marshal(map[string]any{
		"iss": r.cfg.UserID,
		"sub": r.cfg.UserID,
		"aud": r.cfg.Issuer,
		"iat": now.Unix(),
		"exp": now.Add(assertionTTL).Unix(),
	})
	signing := b64(hdr) + "." + b64(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, r.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", errors.New("tokenrefresh: signing failed")
	}
	return signing + "." + b64(sig), nil
}

type tokenResponse struct {
	AccessToken string          `json:"access_token"`
	TokenType   string          `json:"token_type"`
	ExpiresIn   json.RawMessage `json:"expires_in"`
}

// fetch performs one exchange. The returned error never contains the
// response body.
func (r *Refresher) fetch(ctx context.Context) (token string, ttl time.Duration, err error) {
	asr, err := r.assertion(r.clock.Now())
	if err != nil {
		return "", 0, err
	}
	form := url.Values{"grant_type": {grantType}, "assertion": {asr}, "scope": {r.cfg.Scope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.tokURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, errors.New("tokenrefresh: bad request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := r.client.Do(req) //nolint:gosec // issuer is validated https (or test loopback)
	if err != nil {
		// url.Error carries the URL (public) but never the request body.
		return "", 0, fmt.Errorf("tokenrefresh: token request failed: %w", unwrapURL(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("tokenrefresh: token endpoint answered %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return "", 0, errors.New("tokenrefresh: token response unreadable")
	}
	if len(body) > maxResponse {
		return "", 0, errors.New("tokenrefresh: token response too large")
	}
	var tr tokenResponse
	if json.Unmarshal(body, &tr) != nil {
		return "", 0, errors.New("tokenrefresh: token response is not valid JSON")
	}
	if tr.TokenType != "" && !strings.EqualFold(tr.TokenType, "bearer") {
		return "", 0, errors.New("tokenrefresh: unexpected token type")
	}
	if tr.AccessToken == "" || len(tr.AccessToken) > maxTokenBytes || !tokenPattern.MatchString(tr.AccessToken) {
		return "", 0, errors.New("tokenrefresh: token response carries no usable access token")
	}
	var secs float64
	if json.Unmarshal(tr.ExpiresIn, &secs) != nil || math.IsNaN(secs) || secs < 1 || secs > 7*24*3600 {
		return "", 0, errors.New("tokenrefresh: token response carries no usable expires_in")
	}
	return tr.AccessToken, time.Duration(secs * float64(time.Second)), nil
}

func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// writeAtomic writes data to path through a temp file in the same directory.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".token-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	if d, err := os.Open(dir); err == nil { //nolint:gosec // same dir as the token file
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (r *Refresher) setReady(ready bool) {
	if r.cfg.ReadyFile == "" {
		return
	}
	if ready {
		_ = writeAtomic(r.cfg.ReadyFile, []byte("ok\n"), 0o644)
	} else {
		_ = removeIfPresent(r.cfg.ReadyFile)
	}
}

// renewDelay is half of the lifetime, jittered downwards by up to 10% (never
// later than half), clamped to [minRenew, maxRenew] and never beyond the
// lifetime itself.
func (r *Refresher) renewDelay(ttl time.Duration) time.Duration {
	d := time.Duration(float64(ttl/2) * (1 - 0.1*r.jitter()))
	if d > maxRenew {
		d = maxRenew
	}
	if d < minRenew {
		d = minRenew
	}
	if d > ttl {
		d = ttl
	}
	return d
}

func (r *Refresher) retryDelay(failures int) time.Duration {
	d := backoffStart
	for i := 1; i < failures && d < backoffMax; i++ {
		d *= 2
	}
	if d > backoffMax {
		d = backoffMax
	}
	// jitter in [0.5, 1.0) of the step
	return time.Duration(float64(d) * (0.5 + 0.5*r.jitter()))
}

// Run refreshes until ctx is done (returns ctx.Err()) or the held token
// expires without a successful refresh (returns ErrExpired, file removed).
// A failure to remove the token file is returned as is: not being able to
// clear a possibly stale credential is fatal.
func (r *Refresher) Run(ctx context.Context) error {
	// An unknown leftover is not a valid token.
	if err := removeIfPresent(r.cfg.TokenFile); err != nil {
		return fmt.Errorf("tokenrefresh: cannot clear previous token file: %w", err)
	}
	r.setReady(false)

	var (
		expiry   time.Time // zero: no valid token on disk
		nextTry  time.Time // when to attempt the next refresh
		failures int
	)
	for {
		now := r.clock.Now()
		if !expiry.IsZero() && !now.Before(expiry) {
			return r.expire()
		}
		if now.Before(nextTry) {
			wake := nextTry
			if !expiry.IsZero() && expiry.Before(wake) {
				wake = expiry // clear on time, not at the next retry
			}
			if err := r.clock.Sleep(ctx, wake.Sub(now)); err != nil {
				return err
			}
			continue
		}

		actx, cancel := ctx, context.CancelFunc(func() {})
		if !expiry.IsZero() {
			// The attempt must not outlive the token it is replacing.
			actx, cancel = context.WithDeadline(ctx, expiry)
		}
		token, ttl, err := r.fetch(actx)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			if werr := writeAtomic(r.cfg.TokenFile, []byte(token), r.cfg.FileMode); werr != nil {
				err = errors.New("tokenrefresh: cannot write token file")
			}
		}
		if err != nil {
			failures++
			nextTry = r.clock.Now().Add(r.retryDelay(failures))
			r.log.Warn("token refresh failed", "error", err.Error(), "consecutive_failures", failures,
				"token_valid", !expiry.IsZero())
			continue
		}
		failures = 0
		now = r.clock.Now()
		expiry = now.Add(ttl)
		delay := r.renewDelay(ttl)
		nextTry = now.Add(delay)
		r.setReady(true)
		r.log.Info("token refreshed", "expires_in_seconds", int64(ttl/time.Second), "next_refresh_in_seconds", int64(delay/time.Second))
	}
}

func (r *Refresher) expire() error {
	r.setReady(false)
	if err := removeIfPresent(r.cfg.TokenFile); err != nil {
		return fmt.Errorf("tokenrefresh: token expired and the file could not be removed: %w", err)
	}
	r.log.Error("token expired and could not be refreshed; token file removed")
	return ErrExpired
}
