// Package linkflow serves the account-link flow of slice C2-1b
// (docs/design/token-store.md 4.1): the /link page, the one-time code for
// an authenticated principal, POST /link/preview and POST /link/upload.
//
// The whole flow sits behind GLOVO_C2_ENABLED (design 6). Off, Register
// mounts nothing and every route is a 404; the store is never built.
//
// Nothing this package writes to a log, an error or a response carries a
// link code, a session, or text from a dependency. Refusals are fixed
// texts from one table.
package linkflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/mctlhq/mctl-glovo/internal/authn"
	"github.com/mctlhq/mctl-glovo/internal/glovoapi"
	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

// Wire formats.
const (
	UploadSchema  = "glovo-link-upload/v1"
	PreviewSchema = "glovo-link-preview/v1"
	UnlinkSchema  = "glovo-link-unlink/v1"
	// MaxBody bounds every request body (design 4.1 step 4).
	MaxBody = 16 << 10
)

// linkTimeout bounds the work after a code is redeemed. It runs detached
// from the request: a client that hangs up must not leave the redemption
// done and the outcome unknown to the store.
const linkTimeout = 30 * time.Second

// Verifier is the token check, authn.Verifier in production: exact aud,
// exact iss, EdDSA against mctl-api's JWKS. The link routes use two: Verifier
// (the MCP audience) mints a code, LinkVerifier (the dedicated link audience,
// with a fresh-login requirement) previews and uploads.
type Verifier interface {
	Verify(ctx context.Context, token string) (*authn.Principal, error)
}

// Config configures the handlers.
type Config struct {
	// Enabled is GLOVO_C2_ENABLED. False registers no route.
	Enabled bool
	// Store, Verifier and Consent are required when Enabled.
	Store    tokenstore.Store
	Verifier Verifier
	Consent  ConsentSource
	// LinkVerifier checks the token on /link/preview and /link/upload: the
	// link audience and a fresh MCTL login (auth_time). Nil leaves those two
	// routes answering 503 and nothing else changes: no code can be
	// previewed or redeemed without it, so a missing link audience fails
	// closed and never falls back to the MCP verifier.
	LinkVerifier Verifier
	// TrustedProxies are the hops (ingress, tunnel) whose X-Forwarded-For
	// is believed. Empty: the client IP is the peer address.
	TrustedProxies []netip.Prefix
	Log            *slog.Logger
}

// Handler serves the link routes.
type Handler struct {
	cfg Config
	log *slog.Logger
}

// New validates cfg.
func New(cfg Config) (*Handler, error) {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Enabled && (cfg.Store == nil || cfg.Verifier == nil || cfg.Consent == nil) {
		return nil, errors.New("linkflow: store, verifier and consent source are required when enabled")
	}
	return &Handler{cfg: cfg, log: log}, nil
}

// Register mounts the routes on mux, or nothing when the flow is off.
func (h *Handler) Register(mux *http.ServeMux) {
	if !h.cfg.Enabled {
		return
	}
	mux.Handle("GET /link", headers(http.HandlerFunc(h.handlePage)))
	mux.Handle("POST /link/code", headers(http.HandlerFunc(h.handleCode)))
	mux.Handle("POST /link/preview", headers(http.HandlerFunc(h.handlePreview)))
	mux.Handle("POST /link/upload", headers(http.HandlerFunc(h.handleUpload)))
	mux.Handle("POST /link/unlink", headers(http.HandlerFunc(h.handleUnlink)))
}

func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Cache-Control", "no-store")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		next.ServeHTTP(w, r)
	})
}

// --- refusals ------------------------------------------------------------

// refusal is one fixed answer. Code is stable for the helper; Message is
// the only text a client ever sees.
type refusal struct {
	Status  int
	Code    string
	Message string
}

