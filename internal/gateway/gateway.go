// Package gateway is the HTTP surface of glovo-home: the RFC 9728
// protected-resource metadata, the bearer challenge and the authenticated
// MCP Streamable HTTP endpoint.
//
// The metadata, the challenge and their headers copy what mctl-api serves
// for glovo.mctl.ai today (internal/api/glovo_basket.go), so clients that
// already discovered the host keep working when it moves here.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/mctlhq/mctl-glovo/internal/authn"
)

// maxBodyBytes bounds a POST /mcp body.
const maxBodyBytes = 1 << 20

// Verifier is the token check, authn.Verifier in production.
type Verifier interface {
	Verify(ctx context.Context, token string) (*authn.Principal, error)
}

// Config configures the gateway.
type Config struct {
	// Resource is the exact resource, e.g. https://glovo.mctl.ai/mcp.
	Resource string
	// AuthorizationServer is the issuer, e.g. https://api.mctl.ai.
	AuthorizationServer string
	Verifier            Verifier
	// MCP is the streamable HTTP handler the authenticated /mcp reaches.
	MCP http.Handler
	Log *slog.Logger
}

// Gateway serves the routes.
type Gateway struct {
	cfg         Config
	log         *slog.Logger
	metadataURL string
	host        string
	sessions    *sessionOwners
}

// New validates cfg and returns a Gateway.
func New(cfg Config) (*Gateway, error) {
	u, err := url.Parse(cfg.Resource)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("gateway: resource must be an absolute https URL")
	}
	if cfg.AuthorizationServer == "" || cfg.Verifier == nil || cfg.MCP == nil {
		return nil, errors.New("gateway: authorization server, verifier and MCP handler are required")
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Gateway{
		cfg: cfg,
		log: log,
		// mctl-api's challenge points at the root metadata document, not
		// the /mcp-suffixed alias; both are served.
		metadataURL: u.Scheme + "://" + u.Host + "/.well-known/oauth-protected-resource",
		host:        strings.ToLower(u.Hostname()),
		sessions:    newSessionOwners(),
	}, nil
}

// Register mounts the routes on mux.
func (g *Gateway) Register(mux *http.ServeMux) {
	meta := g.securityHeaders(http.HandlerFunc(g.handleMeta))
	mux.Handle("GET /.well-known/oauth-protected-resource", meta)
	mux.Handle("GET /.well-known/oauth-protected-resource/mcp", meta)
	mcpH := g.securityHeaders(g.hostGuard(g.authenticate(g.bindSession(g.cfg.MCP))))
	mux.Handle("POST /mcp", mcpH)
	mux.Handle("GET /mcp", mcpH)
	mux.Handle("DELETE /mcp", mcpH)
}

// ProtectedResourceMeta is the RFC 9728 document, field for field what
// mctl-api serves.
type ProtectedResourceMeta struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

