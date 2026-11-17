package gateway

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/mctlhq/mctl-glovo/internal/childmcp"
)

var declRe = regexp.MustCompile(`(--[a-z0-9-]+)\s*:\s*([^;}]+)`)

// firstBlock returns the body of the first "open ... }" block in s.
func firstBlock(t *testing.T, s, open string) string {
	t.Helper()
	i := strings.Index(s, open)
	if i < 0 {
		t.Fatalf("no %q in landing.css", open)
	}
	body, _, ok := strings.Cut(s[i+len(open):], "}")
	if !ok {
		t.Fatalf("unterminated %q block", open)
	}
	return body
}

func decls(block string) map[string]string {
	m := map[string]string{}
	for _, d := range declRe.FindAllStringSubmatch(block, -1) {
		m[d[1]] = strings.TrimSpace(d[2])
	}
	return m
}

// themeTokens returns the resolved token map of one theme of the page:
// the design's flat --mctl-* tokens, landing.css's :root block and, for dark,
// its prefers-color-scheme:dark overlay.
func themeTokens(t *testing.T, dark bool) map[string]string {
	t.Helper()
	css := landingAsset("landing.css")
	m := decls(landingAsset("mctl.css"))
	root := firstBlock(t, css, ":root{")
	for k, v := range decls(root) {
		m[k] = v
	}
	if dark {
		i := strings.Index(css, "@media (prefers-color-scheme:dark){")
		if i < 0 {
			t.Fatal("no dark overlay in landing.css")
		}
		blk := firstBlock(t, css[i:], ":root{")
		for k, v := range decls(blk) {
			m[k] = v
		}
	}
	return m
}

var varRe = regexp.MustCompile(`^var\((--[a-z0-9-]+)\)$`)

func resolve(t *testing.T, m map[string]string, name string) [3]float64 {
	t.Helper()
	v := m[name]
	for i := 0; i < 8; i++ {
		g := varRe.FindStringSubmatch(v)
		if g == nil {
			break
		}
		v = m[g[1]]
	}
	if !regexp.MustCompile(`^#[0-9a-fA-F]{6}$`).MatchString(v) {
		t.Fatalf("token %s does not resolve to a #rrggbb colour: %q", name, v)
	}
	var c [3]float64
	for i := 0; i < 3; i++ {
		n, _ := strconv.ParseUint(v[1+2*i:3+2*i], 16, 8)
		c[i] = float64(n) / 255
	}
	return c
}

func lum(c [3]float64) float64 {
	f := func(x float64) float64 {
		if x <= 0.03928 {
			return x / 12.92
		}
		return math.Pow((x+0.055)/1.055, 2.4)
	}
	return 0.2126*f(c[0]) + 0.7152*f(c[1]) + 0.0722*f(c[2])
}