// Refusals, every one a fixed text (design 4.1 step 4).
var (
	refMalformed       = refusal{http.StatusBadRequest, "malformed_request", "the request is not a valid link request"}
	refTooLarge        = refusal{http.StatusRequestEntityTooLarge, "request_too_large", "the request is larger than 16 KiB"}
	refRateLimited     = refusal{http.StatusTooManyRequests, "rate_limited", "too many link attempts; try again later"}
	refKilled          = refusal{http.StatusServiceUnavailable, "linking_disabled", "linking is switched off"}
	refCode            = refusal{http.StatusBadRequest, "code_refused", "the link code is invalid, used or expired; request a new code"}
	refSession         = refusal{http.StatusUnprocessableEntity, "session_refused", "the Glovo session was not accepted; capture a new session and request a new code"}
	refUnavailable     = refusal{http.StatusServiceUnavailable, "unavailable", "linking is unavailable right now; request a new code and try again later"}
	refNoConsent       = refusal{http.StatusForbidden, "consent_required", "record your consent to the current link terms first"}
	refUnauthorized    = refusal{http.StatusUnauthorized, "invalid_token", "the access token is not valid for this resource"}
	refForbidden       = refusal{http.StatusForbidden, "access_denied", "this account is not allowed to link"}
	refAuthUnavailable = refusal{http.StatusServiceUnavailable, "temporarily_unavailable", "cannot verify tokens right now; try again later"}
	refFreshLogin      = refusal{http.StatusUnauthorized, "fresh_login_required", "sign in to MCTL again to continue linking"}
	refLinkLoginOff    = refusal{http.StatusServiceUnavailable, "unavailable", "linking is unavailable right now; request a new code and try again later"}
)

func (h *Handler) refuse(w http.ResponseWriter, route string, ref refusal) {
	h.log.Info("link", "route", route, "outcome", ref.Code, "status", ref.Status)
	if ref.Status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	}
	if ref.Status == http.StatusServiceUnavailable || ref.Status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "60")
	}
	writeJSON(w, ref.Status, map[string]string{"error": ref.Code, "message": ref.Message})
}

// storeRefusal maps a store error to its refusal. Unknown errors are
// "unavailable", never a pass.
func storeRefusal(err error) refusal {
	switch {
	case errors.Is(err, tokenstore.ErrKilled):
		return refKilled
	case errors.Is(err, tokenstore.ErrRateLimited):
		return refRateLimited
	case errors.Is(err, tokenstore.ErrCodeRefused):
		return refCode
	case errors.Is(err, tokenstore.ErrSessionRefused):
		return refSession
	case errors.Is(err, tokenstore.ErrConsentRefused):
		return refNoConsent
	default:
		return refUnavailable
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// --- request bodies ------------------------------------------------------

// readBody enforces the size, the media type, one JSON object with exactly
// the keys in want, and no trailing data.
func readBody(w http.ResponseWriter, r *http.Request, want ...string) (map[string]json.RawMessage, *refusal) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return nil, &refMalformed
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, &refTooLarge
		}
		return nil, &refMalformed
	}
	obj, ok := decodeObject(raw)
	if !ok || len(obj) != len(want) {
		return nil, &refMalformed
	}
	for _, k := range want {
		if _, ok := obj[k]; !ok {
			return nil, &refMalformed
		}
	}
	return obj, nil
}

// decodeObject reads one JSON object whose top-level keys are all
// distinct, followed by nothing. A repeated key is refused rather than
// resolved: encoding/json would keep the last value silently.
func decodeObject(raw []byte) (map[string]json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	obj := map[string]json.RawMessage{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, false
		}
		k, ok := t.(string)
		if !ok {
			return nil, false
		}
		if _, dup := obj[k]; dup {
			return nil, false
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		obj[k] = v
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return obj, true
}

func stringField(obj map[string]json.RawMessage, k string) (string, bool) {
	var s string
	if err := json.Unmarshal(obj[k], &s); err != nil || s == "" {
		return "", false
	}
	return s, true
}

func checkSchema(obj map[string]json.RawMessage, want string) bool {
	s, ok := stringField(obj, "schema")
	return ok && s == want
}

// --- client IP -----------------------------------------------------------

// clientIP is the peer address, or, when the peer is a trusted proxy, the
// right-most X-Forwarded-For entry that is not one. It is "" when an entry
// does not parse, and when a trusted peer names no untrusted hop: falling
// back to a proxy's own address would put every client behind it in one
// rate-limit bucket. The store's rate limit refuses "".
func (h *Handler) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return ""
	}
	ip = ip.Unmap()
	if !h.trusted(ip) {
		return ip.String()
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return ""
		}
		hop = hop.Unmap()
		if !h.trusted(hop) {
			return hop.String()
		}
	}
	return ""
}

