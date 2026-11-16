package linkflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

// ConsentReadPath is mctl-api's consent read route (mctl-api, slice C2-1c),
// followed by the principal id.
const ConsentReadPath = "/api/v1/assistants/glovo-consent/v1/principals/"

// maxConsentBody bounds the read answer.
const maxConsentBody = 4 << 10

// maxTokenFile bounds the credential file. A larger file is refused without
// a request: it is not a machine-user token.
const maxTokenFile = 8 << 10

// tokenPattern is what a bearer token may be (a JWT or an opaque base64url
// string). Anything else, whitespace and control bytes included, is refused
// so the file can never smuggle a header or a second line into the request.
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]+$`)

// APIConsentConfig configures APIConsent.
type APIConsentConfig struct {
	// BaseURL is mctl-api's http or https origin, e.g. https://api.mctl.ai
	// (http for an in-cluster service address).
	BaseURL string
	// TokenFile is an absolute path to the service credential mctl-api
	// accepts for the consent reader (a short-lived machine-user token
	// kept fresh by a sidecar). It is read on every call, never cached, so
	// a rotated token is picked up without a restart.
	TokenFile string
	// Client is the HTTP client; nil is a direct client with a 5 s
	// timeout. mctl-api traffic never goes through the Glovo egress proxy.
	// Redirects are never followed, whichever client is given.
	Client *http.Client
}

// APIConsent is the ConsentSource that reads mctl-api's consent record.
// Only mctl-api's documented absence (200, state "none") is ErrNoConsent;
// every other answer, and every failure to get one, is
// ErrConsentUnavailable. Nothing from the token or the response body ever
// reaches an error.
type APIConsent struct {
	base      string
	tokenFile string
	client    *http.Client
}

// NewAPIConsent validates cfg.
func NewAPIConsent(cfg APIConsentConfig) (*APIConsent, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("linkflow: consent API base URL must be an http or https origin")
	}
	if cfg.TokenFile == "" || !filepath.IsAbs(cfg.TokenFile) || filepath.Clean(cfg.TokenFile) != cfg.TokenFile {
		return nil, errors.New("linkflow: consent API token file must be a clean absolute path")
	}
	var c http.Client
	if cfg.Client != nil {
		c = *cfg.Client
	} else {
		c = http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
			TLSHandshakeTimeout:   2 * time.Second,
			ResponseHeaderTimeout: 4 * time.Second,
		}}
	}
	// A 3xx is an answer, not a pointer: it lands on the non-200 path and
	// fails closed, so a redirect can never supply the consent.
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &APIConsent{base: u.Scheme + "://" + u.Host, tokenFile: cfg.TokenFile, client: &c}, nil
}

type consentAnswer struct {
	PrincipalID    string `json:"principal_id"`
	State          string `json:"state"`
	ConsentVersion string `json:"consent_version"`
}

func unavailable(reason string) error {
	return fmt.Errorf("%w: %s", ErrConsentUnavailable, reason)
}

// RecordedConsent implements ConsentSource.
func (c *APIConsent) RecordedConsent(ctx context.Context, pid tokenstore.PID) (string, error) {
	id := pid.String()
	if id == "" {
		return "", unavailable("no principal")
	}
	token, err := readToken(c.tokenFile)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+ConsentReadPath+url.PathEscape(id), nil) //nolint:gosec // origin is operator config validated as https; pid is a validated prn_ id
	if err != nil {
		return "", unavailable("request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req) //nolint:gosec // see the request above
	if err != nil {
		return "", unavailable("request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", unavailable(fmt.Sprintf("status %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxConsentBody+1))
	if err != nil || len(body) > maxConsentBody {
		return "", unavailable("body unreadable")
	}
	var a consentAnswer
	if err := json.Unmarshal(body, &a); err != nil {
		return "", unavailable("body malformed")
	}
	switch {
	case a.PrincipalID != id:
		return "", unavailable("answer for another principal")
	case a.State == "none" && a.ConsentVersion == "":
		return "", ErrNoConsent
	case a.State == "active" && a.ConsentVersion != "":
		return a.ConsentVersion, nil
	}
	return "", unavailable("unknown state")
}

// readToken reads and validates the credential file. Every failure is
// ErrConsentUnavailable and happens before any request, so a bad file never
// reaches the network. The contents never reach an error.
func readToken(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // operator-configured absolute token path
	if err != nil {
		return "", unavailable("token file unreadable")
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return "", unavailable("token file unreadable")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxTokenFile+1))
	if err != nil {
		return "", unavailable("token file unreadable")
	}
	if len(raw) > maxTokenFile {
		return "", unavailable("token file too large")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", unavailable("token file empty")
	}
	if !tokenPattern.MatchString(token) {
		return "", unavailable("token file malformed")
	}
	return token, nil
}
