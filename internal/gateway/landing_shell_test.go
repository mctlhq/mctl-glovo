package gateway

import (
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

func renderAll(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, l := range landingLangs {
		b, err := RenderLanding(l, resource)
		if err != nil {
			t.Fatal(err)
		}
		out[l] = string(b)
	}
	return out
}

func TestLandingUsesPlatformShell(t *testing.T) {
	for l, page := range renderAll(t) {
		tx := landingTexts[l]
		for _, want := range []string{
			`<header class="mctl-site-header`,
			`class="mctl-site-brand"`, `<span class="mctl-site-brand__accent">M</span><span>CTL</span>`,
			`<nav class="mctl-site-nav" aria-label="` + tx.LangNavL + `">`,
			`<footer class="mctl-site-footer">`, `class="mctl-site-footer__links"`, `class="mctl-site-footer__copy"`,
			`class="mctl-site-chip"`,
			`<a class="skip-link" href="#main">` + tx.SkipL + `</a>`, `<main id="main">`,
			`<pre tabindex="0">`,
			`href="https://mctl.ai/privacy/"`, `href="mailto:support@mctl.ai"`,
			tx.Footer, tx.PrivacyL, tx.ContactL,
		} {
			if !strings.Contains(page, want) {
				t.Errorf("%s: page lacks %q", l, want)
			}
		}
		if strings.Contains(page, `class="pill"`) || strings.Contains(page, "style=") {
			t.Errorf("%s: legacy pill or inline style attribute present", l)
		}
	}
}

func TestLandingLanguageLinksAreAccessible(t *testing.T) {
	for l, page := range renderAll(t) {
		if n := len(regexp.MustCompile(`<a [^>]*aria-current="page"`).FindAllString(page, -1)); n != 1 {
			t.Errorf("%s: %d aria-current=page, want exactly 1", l, n)
		}
		if strings.Contains(page, ` aria-current="true"`) {
			t.Errorf("%s: legacy aria-current=true", l)
		}
		for _, c := range landingLangs {
			want := `<a href="/?lang=` + c + `" lang="` + c + `" hreflang="` + c + `" aria-label="` + landingLangNames[c] + ` (` + strings.ToUpper(c) + `)"`
			if c == l {
				want += ` aria-current="page"`
			}
			if !strings.Contains(page, want+">") {
				t.Errorf("%s: language link for %s missing or wrong: want %q", l, c, want)
			}
		}
	}
}

func TestLandingHeadMetadata(t *testing.T) {
	const origin = "https://glovo.mctl.ai/"
	for l, page := range renderAll(t) {
		tx := landingTexts[l]
		want := []string{
			`<meta name="robots" content="noindex">`,
			`<link rel="canonical" href="` + origin + `?lang=` + l + `">`,
			`<link rel="alternate" hreflang="x-default" href="` + origin + `">`,
			`<meta property="og:type" content="website">`,
			`<meta property="og:url" content="` + origin + `?lang=` + l + `">`,
			`<meta property="og:locale" content="` + tx.Locale + `">`,
			`<meta name="twitter:card" content="summary">`,
			`<meta name="theme-color" media="(prefers-color-scheme: dark)" content="` + landingThemeDark + `">`,
			`<meta name="theme-color" media="(prefers-color-scheme: light)" content="` + landingThemeLight + `">`,
		}
		for _, c := range landingLangs {
			want = append(want, `<link rel="alternate" hreflang="`+c+`" href="`+origin+`?lang=`+c+`">`)
		}
		for _, w := range want {
			if !strings.Contains(page, w) {
				t.Errorf("%s: head lacks %q", l, w)
			}
		}
		// Self-canonical: the canonical of a variant must be one of its own hreflang alternates.
		if n := strings.Count(page, `rel="canonical"`); n != 1 {
			t.Errorf("%s: %d canonical links", l, n)
		}
		// No image or icon: img-src is 'none' under the CSP and the Worker
		// route serves "/" only, so neither could be fetched.
		for _, banned := range []string{"og:image", "twitter:image", `rel="icon"`, `rel="shortcut icon"`} {
			if strings.Contains(page, banned) {
				t.Errorf("%s: %q cannot be served within the CSP and route", l, banned)
			}
		}
	}
}

func TestLandingOriginFallsBack(t *testing.T) {
	if got := landingOrigin("https://glovo.mctl.ai/mcp"); got != "https://glovo.mctl.ai/" {
		t.Fatalf("origin %q", got)
	}
	if got := landingOrigin("not a url"); got != "/" {
		t.Fatalf("origin %q", got)
	}
}

func TestLandingVendoredCSSIsPinned(t *testing.T) {
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(LandingDesignVersion) {
		t.Fatalf("version %q is not an exact semver (no floating path)", LandingDesignVersion)
	}
	for _, sheet := range []string{"mctl", "site"} {
		css := landingAsset(sheet + ".css")
		head := LandingVendoredHeader(sheet)
		if !strings.HasPrefix(css, head) {
			t.Fatalf("%s.css does not start with %q: run `go run ./cmd/landing-css-sync`", sheet, head)
		}
		// The upstream sheet names its own version on its first line.
		first, _, _ := strings.Cut(strings.TrimPrefix(css, head), "\n")
		if !strings.HasPrefix(first, "/* @mctlhq/css "+LandingDesignVersion+" ") {
			t.Fatalf("%s.css upstream header %q is not version %s", sheet, first, LandingDesignVersion)
		}
		if strings.Contains(css, "@import") || strings.Contains(css, "url(") {
			t.Fatalf("%s.css fetches a resource, which the CSP forbids", sheet)
		}
	}
}

func TestSystemLightBlock(t *testing.T) {
	b, err := systemLightBlock(landingAsset("mctl.css"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"@media (prefers-color-scheme:light)", ":root:not([data-theme])", "--surface-bg:", "--surface-fg:"} {
		if !strings.Contains(b, want) {
			t.Errorf("light block lacks %q", want)
		}
	}
	if strings.Contains(b, "/*") && strings.Count(b, "/*") != 1 {
		t.Error("upstream comments leaked into the derived block")
	}
	if _, err := systemLightBlock(":root{}"); err == nil {
		t.Error("a sheet without the light rule must be an error, not an empty block")
	}
}

// The theme-color meta values must be the design system's page backgrounds.
func TestLandingThemeColorsMatchTokens(t *testing.T) {
	tokens := landingAsset("mctl.css")
	for name, want := range map[string]string{"dark": landingThemeDark, "light": landingThemeLight} {
		decl := "--mctl-surface-" + name + "-bg: " + want + ";"
		if !strings.Contains(tokens, decl) {
			t.Errorf("tokens do not declare %q", decl)
		}
	}
}

// The endpoint is code on a card; its colour must stay a neutral token, never
// the accent (2.88:1 on the old background, below AA).
func TestLandingEndpointContrastToken(t *testing.T) {
	m := regexp.MustCompile(`\.endpoint\{([^}]*)\}`).FindStringSubmatch(landingAsset("landing.css"))
	if m == nil {
		t.Fatal("no .endpoint rule")
	}
	if strings.Contains(m[1], "accent") || strings.Contains(m[1], "color:") {
		t.Fatalf(".endpoint sets its own colour: %q", m[1])
	}
}

// The CSP must hash exactly the <style> element the page carries. The hash is
// computed here from the served HTML, not from the landingCSS variable, so a
// mismatch between header and body fails even if both come from one source.
func TestLandingCSPHashMatchesServedStyle(t *testing.T) {
	ts := landingServer(t)
	for _, l := range landingLangs {
		r, body := do(t, "GET", ts.URL+"/?lang="+l, nil, "")
		csp := r.Header.Get("Content-Security-Policy")
		if n := strings.Count(body, "<style>"); n != 1 {
			t.Fatalf("%s: %d <style> elements, want 1", l, n)
		}
		style := body[strings.Index(body, "<style>")+len("<style>") : strings.Index(body, "</style>")]
		sum := sha256.Sum256([]byte(style))
		want := "default-src 'none'; style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) +
			"'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
		if csp != want {
			t.Fatalf("%s: CSP\n got %q\nwant %q", l, csp, want)
		}
	}
}

func cssRule(css, selector string) string {
	i := strings.Index(css, selector+"{")
	if i < 0 {
		return ""
	}
	rest := css[i+len(selector)+1:]
	body, _, _ := strings.Cut(rest, "}")
	return body
}

// The focused skip link must stack above the sticky header, which
// site.css puts at --mctl-z-index-nav. A literal z-index would silently lose
// to a token change, so the link must use a token that is strictly higher.
func TestLandingSkipLinkStacksAboveHeader(t *testing.T) {
	tokens := landingAsset("mctl.css")
	z := func(name string) int {
		m := regexp.MustCompile(`--mctl-z-index-` + name + `: (\d+);`).FindStringSubmatch(tokens)
		if m == nil {
			t.Fatalf("no z-index token %s", name)
		}
		n := 0
		for _, c := range m[1] {
			n = n*10 + int(c-'0')
		}
		return n
	}
	hdr := regexp.MustCompile(`\.mctl-site-header \{[^}]*z-index: var\(--mctl-z-index-([a-z]+)\)`).FindStringSubmatch(landingAsset("site.css"))
	if hdr == nil {
		t.Fatal("site.css header z-index is no longer a token: re-check the skip link")
	}
	skip := regexp.MustCompile(`z-index:var\(--mctl-z-index-([a-z]+)\)`).FindStringSubmatch(cssRule(landingAsset("landing.css"), ".skip-link"))
	if skip == nil {
		t.Fatal(".skip-link must take its z-index from a --mctl-z-index-* token")
	}
	if z(skip[1]) <= z(hdr[1]) {
		t.Fatalf("skip link z-index %s (%d) must exceed header %s (%d)", skip[1], z(skip[1]), hdr[1], z(hdr[1]))
	}
}