func (g *Gateway) handleMeta(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_ = json.NewEncoder(w).Encode(ProtectedResourceMeta{
		Resource:             g.cfg.Resource,
		AuthorizationServers: []string{g.cfg.AuthorizationServer},
		ScopesSupported:      []string{},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// unauthorized is mctl-api's glovoBasketUnauthorized, byte for byte.
func (g *Gateway) unauthorized(w http.ResponseWriter, errCode, desc string) {
	v := `Bearer resource_metadata="` + g.metadataURL + `"`
	if errCode != "" {
		v += `, error="` + errCode + `", error_description="` + desc + `"`
	}
	w.Header().Set("WWW-Authenticate", v)
	body := map[string]string{"error": "unauthenticated"}
	if errCode != "" {
		body = map[string]string{"error": errCode, "error_description": desc}
	}
	writeJSON(w, http.StatusUnauthorized, body)
}

// securityHeaders is mctl-api's securityHeaders middleware.
func (g *Gateway) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		h.Set("X-Permitted-Cross-Domain-Policies", "none")
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// hostGuard admits the public resource host and loopback names only. It
// replaces mcp-go's DNS-rebinding guard, which has to be off because the
// tunnel connects over loopback while keeping Host: glovo.mctl.ai.
func (g *Gateway) hostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Host
		if hh, _, err := net.SplitHostPort(h); err == nil {
			h = hh
		}
		h = strings.ToLower(strings.TrimSuffix(strings.Trim(h, "[]"), "."))
		switch h {
		case g.host, "localhost", "127.0.0.1", "::1":
		default:
			writeJSON(w, http.StatusMisdirectedRequest, map[string]string{"error": "misdirected_request"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authenticate requires a valid bearer token on every request.
func (g *Gateway) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("access_token") {
			g.unauthorized(w, "invalid_request", "send the access token in the Authorization header")
			return
		}
		a := r.Header.Get("Authorization")
		if len(a) < 8 || !strings.EqualFold(a[:7], "bearer ") || strings.TrimSpace(a[7:]) == "" {
			g.unauthorized(w, "", "")
			return
		}
		p, err := g.cfg.Verifier.Verify(r.Context(), strings.TrimSpace(a[7:]))
		if err != nil {
			var ae *authn.Error
			if !errors.As(err, &ae) {
				ae = &authn.Error{Reason: "internal", Status: http.StatusServiceUnavailable}
			}
			g.log.Warn("request refused", "reason", ae.Reason, "status", ae.Status, "method", r.Method)
			switch ae.Status {
			case http.StatusServiceUnavailable:
				w.Header().Set("Retry-After", "30")
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{
					"error": "temporarily_unavailable", "error_description": "cannot verify tokens right now; try again later",
				})
			case http.StatusForbidden:
				writeJSON(w, http.StatusForbidden, map[string]string{
					"error": "access_denied", "error_description": "this account is not allowed on this server",
				})
			default:
				g.unauthorized(w, "invalid_token", "the access token is not valid for this resource")
			}
			return
		}
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		next.ServeHTTP(w, r.WithContext(authn.WithPrincipal(r.Context(), p)))
	})
}

// sessionOwners binds each MCP session id to the principal that created
// it, so a session id is useless to any other principal.
type sessionOwners struct {
	mu     sync.Mutex
	owners map[string]sessionOwner
}

type sessionOwner struct {
	principal string
	lastSeen  time.Time
}

const sessionIdle = 24 * time.Hour

func newSessionOwners() *sessionOwners {
	return &sessionOwners{owners: map[string]sessionOwner{}}
}

// check reports whether principal may use id; an id this gateway never saw
// is left to the MCP server, which answers 404 for unknown sessions.
func (s *sessionOwners) check(id, principal string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.owners[id]
	if !ok {
		return true
	}
	if o.principal != principal {
		return false
	}
	o.lastSeen = now
	s.owners[id] = o
	return true
}

func (s *sessionOwners) record(id, principal string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, o := range s.owners {
		if now.Sub(o.lastSeen) > sessionIdle {
			delete(s.owners, k)
		}
	}
	if _, ok := s.owners[id]; !ok {
		s.owners[id] = sessionOwner{principal: principal, lastSeen: now}
	}
}

func (s *sessionOwners) forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.owners, id)
}

type captureWriter struct {
	http.ResponseWriter
	onHeader func(h http.Header)
	once     sync.Once
}

func (c *captureWriter) WriteHeader(code int) {
	c.once.Do(func() { c.onHeader(c.Header()) })
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	c.once.Do(func() { c.onHeader(c.Header()) })
	return c.ResponseWriter.Write(b)
}

func (c *captureWriter) Flush() {
	c.once.Do(func() { c.onHeader(c.Header()) })
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func (g *Gateway) bindSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := authn.FromContext(r.Context())
		now := time.Now()
		if id := r.Header.Get(server.HeaderKeySessionID); id != "" {
			if !g.sessions.check(id, p.ID, now) {
				g.log.Warn("request refused", "reason", "session_principal_mismatch", "method", r.Method)
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "session_not_found"})
				return
			}
			if r.Method == http.MethodDelete {
				defer g.sessions.forget(id)
			}
		}
		cw := &captureWriter{ResponseWriter: w, onHeader: func(h http.Header) {
			if id := h.Get(server.HeaderKeySessionID); id != "" {
				g.sessions.record(id, p.ID, now)
			}
		}}
		next.ServeHTTP(cw, r)
	})
}
