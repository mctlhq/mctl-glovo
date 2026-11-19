package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mctlhq/mctl-glovo/internal/authn"
	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

// controlPath is the cluster-internal kill switch route. mctl-api's
// glovoc2control client PUTs {"killed": bool} here with a machine-user
// bearer token and requires 200 {"killed": <bool>} = the state now stored
// in glovo_control. Every other answer is "state unknown" to it.
const controlPath = "/internal/v1/control/kill-switch"

// controlBodyLimit caps the request body. The only valid body is a few
// bytes; 1 KiB is generous.
const controlBodyLimit = 1 << 10

// controlConfig is the environment of the control endpoint. All of it is
// read only when GLOVO_C2_ENABLED=true. With no subjects configured the
// route is not mounted at all (the store ships killed and is opened by an
// operator), so a half-configured identity can never become "any token".
type controlConfig struct {
	issuer   string
	audience string
	jwksURL  string
	subjects []string
}

func (c controlConfig) enabled() bool { return len(c.subjects) > 0 }

// loadControl reads GLOVO_C2_CONTROL_SUBJECTS (the allow-list of machine
// user subjects), GLOVO_C2_CONTROL_ISSUER, GLOVO_C2_CONTROL_AUDIENCE and
// GLOVO_C2_CONTROL_JWKS_URL. None set means the endpoint is off. Subjects
// set without the other three, or the reverse, refuses the boot: a typo must
// not silently leave the kill switch unreachable, and the identity check is
// never loosened to fit what is configured.
func loadControl(getenv func(string) string) (controlConfig, error) {
	c := controlConfig{
		issuer:   strings.TrimSpace(getenv("GLOVO_C2_CONTROL_ISSUER")),
		audience: strings.TrimSpace(getenv("GLOVO_C2_CONTROL_AUDIENCE")),
		jwksURL:  strings.TrimSpace(getenv("GLOVO_C2_CONTROL_JWKS_URL")),
	}
	for _, s := range strings.Split(getenv("GLOVO_C2_CONTROL_SUBJECTS"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			c.subjects = append(c.subjects, s)
		}
	}
	if len(c.subjects) == 0 && c.issuer == "" && c.audience == "" && c.jwksURL == "" {
		return controlConfig{}, nil
	}
	if len(c.subjects) == 0 || c.issuer == "" || c.audience == "" || c.jwksURL == "" {
		return controlConfig{}, errors.New("GLOVO_C2_CONTROL_SUBJECTS, _ISSUER, _AUDIENCE and _JWKS_URL are set together or not at all")
	}
	for name, v := range map[string]string{"GLOVO_C2_CONTROL_ISSUER": c.issuer, "GLOVO_C2_CONTROL_JWKS_URL": c.jwksURL} {
		u, err := url.Parse(v)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return controlConfig{}, fmt.Errorf("%s is not an https URL", name)
		}
	}
	return c, nil
}

// serviceVerifier is what the control endpoint needs of authn.
type serviceVerifier interface {
	Verify(ctx context.Context, token string) (string, error)
}

// flipper is the durable switch (tokenstore.KillSwitch).
type flipper interface {
	Set(ctx context.Context, killed bool, by string) (bool, error)
}

// controlRoutes serves the kill switch flip.
type controlRoutes struct {
	verifier serviceVerifier
	kill     flipper
	log      *slog.Logger
	// audit receives one line per flip attempt (no subject, no token).
	audit *slog.Logger
	// accounts ends a user's link (WithAccounts); nil leaves those routes off.
	accounts accountEnder
}

func newControlRoutes(v serviceVerifier, k flipper, log, audit *slog.Logger) (*controlRoutes, error) {
	if v == nil || k == nil || log == nil || audit == nil {
		return nil, errors.New("control: verifier, kill switch and loggers are required")
	}
	return &controlRoutes{verifier: v, kill: k, log: log, audit: audit}, nil
}

// Register mounts the route. A nil receiver registers nothing.
func (c *controlRoutes) Register(mux *http.ServeMux) {
	if c != nil {
		mux.HandleFunc("PUT "+controlPath, c.handle)
		if c.accounts != nil {
			mux.HandleFunc("POST "+unlinkPath, c.handleAccount(false))
			mux.HandleFunc("POST "+erasePath, c.handleAccount(true))
		}
	}
}

func controlJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func controlError(w http.ResponseWriter, status int, code string) {
	controlJSON(w, status, map[string]string{"error": code})
}

// viaProxy reports a request that came through a reverse proxy. The route
// is for the cluster-internal service only: a public ingress adds
// forwarding headers, so such a request is treated as not found even with a
// valid token. (The ingress must not route the path either; this is the
// second lock.)
func viaProxy(r *http.Request) bool {
	for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip", "Forwarded"} {
		if r.Header.Get(h) != "" {
			return true
		}
	}
	return false
}

