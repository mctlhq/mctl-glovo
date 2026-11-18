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
	// control is the kill switch endpoint's identity (slice 5); zero value
	// means the route is not mounted.
	control controlConfig
	// linkResource is the dedicated audience of the fresh-login token the
	// glovo-link helper sends to /link/preview and /link/upload
	// (GLOVO_LINK_RESOURCE, e.g. https://glovo.mctl.ai/link). Unset: those two
	// routes answer 503 and no session can be linked; the MCP data plane and
	// code issuing are unaffected. linkMaxAuthAge bounds the age of the
	// token's auth_time (GLOVO_LINK_MAX_AUTH_AGE, default 5m, at most 15m).
	linkResource   string
	linkMaxAuthAge time.Duration
	// basketWrites serves the basket mutation tools. No loader sets it (the
	// environment cannot: see loadBasketWrites); it is the code seam of a
	// future approval-gated change, and it needs basketApprover too.
	basketWrites bool
	// basketApprover is the per-call approval step in front of every basket
	// write (mctl-api#1011/#1012). Nil: there is none, and newMCPRoutes then
	// refuses to serve the write tools.
	basketApprover basketApprover
}

// defaultLinkMaxAuthAge is how old a login may be when a session is
// previewed or uploaded.
const defaultLinkMaxAuthAge = 5 * time.Minute

// loadLinkLogin reads GLOVO_LINK_RESOURCE and GLOVO_LINK_MAX_AUTH_AGE. A value
// that is set but wrong stops the boot: it never falls back to the MCP
// audience or a longer age.
func loadLinkLogin(getenv func(string) string) (string, time.Duration, error) {
	age := defaultLinkMaxAuthAge
	if raw := strings.TrimSpace(getenv("GLOVO_LINK_MAX_AUTH_AGE")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 || d > authn.MaxFreshLoginAge {
			return "", 0, fmt.Errorf("GLOVO_LINK_MAX_AUTH_AGE: %q must be a duration within (0, %s]", raw, authn.MaxFreshLoginAge)
		}
		age = d
	}
	res := strings.TrimSpace(getenv("GLOVO_LINK_RESOURCE"))
	if res == "" {
		return "", age, nil
	}
	u, err := url.Parse(res)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", 0, errors.New("GLOVO_LINK_RESOURCE must be an absolute https URL without userinfo, query or fragment")
	}
	if res == config.Resource {
		return "", 0, errors.New("GLOVO_LINK_RESOURCE must differ from the MCP resource: a link token must not open the MCP data plane")
	}
	return res, age, nil
}

// errBasketWritesNoGate is the startup error for GLOVO_C2_BASKET_WRITES=true.
var errBasketWritesNoGate = errors.New("GLOVO_C2_BASKET_WRITES=true is refused: hosted C2 has no approval gate for basket writes (mctl-api#1011/#1012), and an environment variable alone cannot enable them")

// loadBasketWrites reads GLOVO_C2_BASKET_WRITES, which exists only to be
// refused. Unset, "" and "false" mean read-only (the only mode: no add,
// set-quantity or remove tool is listed, a call by name is refused before
// any Glovo request, and every hosted Glovo client refuses basket
// mutations). "true" is a startup error, not a switch: until an approval gate
// exists, writes on the user-facing hosted C2 are not something a deployment
// can opt into. Anything else (case or whitespace variants, "1", "yes") is
// an error as well, as a typo must stop the rollout, and never reads as
// "off" or as "on".
//
// The seam for the future is in code, not in the environment: c2Config's
// basketWrites and basketApprover fields, which no loader sets, and
// mcpDeps.BasketApprover, without which newMCPRoutes refuses to serve the
// write tools. The enabling change adds an approval step in front of the
// writes and sets those fields in code, in the same reviewed change.
//
// It is read only with C2 on (loadC2 reads nothing else with C2 off, and no
// hosted route exists then, so there is nothing to enable).
func loadBasketWrites(getenv func(string) string) (bool, error) {
	switch v := getenv("GLOVO_C2_BASKET_WRITES"); v {
	case "", "false":
		return false, nil
	case "true":
		return false, errBasketWritesNoGate
	default:
		return false, fmt.Errorf("GLOVO_C2_BASKET_WRITES: %q is not false (and true is refused)", v)
	}
}

