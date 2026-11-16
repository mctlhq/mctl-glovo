package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/mctlhq/mctl-glovo/internal/authn"
	"github.com/mctlhq/mctl-glovo/internal/config"
	"github.com/mctlhq/mctl-glovo/internal/consentgate"
	"github.com/mctlhq/mctl-glovo/internal/glovoapi"
	"github.com/mctlhq/mctl-glovo/internal/health"
	"github.com/mctlhq/mctl-glovo/internal/linkflow"
	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

// c2Config is the environment of the C2 link flow. Every field but the
// flag is read only when the flag is on.
type c2Config struct {
	enabled        bool
	allowed        []string
	databaseURL    string
	vaultAddr      string
	vaultRole      string
	proxy          *url.URL
	trustedProxies []netip.Prefix
	// consentAPI and consentTokenFile name mctl-api's consent read route
	// (C2-1c). Both unset keeps the fail-closed UnavailableConsent.
	consentAPI       string
	consentTokenFile string
	// auditKey keys the audit digest of the pid (GLOVO_C2_AUDIT_KEY, a
	// per-deployment secret from Vault, at least minAuditKeyLen bytes).
	auditKey []byte
	// capacity is the cohort size (GLOVO_C2_CAPACITY, 1..20); caps are the
	// token store limits derived from it and the GLOVO_C2_CAP_* overrides.
	// The kill switch and HTTP routes read them from here.
	capacity int
	caps     tokenstore.Caps
}

// loadC2 reads GLOVO_C2_ENABLED (design 6) and, when it is "true", the
// rest. Anything but "", "false" or "true" is an error, not "off": a typo
// in the flag must stop the rollout, not silently pick a side.
//
// Access: with mctl-api's consent route configured (GLOVO_CONSENT_API_URL
// and GLOVO_CONSENT_TOKEN_FILE), an active glovo:c2 consent admits a
// principal, and GLOVO_C2_ALLOWED_PRINCIPALS is optional: set, it narrows
// access further (a staged pilot); unset, consent alone decides. Without
// the consent route the allowlist is the only gate and is required.
func loadC2(getenv func(string) string) (c2Config, error) {
	switch v := getenv("GLOVO_C2_ENABLED"); v {
	case "", "false":
		return c2Config{}, nil
	case "true":
	default:
		return c2Config{}, fmt.Errorf("GLOVO_C2_ENABLED: %q is not true or false", v)
	}
	capacity, caps, err := loadC2Caps(getenv)
	if err != nil {
		return c2Config{}, err
	}
	c := c2Config{enabled: true, capacity: capacity, caps: caps, vaultRole: getenv("GLOVO_VAULT_ROLE")}
	if c.vaultRole == "" {
		c.vaultRole = "mctl-glovo"
	}
	c.consentAPI, c.consentTokenFile = getenv("GLOVO_CONSENT_API_URL"), getenv("GLOVO_CONSENT_TOKEN_FILE")
	if (c.consentAPI == "") != (c.consentTokenFile == "") {
		return c2Config{}, errors.New("GLOVO_CONSENT_API_URL and GLOVO_CONSENT_TOKEN_FILE are set together or not at all")
	}
	for _, p := range strings.Split(getenv("GLOVO_C2_ALLOWED_PRINCIPALS"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			if !strings.HasPrefix(p, "prn_") {
				return c2Config{}, fmt.Errorf("GLOVO_C2_ALLOWED_PRINCIPALS: %q is not a prn_ principal id", p)
			}
			c.allowed = append(c.allowed, p)
		}
	}
	if len(c.allowed) == 0 && c.consentAPI == "" {
		return c2Config{}, errors.New("GLOVO_C2_ALLOWED_PRINCIPALS is required when GLOVO_C2_ENABLED=true and GLOVO_CONSENT_API_URL is unset")
	}
	key := getenv("GLOVO_C2_AUDIT_KEY")
	if len(key) < minAuditKeyLen {
		return c2Config{}, fmt.Errorf("GLOVO_C2_AUDIT_KEY is required when GLOVO_C2_ENABLED=true and must be at least %d bytes", minAuditKeyLen)
	}
	c.auditKey = []byte(key)
	if c.databaseURL = getenv("GLOVO_DATABASE_URL"); c.databaseURL == "" {
		return c2Config{}, errors.New("GLOVO_DATABASE_URL is required when GLOVO_C2_ENABLED=true")
	}
	if c.vaultAddr = getenv("GLOVO_VAULT_ADDR"); c.vaultAddr == "" {
		return c2Config{}, errors.New("GLOVO_VAULT_ADDR is required when GLOVO_C2_ENABLED=true")
	}
	proxy := getenv("GLOVO_EGRESS_PROXY")
	if proxy == "" {
		return c2Config{}, errors.New("GLOVO_EGRESS_PROXY is required when GLOVO_C2_ENABLED=true")
	}
	u, err := url.Parse(proxy)
	if err != nil {
		return c2Config{}, errors.New("GLOVO_EGRESS_PROXY is not a URL")
	}
	c.proxy = u
	for _, s := range strings.Split(getenv("GLOVO_LINK_TRUSTED_PROXIES"), ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return c2Config{}, fmt.Errorf("GLOVO_LINK_TRUSTED_PROXIES: %q is not a CIDR", s)
		}
		c.trustedProxies = append(c.trustedProxies, p.Masked())
	}
	return c, nil
}