// authenticate is the check every control route shares: not via a proxy, a
// bearer token, a machine-user subject on the allow-list. It writes the
// refusal itself and audits it under event.
func (c *controlRoutes) authenticate(w http.ResponseWriter, r *http.Request, event string) bool {
	if viaProxy(r) {
		http.NotFound(w, r)
		return false
	}
	// Authenticate before reading the body.
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		controlError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	if _, err := c.verifier.Verify(r.Context(), tok); err != nil {
		// Anything that is not a classified refusal is a failed read.
		status := http.StatusServiceUnavailable
		var ae *authn.Error
		if errors.As(err, &ae) {
			status = ae.Status
		}
		if status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", "Bearer")
		}
		c.audit.Warn(event, "status", status)
		controlError(w, status, "refused")
		return false
	}
	return true
}

func (c *controlRoutes) handle(w http.ResponseWriter, r *http.Request) {
	if !c.authenticate(w, r, "kill_switch_refused") {
		return
	}
	killed, ok := readKilled(w, r)
	if !ok {
		return
	}
	// The write lands before the answer, and the answer is what was read
	// back from the row, never the request echoed.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	stored, err := c.kill.Set(ctx, killed, "control-api")
	if err != nil {
		c.audit.Error("kill_switch_flip", "requested", killed, "outcome", "unknown")
		c.log.Error("kill switch flip failed", "error_class", errClass(err))
		controlError(w, http.StatusServiceUnavailable, "state_unknown")
		return
	}
	c.audit.Warn("kill_switch_flip", "requested", killed, "stored", stored, "outcome", "ok")
	controlJSON(w, http.StatusOK, map[string]bool{"killed": stored})
}

// accountPaths are the operator routes that end a user's link. They share
// the kill switch's identity check and its proxy lock.
const (
	unlinkPath = "/internal/v1/control/unlink"
	erasePath  = "/internal/v1/control/erase"
)

// accountEnder is the part of tokenstore.Store the account routes use.
// Neither call checks the kill switch or needs Vault: ending a link must
// work exactly when something else is broken.
type accountEnder interface {
	OperatorUnlink(ctx context.Context, pid tokenstore.PID) error
	Erase(ctx context.Context, pid tokenstore.PID) error
}

// WithAccounts mounts POST unlinkPath and erasePath. Nil leaves them off.
func (c *controlRoutes) WithAccounts(a accountEnder) *controlRoutes {
	c.accounts = a
	return c
}

func (c *controlRoutes) handleAccount(erase bool) http.HandlerFunc {
	event := "account_unlink"
	if erase {
		event = "account_erase"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !c.authenticate(w, r, event+"_refused") {
			return
		}
		var req struct {
			Principal *string `json:"principal"`
		}
		if !readStrict(w, r, &req) {
			return
		}
		if req.Principal == nil {
			controlError(w, http.StatusBadRequest, "bad_body")
			return
		}
		// The id comes from an authenticated operator, not from a user
		// request; it is still held to the prn_ shape by PIDFrom.
		pid := tokenstore.PIDFrom(&authn.Principal{ID: *req.Principal})
		if pid == (tokenstore.PID{}) {
			controlError(w, http.StatusBadRequest, "bad_body")
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
		defer cancel()
		var err error
		if erase {
			err = c.accounts.Erase(ctx, pid)
		} else {
			err = c.accounts.OperatorUnlink(ctx, pid)
		}
		if err != nil {
			c.audit.Error(event, "outcome", "unknown")
			c.log.Error("account end failed", "error_class", errClass(err))
			controlError(w, http.StatusServiceUnavailable, "state_unknown")
			return
		}
		c.audit.Warn(event, "outcome", "ok")
		controlJSON(w, http.StatusOK, map[string]string{"result": map[bool]string{false: "unlinked", true: "erased"}[erase]})
	}
}

// readKilled reads the strict body: a JSON object with exactly one field,
// "killed", a bool. It writes the refusal itself.
func readKilled(w http.ResponseWriter, r *http.Request) (bool, bool) {
	var req struct {
		Killed *bool `json:"killed"`
	}
	if !readStrict(w, r, &req) {
		return false, false
	}
	if req.Killed == nil {
		controlError(w, http.StatusBadRequest, "bad_body")
		return false, false
	}
	return *req.Killed, true
}

// readStrict decodes the one JSON object a control route accepts into dst:
// application/json, at most controlBodyLimit bytes, no unknown fields,
// nothing after the object. It writes the refusal itself; a missing field
// is for the caller to check.
func readStrict(w http.ResponseWriter, r *http.Request, dst any) bool {
	if mt, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";"); strings.TrimSpace(strings.ToLower(mt)) != "application/json" {
		controlError(w, http.StatusUnsupportedMediaType, "content_type")
		return false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, controlBodyLimit))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			controlError(w, http.StatusRequestEntityTooLarge, "too_large")
		} else {
			controlError(w, http.StatusBadRequest, "unreadable_body")
		}
		return false
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		controlError(w, http.StatusBadRequest, "bad_body")
		return false
	}
	// Nothing but whitespace may follow the one object.
	if _, err := dec.Token(); err != io.EOF {
		controlError(w, http.StatusBadRequest, "bad_body")
		return false
	}
	return true
}

// errClass is a coarse, secret-free class of a store error for logs.
func errClass(err error) string {
	switch {
	case errors.Is(err, tokenstore.ErrUnavailable):
		return "store_unavailable"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "other"
	}
}