func (h *Handler) trusted(ip netip.Addr) bool {
	for _, p := range h.cfg.TrustedProxies {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// --- authentication ------------------------------------------------------

// principal verifies the bearer token exactly as the MCP gateway does.
func (h *Handler) principal(r *http.Request) (tokenstore.PID, *refusal) {
	return h.verified(r, h.cfg.Verifier)
}

// linkPrincipal verifies the token of a preview or an upload: the link
// audience and a fresh login. Without a link verifier it refuses; it never
// falls back to the MCP verifier.
func (h *Handler) linkPrincipal(r *http.Request) (tokenstore.PID, *refusal) {
	if h.cfg.LinkVerifier == nil {
		return tokenstore.PID{}, &refLinkLoginOff
	}
	return h.verified(r, h.cfg.LinkVerifier)
}

func (h *Handler) verified(r *http.Request, v Verifier) (tokenstore.PID, *refusal) {
	if r.URL.Query().Has("access_token") {
		return tokenstore.PID{}, &refUnauthorized
	}
	a := r.Header.Get("Authorization")
	if len(a) < 8 || !strings.EqualFold(a[:7], "bearer ") || strings.TrimSpace(a[7:]) == "" {
		return tokenstore.PID{}, &refUnauthorized
	}
	p, err := v.Verify(r.Context(), strings.TrimSpace(a[7:]))
	if err != nil {
		var ae *authn.Error
		if !errors.As(err, &ae) {
			return tokenstore.PID{}, &refAuthUnavailable
		}
		switch ae.Status {
		case http.StatusServiceUnavailable:
			return tokenstore.PID{}, &refAuthUnavailable
		case http.StatusUnauthorized:
			if ae.Reason == authn.ReasonFreshLogin {
				return tokenstore.PID{}, &refFreshLogin
			}
			return tokenstore.PID{}, &refUnauthorized
		case http.StatusForbidden:
			if ae.Reason == authn.ReasonNotAdmitted {
				return tokenstore.PID{}, &refNoConsent
			}
			return tokenstore.PID{}, &refForbidden
		default:
			return tokenstore.PID{}, &refUnauthorized
		}
	}
	pid := tokenstore.PIDFrom(p)
	if pid == (tokenstore.PID{}) {
		return tokenstore.PID{}, &refForbidden
	}
	return pid, nil
}

// --- routes --------------------------------------------------------------

func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

func renderPage(w http.ResponseWriter, d pageData) {
	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, d); err != nil {
		http.Error(w, "page unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// handlePage shows the consent text and how to get a code. It holds no
// secret and needs no token.
func (h *Handler) handlePage(w http.ResponseWriter, _ *http.Request) {
	renderPage(w, newPage("", time.Time{}))
}

// handleCode issues a code for the authenticated principal (step 2). The
// consent version comes from the server-side consent record only, and must
// be the version this service shows.
func (h *Handler) handleCode(w http.ResponseWriter, r *http.Request) {
	const route = "code"
	pid, ref := h.principal(r)
	if ref != nil {
		h.refuse(w, route, *ref)
		return
	}
	if err := h.cfg.Store.LinkAttempt(h.clientIP(r)); err != nil {
		h.refuse(w, route, refRateLimited)
		return
	}
	version, err := h.cfg.Consent.RecordedConsent(r.Context(), pid)
	switch {
	case errors.Is(err, ErrNoConsent):
		h.refuse(w, route, refNoConsent)
		return
	case err != nil:
		h.refuse(w, route, refUnavailable)
		return
	case version != ConsentVersion:
		// A consent to another text is not a consent to this one.
		h.refuse(w, route, refNoConsent)
		return
	}
	code, expires, err := h.cfg.Store.CreateLinkCode(r.Context(), pid, version)
	if err != nil {
		h.refuse(w, route, storeRefusal(err))
		return
	}
	h.log.Info("link", "route", route, "outcome", "issued")
	if wantsHTML(r) {
		renderPage(w, newPage(code, expires))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"code":            code,
		"expires_at":      expires.UTC().Format(time.RFC3339),
		"consent_version": version,
	})
}

// handlePreview answers the masked identity a code belongs to (step 3),
// without burning it.
func (h *Handler) handlePreview(w http.ResponseWriter, r *http.Request) {
	const route = "preview"
	pid, ref := h.linkPrincipal(r)
	if ref != nil {
		h.refuse(w, route, *ref)
		return
	}
	obj, ref := readBody(w, r, "schema", "code")
	if ref != nil {
		h.refuse(w, route, *ref)
		return
	}
	code, ok := stringField(obj, "code")
	if !ok || !checkSchema(obj, PreviewSchema) {
		h.refuse(w, route, refMalformed)
		return
	}
	if err := h.cfg.Store.LinkAttempt(h.clientIP(r)); err != nil {
		h.refuse(w, route, refRateLimited)
		return
	}
	// The identity of a principal is shown only while its consent holds now.
	version, err := h.cfg.Consent.RecordedConsent(r.Context(), pid)
	switch {
	case errors.Is(err, ErrNoConsent), err == nil && version != ConsentVersion:
		h.refuse(w, route, refNoConsent)
		return
	case err != nil:
		h.refuse(w, route, refUnavailable)
		return
	}
	masked, err := h.cfg.Store.PreviewLink(r.Context(), pid, code)
	if err != nil {
		h.refuse(w, route, storeRefusal(err))
		return
	}
	h.log.Info("link", "route", route, "outcome", "previewed")
	writeJSON(w, http.StatusOK, map[string]string{"identity": masked})
}

// handleUpload is step 4, in the design's order: the body, the rate limits,
// then the store, which checks the kill switch, redeems the code, checks
// the session's shape, makes the validating Glovo call, and stores.
func (h *Handler) handleUpload(w http.ResponseWriter, r *http.Request) {
	const route = "upload"
	pid, ref := h.linkPrincipal(r)
	if ref != nil {
		h.refuse(w, route, *ref)
		return
	}
	obj, ref := readBody(w, r, "schema", "code", "session")
	if ref != nil {
		h.refuse(w, route, *ref)
		return
	}
	code, ok := stringField(obj, "code")
	if !ok || !checkSchema(obj, UploadSchema) {
		h.refuse(w, route, refMalformed)
		return
	}
	sdec := json.NewDecoder(bytes.NewReader(obj["session"]))
	sdec.UseNumber()
	var sess map[string]any
	if err := sdec.Decode(&sess); err != nil || sess == nil {
		h.refuse(w, route, refMalformed)
		return
	}
	if err := h.cfg.Store.LinkAttempt(h.clientIP(r)); err != nil {
		h.refuse(w, route, refRateLimited)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), linkTimeout)
	defer cancel()
	if err := h.cfg.Store.Link(ctx, pid, code, glovoapi.Session(sess)); err != nil {
		h.refuse(w, route, storeRefusal(err))
		return
	}
	h.log.Info("link", "route", route, "outcome", "linked")
	writeJSON(w, http.StatusOK, map[string]string{"result": "linked"})
}

// handleUnlink ends the caller's own link and deletes the stored Glovo
// session at once. It takes the same fresh-login token as an upload, so a
// stolen MCP token cannot unlink, and it needs no consent, no code and no
// working Vault: the kill switch and a Vault outage do not stand between a
// user and their exit. The pid is the verified one, never a body field.
func (h *Handler) handleUnlink(w http.ResponseWriter, r *http.Request) {
	const route = "unlink"
	pid, ref := h.linkPrincipal(r)
	if ref != nil {
		h.refuse(w, route, *ref)
		return
	}
	obj, ref := readBody(w, r, "schema")
	if ref != nil {
		h.refuse(w, route, *ref)
		return
	}
	if !checkSchema(obj, UnlinkSchema) {
		h.refuse(w, route, refMalformed)
		return
	}
	// Detached like an upload: a client that hangs up must not leave the
	// outcome unknown.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), linkTimeout)
	defer cancel()
	if err := h.cfg.Store.Unlink(ctx, pid, "user_request"); err != nil {
		h.refuse(w, route, refUnavailable)
		return
	}
	h.log.Info("link", "route", route, "outcome", "unlinked")
	writeJSON(w, http.StatusOK, map[string]string{"result": "unlinked"})
}
