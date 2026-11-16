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
	"syscall"
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
	// BaseURL is mctl-api's origin, e.g. https://api.mctl.ai. http is
	// accepted only for a cluster-internal host (name.svc,
	// name.namespace.svc, optionally .cluster.local), never for an IP
	// literal or any other name.
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

// dnsLabel is one RFC 1123 label.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// clusterInternalHost reports whether host (already lower-cased, no port)
// is an in-cluster service name: name.svc, name.namespace.svc, or either
// followed by .cluster.local. Anything else, IP literals included, is not.
func clusterInternalHost(host string) bool {
	var prefix string
	switch {
	case strings.HasSuffix(host, ".svc.cluster.local"):
		prefix = strings.TrimSuffix(host, ".svc.cluster.local")
	case strings.HasSuffix(host, ".svc"):
		prefix = strings.TrimSuffix(host, ".svc")
	default:
		return false
	}
	labels := strings.Split(prefix, ".")
	if len(labels) > 2 {
		return false
	}
	for _, l := range labels {
		if !dnsLabel.MatchString(l) {
			return false
		}
	}
	return true
}

// validateConsentBaseURL accepts an https origin, or an http origin only
// for a cluster-internal service host. The error never echoes the URL.
func validateConsentBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || u.Opaque != "" {
		return nil, errors.New("linkflow: consent API base URL must be a bare https origin (http only for a cluster-internal .svc host)")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !clusterInternalHost(strings.ToLower(u.Hostname())) {
			return nil, errors.New("linkflow: consent API base URL must be https unless the host is a cluster-internal .svc or .svc.cluster.local name")
		}
	default:
		return nil, errors.New("linkflow: consent API base URL must be a bare https origin (http only for a cluster-internal .svc host)")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	return u, nil
}

// NewAPIConsent validates cfg.
func NewAPIConsent(cfg APIConsentConfig) (*APIConsent, error) {
	u, err := validateConsentBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
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
	return &APIConsent{base: u.Scheme + "://" + strings.ToLower(u.Host), tokenFile: cfg.TokenFile, client: &c}, nil
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+ConsentReadPath+url.PathEscape(id), nil) //nolint:gosec // origin is operator config validated (https, or http for a cluster-internal host); pid is a validated prn_ id
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
	// Stat before Open: opening a FIFO for reading blocks until a writer
	// appears, so a non-regular path must be refused before it is opened.
	// O_NONBLOCK covers a swap between the Stat and the Open; the
	// descriptor is checked again after opening.
	if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() { //nolint:gosec // operator-configured absolute token path
		return "", unavailable("token file unreadable")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // operator-configured absolute token path
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