func ratio(a, b [3]float64) float64 {
	la, lb := lum(a), lum(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// Every text-on-surface pair the page draws, by token. Text must reach AA
// (4.5:1) in both themes; the icon and border pairs are non-text (3:1).
var contrastPairs = []struct {
	fg, bg string
	min    float64
}{
	{"--l-fg", "--l-bg", 4.5}, {"--l-muted", "--l-bg", 4.5}, {"--l-subtle", "--l-bg", 4.5},
	{"--l-fg", "--l-card", 4.5}, {"--l-muted", "--l-card", 4.5},
	{"--l-accent-text", "--l-card", 4.5}, {"--l-accent-text", "--l-bg", 4.5},
	{"--l-accent-text", "--l-chip-bg", 4.5}, {"--l-fg", "--l-chip-bg", 4.5},
	{"--l-accent-fg", "--l-accent", 4.5},
	{"--l-band-fg", "--l-band", 4.5}, {"--l-band-muted", "--l-band", 4.5},
	{"--l-fg", "--l-code-bg", 4.5}, {"--l-fg", "--l-note-bg", 4.5},
	{"--l-fg", "--l-never-bg", 4.5},
	{"--l-accent", "--l-card", 3}, {"--l-accent", "--l-icon-bg", 3}, {"--l-accent", "--l-note-bg", 3},
	{"--l-accent", "--l-never-bg", 3}, {"--l-focus", "--l-band", 3}, {"--l-band-fg", "--l-band", 3},
}

func TestLandingContrast(t *testing.T) {
	for _, dark := range []bool{false, true} {
		m := themeTokens(t, dark)
		name := map[bool]string{false: "light", true: "dark"}[dark]
		for _, p := range contrastPairs {
			r := ratio(resolve(t, m, p.fg), resolve(t, m, p.bg))
			t.Logf("%s %s on %s: %.2f", name, p.fg, p.bg, r)
			if r < p.min {
				t.Errorf("%s: %s on %s is %.2f:1, want >= %.1f", name, p.fg, p.bg, r, p.min)
			}
		}
	}
}

// The meta theme-color values are the page backgrounds of the two themes.
func TestLandingThemeColorsMatchPageBackgrounds(t *testing.T) {
	light := themeTokens(t, false)
	dark := themeTokens(t, true)
	hex := func(c [3]float64) string {
		return "#" + strings.Join([]string{h2(c[0]), h2(c[1]), h2(c[2])}, "")
	}
	if got := hex(resolve(t, light, "--l-bg")); got != landingThemeLight {
		t.Errorf("light page background %s != landingThemeLight %s", got, landingThemeLight)
	}
	if got := hex(resolve(t, dark, "--l-bg")); got != landingThemeDark {
		t.Errorf("dark page background %s != landingThemeDark %s", got, landingThemeDark)
	}
}

func h2(x float64) string {
	const d = "0123456789abcdef"
	n := int(math.Round(x * 255))
	return string([]byte{d[n>>4], d[n&15]})
}

// The Glovo flavour is two tokens and nothing more.
func TestLandingYellowIsTwoTokens(t *testing.T) {
	css := landingAsset("landing.css")
	for _, tok := range []string{"--l-yellow:#ffc244;", "--l-cream:#fff8e6;"} {
		if strings.Count(css, tok) != 1 {
			t.Errorf("want exactly one declaration %q", tok)
		}
	}
	// Brand yellow literals appear only in those declarations.
	if n := strings.Count(strings.ToLower(css), "ffc244"); n != 1 {
		t.Errorf("yellow literal appears %d times, want 1 (the --l-yellow token)", n)
	}
}

func TestLandingRedesignElements(t *testing.T) {
	for l, page := range renderAll(t) {
		tx := landingTexts[l]
		for _, want := range []string{
			`class="lp-hero"`, `class="mctl-site-chip lp-eyebrow"`, `<h1 `,
			`class="btn btn-primary" href="#connect">` + tx.BtnConnect + `</a>`,
			`class="btn btn-secondary" href="#can">` + tx.CanH + `</a>`,
			`<code class="lp-url">` + resource + `</code>`,
			`id="can"`, `id="connect"`, `id="never"`, `class="lp-grid lp-grid--2 lp-never"`,
			tx.StatTools, tx.StatNoPay, tx.StatAuthL, tx.StatAudit, tx.CanNote,
			tx.Footer,
		} {
			if !strings.Contains(page, want) {
				t.Errorf("%s: page lacks %q", l, want)
			}
		}
		if n := strings.Count(page, `class="lp-stat"`); n != 4 {
			t.Errorf("%s: %d stat tiles, want 4", l, n)
		}
		svgs := strings.Count(page, "<svg ")
		if svgs != len(tx.Can)+len(tx.Never)+len(tx.Safe)+4 { // + note, plug, terminal, grid
			t.Errorf("%s: %d icons", l, svgs)
		}
		if strings.Count(page, `aria-hidden="true" focusable="false"`) != svgs {
			t.Errorf("%s: an icon is not aria-hidden", l)
		}
	}
}

// Nothing the CSP would block, and nothing that suggests Glovo artwork:
// no script, no image, no external resource; inline SVG only.
func TestLandingNoScriptNoExternalNoImages(t *testing.T) {
	for l, page := range renderAll(t) {
		low := strings.ToLower(page)
		for _, banned := range []string{"<script", "<img", "<picture", "<video", "<iframe", "<link rel=\"stylesheet", "<link rel=\"preload", "url(", "@import", "javascript:", "onclick=", "<object", "<embed", "srcset", " src="} {
			if strings.Contains(low, banned) {
				t.Errorf("%s: page contains %q", l, banned)
			}
		}
		for _, m := range regexp.MustCompile(`(?:href|src|action)="(https?:[^"]*)"`).FindAllStringSubmatch(page, -1) {
			u := m[1]
			if !strings.HasPrefix(u, "https://mctl.ai") && !strings.HasPrefix(u, "https://glovo.mctl.ai/") {
				t.Errorf("%s: unexpected absolute link %s", l, u)
			}
		}
	}
}

func TestLandingDisclaimerInAllLanguages(t *testing.T) {
	want := map[string]string{
		"en": "Not affiliated with or endorsed by Glovo.",
		"es": "Sin relación con Glovo ni respaldo de Glovo.",
		"ru": "Не связан с Glovo и не одобрен Glovo.",
	}
	for l, page := range renderAll(t) {
		if !strings.Contains(page, `<p class="mctl-site-footer__copy">`+want[l]+`</p>`) {
			t.Errorf("%s: footer disclaimer missing or changed", l)
		}
	}
}

// Figures on the page come from the code. 27 is pinned on purpose: changing
// the tool set must change this number, and so the page, in the same commit.
func TestLandingFiguresMatchTheToolSet(t *testing.T) {
	const pinnedTools = 27
	if len(childmcp.KnownTools) != pinnedTools {
		t.Fatalf("KnownTools has %d tools; the landing page figure was pinned at %d. Review the page copy, then update pinnedTools", len(childmcp.KnownTools), pinnedTools)
	}
	forbidden := []string{"checkout", "payment", "pay", "paying", "charge", "buy", "place", "placeorder", "submit", "submitorder", "confirm", "finalize", "finalise", "execute", "ordernow"}
	n := 0
	for _, name := range childmcp.KnownTools {
		for _, w := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return r == '_' }) {
			for _, f := range forbidden {
				if w == f {
					n++
				}
			}
		}
	}
	if n != 0 {
		t.Fatalf("%d allowlisted tools carry checkout/payment words", n)
	}
	if got := landingCheckoutTools(); got != 0 {
		t.Fatalf("landingCheckoutTools = %d", got)
	}
	for l, page := range renderAll(t) {
		tx := landingTexts[l]
		for _, s := range []string{
			`<span class="lp-stat__v">27</span><span class="lp-stat__l">` + tx.StatTools + `</span>`,
			`<span class="lp-stat__v">0</span><span class="lp-stat__l">` + tx.StatNoPay + `</span>`,
			`<span class="lp-stat__v">OAuth 2.1</span>`,
			`<span class="lp-stat__v">1</span><span class="lp-stat__l">` + tx.StatAudit + `</span>`,
		} {
			if !strings.Contains(page, s) {
				t.Errorf("%s: stat tile missing: %s", l, s)
			}
		}
	}
}

// Icon lists must cover every card, or a template index would panic at runtime.
func TestLandingIconsCoverCards(t *testing.T) {
	for l, tx := range landingTexts {
		if len(tx.Can) != len(canIcons) || len(tx.Never) != len(neverIcons) || len(tx.Safe) != len(safeIcons) {
			t.Errorf("%s: card and icon counts differ", l)
		}
	}
	for _, set := range [][]string{canIcons, neverIcons, safeIcons} {
		for _, n := range set {
			if _, ok := landingIcons[n]; !ok {
				t.Errorf("unknown icon %s", n)
			}
		}
	}
}

// The container must not shrink to a narrow column on a wide screen, nor
// stretch past a readable width.
func TestLandingContainerWidth(t *testing.T) {
	m := regexp.MustCompile(`--site-content-width:(\d+)px`).FindStringSubmatch(landingAsset("landing.css"))
	if m == nil {
		t.Fatal("no --site-content-width override")
	}
	w, _ := strconv.Atoi(m[1])
	if w < 1080 || w > 1200 {
		t.Fatalf("content width %d outside 1080-1200", w)
	}
}