// c2App is everything the C2 flag mounts: the link flow and the MCP routes.
// With the flag off both are inert and nothing is registered.
type c2App struct {
	link *linkflow.Handler
	mcp  *mcpRoutes
	// ready are the extra readiness checks (token verification keys).
	ready []health.Checker
}

// Register mounts the link and MCP routes.
func (a *c2App) Register(mux *http.ServeMux) {
	a.link.Register(mux)
	a.mcp.Register(mux)
}

// Shutdown drains the MCP surface; safe with the flag off.
func (a *c2App) Shutdown(ctx context.Context) error { return a.mcp.Shutdown(ctx) }

// buildLink returns the C2 handlers. With the flag off it builds nothing:
// no store, no Vault login, no database (design 6). With it on, every
// dependency is required. The consent source is mctl-api's consent read
// route (C2-1c) when GLOVO_CONSENT_API_URL is set, and otherwise the
// fail-closed one, so no code can be issued. closeFn releases what was
// opened.
func buildLink(ctx context.Context, c c2Config, log *slog.Logger) (*c2App, func(), error) {
	noop := func() {}
	if !c.enabled {
		h, err := linkflow.New(linkflow.Config{Enabled: false})
		return &c2App{link: h}, noop, err
	}
	consent, admit, err := c2Access(c)
	if err != nil {
		return nil, noop, err
	}
	gate, err := consentgate.New(consentgate.Config{Source: consent, Version: linkflow.ConsentVersion})
	if err != nil {
		return nil, noop, err
	}
	verifier, err := authn.New(verifierConfig(c, gate, admit))
	if err != nil {
		return nil, noop, err
	}
	validate, err := glovoapi.NewSessionValidator(glovoapi.WithProxy(c.proxy))
	if err != nil {
		return nil, noop, fmt.Errorf("glovo validator: %w", err)
	}
	login, err := tokenstore.NewKubernetesLogin(tokenstore.KubernetesLoginConfig{Addr: c.vaultAddr, Role: c.vaultRole})
	if err != nil {
		return nil, noop, err
	}
	transit, err := tokenstore.NewVaultTransit(tokenstore.TransitConfig{Addr: c.vaultAddr, Token: login})
	if err != nil {
		return nil, noop, err
	}
	db, err := sql.Open("pgx", c.databaseURL)
	if err != nil {
		return nil, noop, errors.New("GLOVO_DATABASE_URL: cannot open")
	}
	closeDB := func() { _ = db.Close() }
	pg, err := tokenstore.NewPostgres(ctx, db)
	if err != nil {
		closeDB()
		return nil, noop, fmt.Errorf("token store database: %w", err)
	}
	kill, err := tokenstore.NewKillSwitch(pg)
	if err != nil {
		closeDB()
		return nil, noop, err
	}
	scfg := tokenstore.Config{
		DB:           pg,
		Transit:      transit,
		Kill:         kill,
		Logger:       log.With("component", "tokenstore"),
		ValidateLink: validate,
		// MaskIdentity needs mctl-api's identity lookup (C2-1c). Until
		// then PreviewLink refuses, so the helper never uploads.
		MaskIdentity:  nil,
		ClientOptions: []glovoapi.Option{glovoapi.WithProxy(c.proxy), glovoapi.WithRequireProxy()},
	}
	withLinkConsent(&scfg, gate)
	if err := withCaps(&scfg, c); err != nil {
		closeDB()
		return nil, noop, err
	}
	store, err := tokenstore.New(scfg)
	if err != nil {
		closeDB()
		return nil, noop, err
	}
	sessions, err := tokenstore.NewSessions(store, 0, scfg.ClientOptions...)
	if err != nil {
		closeDB()
		return nil, noop, err
	}
	// A failed first fetch is not fatal: readiness stays false and requests
	// get 503 until a refresh succeeds (as glovo-home).
	startCtx, cancelStart := context.WithTimeout(ctx, 10*time.Second)
	if err := verifier.Refresh(startCtx); err != nil {
		log.Warn("initial jwks fetch failed", "error", err)
	}
	cancelStart()
	who, err := newHMACWho(c.auditKey)
	if err != nil {
		closeDB()
		return nil, noop, err
	}
	routes, err := newMCPRoutes(mcpDeps{
		Verifier: verifier,
		Clients:  sessions,
		Audit:    log.With("log", "audit"),
		Log:      log,
		// Who is the keyed digest; the masked MCTL identity helper (build
		// infra, MaskIdentity) can replace it behind the same seam.
		Who: who,
		// SEAM(C2-3): Admit (mctl-api join/consent per-call decision) is
		// wired here by a later slice. Until then the verified principal
		// alone selects the pid.
	})
	if err != nil {
		closeDB()
		return nil, noop, err
	}
	go sweepSessions(ctx, sessions)
	h, err := linkflow.New(linkflow.Config{
		Enabled:        true,
		Store:          store,
		Verifier:       verifier,
		Consent:        consent,
		TrustedProxies: c.trustedProxies,
		Log:            log.With("component", "linkflow"),
	})
	if err != nil {
		closeDB()
		return nil, noop, err
	}
	return &c2App{link: h, mcp: routes, ready: []health.Checker{verifier.Ready}}, closeDB, nil
}

