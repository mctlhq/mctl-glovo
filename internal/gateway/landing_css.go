package gateway

import (
	"embed"
	"fmt"
	"regexp"
	"strings"
)

// The landing page carries the MCTL public-site shell (mctl.css tokens and
// site.css). Both sheets are vendored byte for byte from an immutable,
// versioned path on ui.mctl.ai and inlined into the one hashed <style>, so
// the CSP stays style-src 'sha256-...' with default-src 'none': nothing is
// fetched at runtime and the page cannot be restyled without a commit here.
//
// Upgrading is deliberate: change LandingDesignVersion, run
// `go run ./cmd/landing-css-sync`, review the diff, then regenerate the
// Worker bundle with `go run ./cmd/landing-gen`. CI runs
// `go run ./cmd/landing-css-sync -check` so the vendored copy cannot drift
// from the pinned version.

// LandingDesignVersion is the pinned @mctlhq/css version.
const LandingDesignVersion = "0.21.0"

// LandingDesignSource returns the pinned URL of one vendored sheet.
func LandingDesignSource(sheet string) string {
	return "https://ui.mctl.ai/" + LandingDesignVersion + "/" + sheet + ".css"
}

// LandingVendoredHeader is the first line of a vendored sheet.
func LandingVendoredHeader(sheet string) string {
	return "/* Vendored byte for byte from " + LandingDesignSource(sheet) +
		" by cmd/landing-css-sync. Do not edit. */\n"
}

//go:embed landingassets/mctl.css landingassets/site.css landingassets/landing.css
var landingAssets embed.FS

func landingAsset(name string) string {
	b, err := landingAssets.ReadFile("landingassets/" + name)
	if err != nil {
		panic(err) // embedded at build time
	}
	return string(b)
}

var lightRule = regexp.MustCompile(`\[data-theme=['"]light['"]\]\s*\{([^}]*)\}`)

// systemLightBlock makes light the page's default look and lets a dark system
// theme switch it back. The design system is dark by default and defines the
// light surfaces only under an explicit [data-theme='light']. The block is derived from that rule, so
// it can never name a variable the pinned version does not define (the same
// approach as mctl-alice's sync-tokens.mjs).
func systemLightBlock(tokens string) (string, error) {
	m := lightRule.FindStringSubmatch(tokens)
	if m == nil {
		return "", fmt.Errorf("no [data-theme='light'] rule in mctl.css: the package layout changed")
	}
	body := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(m[1], "")
	var decls []string
	for _, d := range strings.Split(body, ";") {
		if d = strings.TrimSpace(d); d != "" {
			decls = append(decls, "    "+d+";")
		}
	}
	return "/* mctl-glovo: system light theme, derived from the [data-theme='light'] rule above. */\n" +
		"@media (prefers-color-scheme:light),(prefers-color-scheme:no-preference){:root:not([data-theme]){\n" + strings.Join(decls, "\n") + "\n}}\n", nil
}

// landingCSS is the whole stylesheet: tokens, system-light block, shell,
// then the page's own rules. The CSP hash is taken over exactly this string.
var landingCSS = func() string {
	tokens := landingAsset("mctl.css")
	light, err := systemLightBlock(tokens)
	if err != nil {
		panic(err)
	}
	return tokens + light + landingAsset("site.css") + landingAsset("landing.css")
}()