// basketWriteOptions is the client layer of the capability: hosted clients
// refuse basket mutations unless built with glovoapi.WithBasketWrites, so
// the default config yields none. newMCPRoutes refuses basket writes without
// an approver.
func basketWriteOptions(c c2Config) []glovoapi.Option {
	if !c.basketWrites {
		return nil
	}
	return []glovoapi.Option{glovoapi.WithBasketWrites()}
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
	if c.control, err = loadControl(getenv); err != nil {
		return c2Config{}, err
	}
	if c.linkResource, c.linkMaxAuthAge, err = loadLinkLogin(getenv); err != nil {
		return c2Config{}, err
	}
	if c.basketWrites, err = loadBasketWrites(getenv); err != nil { // never true: see loadBasketWrites
		return c2Config{}, err
	}
	c.consentAPI, c.consentTokenFile = getenv("GLOVO_CONSENT_API_URL"), getenv("GLOVO_CONSENT_TOKEN_FILE")
	if (c.consentAPI == "") != (c.consentTokenFile == "") {
		return c2Config{}, errors.New("GLOVO_CONSENT_API_URL and GLOVO_CONSENT_TOKEN_FILE are set together or not at all")
	}
	if c.consentAPI != "" {
		// Validate at boot: scheme, no userinfo/query, absolute token path.
		if _, err := linkflow.NewAPIConsent(linkflow.APIConsentConfig{BaseURL: c.consentAPI, TokenFile: c.consentTokenFile}); err != nil {
			return c2Config{}, fmt.Errorf("GLOVO_CONSENT_API_URL / GLOVO_CONSENT_TOKEN_FILE: %w", err)
		}
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
	// control is the kill switch flip route; nil when not configured.
	control *controlRoutes
	// ready are the extra readiness checks (token verification keys).
	ready []health.Checker
}

// Register mounts the link and MCP routes.
func (a *c2App) Register(mux *http.ServeMux) {
	a.link.Register(mux)
	a.mcp.Register(mux)
	a.control.Register(mux)
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
	scfg := storeConfig(c, pg, transit, kill, validate, consent, gate, log)
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
	// Per-call decision: only in consent mode. In allowlist-only mode the
	// consent source never consents, so an admitter would refuse everyone;
	// there the verifier's allowlist is the gate.
	callAdmit, err := admitterFor(consent, admit)
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
		Who:  who,
		Kill: kill.Check,
		// Admit is mctl-api's consent (and slot) decision per tool call,
		// with a few-second positive cache (admit.go).
		Admit:          callAdmit,
		BasketWrites:   c.basketWrites,
		BasketApprover: c.basketApprover,
	})
	if err != nil {
		closeDB()
		return nil, noop, err
	}
	go sweepSessions(ctx, sessions)
	linkVerifier, err := buildLinkVerifier(ctx, c, gate, admit, log)
	if err != nil {
		closeDB()
		return nil, noop, err
	}
	h, err := linkflow.New(linkflow.Config{
		Enabled:        true,
		Store:          store,
		Verifier:       verifier,
		LinkVerifier:   linkVerifier,
		Consent:        consent,
		TrustedProxies: c.trustedProxies,
		Log:            log.With("component", "linkflow"),
	})
	if err != nil {
		closeDB()
		return nil, noop, err
	}
	app := &c2App{link: h, mcp: routes, ready: []health.Checker{verifier.Ready}}
	if c.control.enabled() {
		app.control, err = buildControl(ctx, c.control, kill, log)
		if err != nil {
			closeDB()
			return nil, noop, err
		}
	}
	return app, closeDB, nil
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

