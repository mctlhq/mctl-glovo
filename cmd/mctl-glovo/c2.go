package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/mctlhq/mctl-glovo/internal/authn"
	"github.com/mctlhq/mctl-glovo/internal/config"
	"github.com/mctlhq/mctl-glovo/internal/consentgate"
	"github.com/mctlhq/mctl-glovo/internal/glovoapi"
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
	c := c2Config{enabled: true, vaultRole: getenv("GLOVO_VAULT_ROLE")}
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

// buildLink returns the link handlers. With the flag off it builds nothing:
// no store, no Vault login, no database (design 6). With it on, every
// dependency is required. The consent source is mctl-api's consent read
// route (C2-1c) when GLOVO_CONSENT_API_URL is set, and otherwise the
// fail-closed one, so no code can be issued. closeFn releases what was
// opened.
func buildLink(ctx context.Context, c c2Config, log *slog.Logger) (*linkflow.Handler, func(), error) {
	noop := func() {}
	if !c.enabled {
		h, err := linkflow.New(linkflow.Config{Enabled: false})
		return h, noop, err
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
	store, err := tokenstore.New(scfg)
	if err != nil {
		closeDB()
		return nil, noop, err
	}
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
	return h, closeDB, nil
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