// sweepSessions drops idle hosted clients, and every client when the kill
// switch is on or unreadable, at least every MaxKillCache (Sessions.Sweep).
func sweepSessions(ctx context.Context, s *tokenstore.Sessions) {
	t := time.NewTicker(tokenstore.MaxKillCache)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Sweep(ctx)
		}
	}
}

// c2Access returns the consent source and whether it admits principals at
// the token verifier. With mctl-api's consent route configured, consent is
// the gate (the allowlist, if set, narrows it). Without it, the source is
// the fail-closed UnavailableConsent, the allowlist alone admits, and an
// empty allowlist is refused here as well as in loadC2.
func c2Access(c c2Config) (linkflow.ConsentSource, bool, error) {
	if c.consentAPI == "" {
		if len(c.allowed) == 0 {
			return nil, false, errors.New("c2: neither GLOVO_CONSENT_API_URL nor GLOVO_C2_ALLOWED_PRINCIPALS is set")
		}
		return linkflow.UnavailableConsent{}, false, nil
	}
	api, err := linkflow.NewAPIConsent(linkflow.APIConsentConfig{BaseURL: c.consentAPI, TokenFile: c.consentTokenFile})
	if err != nil {
		return nil, false, err
	}
	return api, true, nil
}

// verifierConfig is the C2 token check: a team or personal grant, the
// allowlist when set, and, in consent mode, an active consent through gate.
func verifierConfig(c c2Config, gate authn.Admitter, admit bool) authn.Config {
	vcfg := authn.Config{
		Issuer:            config.Issuer,
		Resource:          config.Resource,
		JWKSURL:           config.JWKSURL,
		AllowedPrincipals: c.allowed,
		RequireGrant:      true,
	}
	if admit {
		vcfg.Admitter = gate
	}
	return vcfg
}

// withLinkConsent makes /link/upload re-check the consent through gate after
// the code is redeemed and before the session is stored. It is set in both
// access modes: without the consent route the source never consents, so
// no session can be stored, as no code can be issued.
func withLinkConsent(cfg *tokenstore.Config, gate *consentgate.Gate) {
	cfg.LinkConsent = gate.LinkConsent
}

// withCaps sets the store's caps from the validated C2 configuration. A
// config that never went through loadC2 has zero Caps, which tokenstore.New
// would read as "use defaults"; that is refused so the configured limits can
// never be silently replaced.
func withCaps(cfg *tokenstore.Config, c c2Config) error {
	if c.caps == (tokenstore.Caps{}) {
		return errors.New("c2: caps are not configured")
	}
	cfg.Caps = c.caps
	return nil
}
