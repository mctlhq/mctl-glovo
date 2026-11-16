package linkflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

// ConsentReadPath is mctl-api's consent read route (mctl-api, slice C2-1c),
// followed by the principal id.
const ConsentReadPath = "/api/v1/assistants/glovo-consent/v1/principals/"

// maxConsentBody bounds the read answer.
const maxConsentBody = 4 << 10

// APIConsentConfig configures APIConsent.
type APIConsentConfig struct {
	// BaseURL is mctl-api's https origin, e.g. https://api.mctl.ai.
	BaseURL string
	// TokenFile holds the service credential mctl-api accepts for the
	// consent reader (a projected ServiceAccount token). It is read on
	// every call, so a rotated token is picked up without a restart.
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
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("linkflow: consent API base URL must be an https origin")
	}
	if strings.TrimSpace(cfg.TokenFile) == "" {
		return nil, errors.New("linkflow: consent API token file is required")
	}
	var c http.Client
	if cfg.Client != nil {
		c = *cfg.Client
	} else {
		c = http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}
	}
	// A 3xx is an answer, not a pointer: it lands on the non-200 path and
	// fails closed, so a redirect can never supply the consent.
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &APIConsent{base: "https://" + u.Host, tokenFile: cfg.TokenFile, client: &c}, nil
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
	raw, err := os.ReadFile(c.tokenFile) //nolint:gosec // operator-configured token path
	if err != nil {
		return "", unavailable("token file unreadable")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", unavailable("token file empty")
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
