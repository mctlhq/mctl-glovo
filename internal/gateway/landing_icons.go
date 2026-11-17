package gateway

import (
	"html/template"
	"strconv"

	"github.com/mctlhq/mctl-glovo/internal/childmcp"
)

// landingStat is one figure of the stat row. The figures are derived from the
// code that serves the tools, not typed in: the tool count is the mirrored
// allowlist and the checkout figure counts the names the same allowlist would
// have to refuse. landing_figures_test.go pins both, so a change to the tool
// set must change the page on purpose.
type landingStat struct{ V, L string }

func landingStats(t landingText) []landingStat {
	return []landingStat{
		{strconv.Itoa(len(childmcp.KnownTools)), t.StatTools},
		{strconv.Itoa(landingCheckoutTools()), t.StatNoPay},
		{"OAuth 2.1", t.StatAuthL},
		{"1", t.StatAudit},
	}
}

// landingCheckoutTools counts allowlisted tools that childmcp.Blocked would
// refuse as checkout, payment or order placement. The allowlist and the block
// list are disjoint by construction, so the figure is 0 for as long as that
// holds; the test fails the build if it ever is not.
func landingCheckoutTools() int {
	n := 0
	for _, name := range childmcp.KnownTools {
		if childmcp.Blocked(name) {
			n++
		}
	}
	return n
}

// Icons are small, decorative inline SVG (the CSP admits inline markup, not
// images): stroke-only, currentColor, hidden from assistive technology.
var landingIcons = map[string]string{
	"search":   `<circle cx="11" cy="11" r="6.5"/><path d="m16 16 4.5 4.5"/>`,
	"history":  `<circle cx="12" cy="12" r="8.5"/><path d="M12 7.5V12l3 2"/>`,
	"spark":    `<path d="m12 3 1.9 5.1L19 10l-5.1 1.9L12 17l-1.9-5.1L5 10l5.1-1.9z"/><path d="m18.5 16 .7 1.8 1.8.7-1.8.7-.7 1.8-.7-1.8-1.8-.7 1.8-.7z"/>`,
	"basket":   `<path d="M4.5 9h15l-1.6 9.5H6.1z"/><path d="M8.5 9V7a3.5 3.5 0 0 1 7 0v2"/>`,
	"info":     `<circle cx="12" cy="12" r="8.5"/><path d="M12 11v5"/><path d="M12 8v.01"/>`,
	"ban":      `<circle cx="12" cy="12" r="8.5"/><path d="m6 6 12 12"/>`,
	"card":     `<rect x="3.5" y="6" width="17" height="12" rx="2"/><path d="M3.5 10.5h17"/><path d="m5 4 14 16"/>`,
	"plug":     `<path d="M9 4v4M15 4v4"/><path d="M6.5 8h11v3.5a5.5 5.5 0 0 1-11 0z"/><path d="M12 17v3.5"/>`,
	"terminal": `<rect x="3.5" y="5" width="17" height="14" rx="2"/><path d="m7.5 10 3 2.5-3 2.5"/><path d="M13 15h3.5"/>`,
	"grid":     `<rect x="4" y="4" width="6.5" height="6.5" rx="1.2"/><rect x="13.5" y="4" width="6.5" height="6.5" rx="1.2"/><rect x="4" y="13.5" width="6.5" height="6.5" rx="1.2"/><rect x="13.5" y="13.5" width="6.5" height="6.5" rx="1.2"/>`,
	"lock":     `<rect x="5" y="10.5" width="14" height="9.5" rx="2"/><path d="M8.5 10.5V8a3.5 3.5 0 0 1 7 0v2.5"/>`,
	"shield":   `<path d="M12 3.5 5 6v5.5c0 4.2 2.8 7.4 7 9 4.2-1.6 7-4.8 7-9V6z"/><path d="m9 12 2.2 2.2L15.5 10"/>`,
	"database": `<ellipse cx="12" cy="6.5" rx="7" ry="2.8"/><path d="M5 6.5v11c0 1.5 3.1 2.8 7 2.8s7-1.3 7-2.8v-11"/><path d="M5 12c0 1.5 3.1 2.8 7 2.8s7-1.3 7-2.8"/>`,
	"list":     `<path d="M8.5 7H20M8.5 12H20M8.5 17H20"/><path d="M4 7v.01M4 12v.01M4 17v.01"/>`,
}

var (
	canIcons   = []string{"search", "history", "spark", "basket"}
	neverIcons = []string{"ban", "card"}
	safeIcons  = []string{"lock", "shield", "database", "list"}
)

func landingIcon(name string) template.HTML {
	body, ok := landingIcons[name]
	if !ok {
		panic("landing: unknown icon " + name) // compile-time table; a typo must not ship
	}
	return template.HTML(`<svg class="lp-icon" viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">` + body + `</svg>`) //nolint:gosec // G203: static table above, never input
}