// storeConfig is the token store's configuration without the caps: the
// masked-identity lookup comes from mctl-api's consent route (nil without it,
// so the preview refuses) and the upload re-checks consent through gate.
func storeConfig(c c2Config, db tokenstore.DB, transit tokenstore.Transit, kill *tokenstore.KillSwitch,
	validate func(context.Context, glovoapi.Session) error, consent linkflow.ConsentSource, gate *consentgate.Gate, log *slog.Logger) tokenstore.Config {
	scfg := tokenstore.Config{
		DB:            db,
		Transit:       transit,
		Kill:          kill,
		Logger:        log.With("component", "tokenstore"),
		ValidateLink:  validate,
		MaskIdentity:  maskIdentityFrom(consent),
		ClientOptions: append([]glovoapi.Option{glovoapi.WithProxy(c.proxy), glovoapi.WithRequireProxy()}, basketWriteOptions(c)...),
	}
	withLinkConsent(&scfg, gate)
	return scfg
}

// maskIdentityFrom is the store's masked-identity lookup: mctl-api's label
// when the consent route is configured, nil otherwise (the preview then
// refuses). pid is a string only because the store's seam takes one; it is
// re-validated into a PID before the read.
func maskIdentityFrom(src linkflow.ConsentSource) func(context.Context, string) (string, error) {
	api, ok := src.(*linkflow.APIConsent)
	if !ok {
		return nil
	}
	return func(ctx context.Context, pid string) (string, error) {
		return api.MaskedIdentity(ctx, tokenstore.PIDFrom(&authn.Principal{ID: pid}))
	}
}

// linkVerifierConfig is the check on a preview or an upload: the dedicated
// link audience and a fresh login. No grant is required (a link token is not
// a data-plane token); the allowlist and the consent admission still apply.
func linkVerifierConfig(c c2Config, gate authn.Admitter, admit bool) authn.Config {
	vcfg := verifierConfig(c, gate, admit)
	vcfg.Resource = c.linkResource
	vcfg.RequireGrant = false
	vcfg.MaxAuthAge = c.linkMaxAuthAge
	return vcfg
}

// buildLinkVerifier returns the link-route verifier, or nil when
// GLOVO_LINK_RESOURCE is unset (preview and upload then refuse).
func buildLinkVerifier(ctx context.Context, c c2Config, gate authn.Admitter, admit bool, log *slog.Logger) (linkflow.Verifier, error) {
	if c.linkResource == "" {
		log.Warn("GLOVO_LINK_RESOURCE is unset: /link/preview and /link/upload refuse every request")
		return nil, nil
	}
	v, err := authn.New(linkVerifierConfig(c, gate, admit))
	if err != nil {
		return nil, err
	}
	startCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := v.Refresh(startCtx); err != nil {
		log.Warn("initial link jwks fetch failed", "error", err)
	}
	return v, nil
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

// buildControl wires the kill switch flip route: the machine-user token
// verifier (internal/authn, the subject allow-list) over the durable switch.
// The first key set fetch is best effort: until it works the endpoint
// answers 503, which mctl-api reads as "state unknown", and the service
// itself stays ready (the flip path being down must not take MCP down).
func buildControl(ctx context.Context, cc controlConfig, kill *tokenstore.KillSwitch, log *slog.Logger) (*controlRoutes, error) {
	v, err := authn.NewService(authn.ServiceConfig{
		Issuer: cc.issuer, Audience: cc.audience, JWKSURL: cc.jwksURL, Subjects: cc.subjects,
	})
	if err != nil {
		return nil, err
	}
	startCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := v.Refresh(startCtx); err != nil {
		log.Warn("initial control jwks fetch failed", "error", err)
	}
	return newControlRoutes(v, kill, log.With("component", "control"), log.With("log", "audit"))
}
