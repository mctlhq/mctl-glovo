package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"html/template"
	"net/http"
	"net/url"
	"strings"
)

// The landing page at "/": what the server is, how to connect a client and
// what it will never do. Server-rendered in English, Spanish or Russian, no
// JavaScript, no external resources (the shared MCTL stylesheet is vendored
// into the page, see landing_css.go). The only inline style is admitted by
// its hash, so the page keeps a CSP as strict as the API routes.

const langCookie = "glovo_lang"

var landingLangs = []string{"en", "es", "ru"}

type landingText struct {
	Lang, Title, Lead, Preview                string
	ConnectH, EndpointL, ClaudeAI, ClaudeCode string
	ClaudeCodeThen, Other                     string
	CanH                                      string
	Can                                       []string
	NeverH                                    string
	Never                                     []string
	SafeH                                     string
	Safe                                      []string
	Footer                                    string
	Locale, ProductName                       string
	SkipL, LangNavL, FooterNavL               string
	PrivacyL, ContactL                        string
}

var landingTexts = map[string]landingText{
	"en": {
		Lang:           "en",
		Title:          "Glovo for your AI assistant",
		Lead:           "Connect Claude or any MCP client to your Glovo account: find stores and dishes, look through past orders, get suggestions and prepare the basket. Each invited person links their own account. You place and pay for the order yourself, in the Glovo app.",
		Preview:        "Private preview: access is by invitation only.",
		ConnectH:       "Connect",
		EndpointL:      "MCP endpoint",
		ClaudeAI:       "claude.ai: Settings → Connectors → Add custom connector, paste the endpoint and sign in with MCTL.",
		ClaudeCode:     "Claude Code:",
		ClaudeCodeThen: "then run /mcp and sign in with MCTL.",
		Other:          "Other clients: any MCP client with Streamable HTTP and OAuth sign-in.",
		CanH:           "What it can do",
		Can: []string{
			"Search stores, menus and products near your address.",
			"Read your order history and statistics.",
			"Suggest what to order and plan a reorder.",
			"Add, remove and change items in your Glovo basket.",
			"Basket changes go straight to your real Glovo basket, with no approval step. You place and pay for the order in the Glovo app.",
		},
		NeverH: "What it never does",
		Never: []string{
			"No checkout, payment or order placement. Those tools are not offered, and calls to them are refused by name.",
			"The tools never read or return payment details.",
		},
		SafeH: "Security",
		Safe: []string{
			"Sign-in goes through MCTL, or with Google (OAuth 2.1, api.mctl.ai). Every request is checked. Only invited accounts get in.",
			"The Glovo session is never sent to the AI client.",
			"Data processed: your order history and addresses, to answer your requests.",
			"Every tool call writes one audit line, without arguments or results.",
		},
		Footer: "Not affiliated with or endorsed by Glovo.",
		Locale: "en_US", ProductName: "Glovo MCP",
		SkipL: "Skip to content", LangNavL: "Language", FooterNavL: "Footer",
		PrivacyL: "Privacy", ContactL: "Contact",
	},
	"es": {
		Lang:           "es",
		Title:          "Glovo para tu asistente de IA",
		Lead:           "Conecta Claude o cualquier cliente MCP a tu cuenta de Glovo: busca tiendas y platos, revisa pedidos anteriores, recibe sugerencias y prepara la cesta. Cada persona invitada vincula su propia cuenta. El pedido lo haces y lo pagas tú, en la app de Glovo.",
		Preview:        "Vista previa privada: acceso solo por invitación.",
		ConnectH:       "Conectar",
		EndpointL:      "Endpoint MCP",
		ClaudeAI:       "claude.ai: Configuración → Conectores → Añadir conector personalizado, pega el endpoint e inicia sesión con MCTL.",
		ClaudeCode:     "Claude Code:",
		ClaudeCodeThen: "después ejecuta /mcp e inicia sesión con MCTL.",
		Other:          "Otros clientes: cualquier cliente MCP con Streamable HTTP e inicio de sesión OAuth.",
		CanH:           "Qué puede hacer",
		Can: []string{
			"Buscar tiendas, menús y productos cerca de tu dirección.",
			"Leer tu historial de pedidos y estadísticas.",
			"Sugerir qué pedir y planificar un pedido repetido.",
			"Añadir, quitar y cambiar productos en tu cesta de Glovo.",
			"Los cambios van directo a tu cesta real de Glovo, sin paso de aprobación. El pedido lo haces y lo pagas en la app de Glovo.",
		},
		NeverH: "Qué nunca hace",
		Never: []string{
			"Sin tramitar pedidos, sin pagar, sin realizar pedidos. Esas herramientas no se ofrecen y las llamadas a ellas se rechazan por nombre.",
			"Las herramientas nunca leen ni devuelven datos de pago.",
		},
		SafeH: "Seguridad",
		Safe: []string{
			"El inicio de sesión pasa por MCTL, o con Google (OAuth 2.1, api.mctl.ai). Cada solicitud se verifica. Solo entran cuentas invitadas.",
			"La sesión de Glovo nunca se envía al cliente de IA.",
			"Datos tratados: tu historial de pedidos y tus direcciones, para responder a tus solicitudes.",
			"Cada llamada a una herramienta deja una línea de auditoría, sin argumentos ni resultados.",
		},
		Footer: "Sin relación con Glovo ni respaldo de Glovo.",
		Locale: "es_ES", ProductName: "Glovo MCP",
		SkipL: "Saltar al contenido", LangNavL: "Idioma", FooterNavL: "Pie de página",
		PrivacyL: "Privacidad", ContactL: "Contacto",
	},
	"ru": {
		Lang:           "ru",
		Title:          "Glovo для вашего AI-ассистента",
		Lead:           "Подключите Claude или любой MCP-клиент к вашему аккаунту Glovo: поиск магазинов и блюд, история заказов, подсказки и подготовка корзины. Каждый приглашённый подключает свой аккаунт. Заказ вы оформляете и оплачиваете сами, в приложении Glovo.",
		Preview:        "Закрытый доступ: только по приглашению.",
		ConnectH:       "Подключение",
		EndpointL:      "MCP-адрес",
		ClaudeAI:       "claude.ai: Settings → Connectors → Add custom connector, вставьте адрес и войдите через MCTL.",
		ClaudeCode:     "Claude Code:",
		ClaudeCodeThen: "затем /mcp и вход через MCTL.",
		Other:          "Другие клиенты: любой MCP-клиент со Streamable HTTP и входом по OAuth.",
		CanH:           "Что умеет",
		Can: []string{
			"Искать магазины, меню и товары рядом с вашим адресом.",
			"Читать историю заказов и статистику.",
			"Подсказывать, что заказать, и подготовить повторный заказ.",
			"Добавлять, убирать и менять товары в корзине Glovo.",
			"Изменения сразу попадают в вашу настоящую корзину Glovo, без шага подтверждения. Заказ вы оформляете и оплачиваете в приложении Glovo.",
		},
		NeverH: "Чего не делает никогда",
		Never: []string{
			"Не оформляет заказ, не платит и не отправляет заказ. Таких инструментов нет, а вызовы по их именам отклоняются.",
			"Инструменты никогда не читают и не возвращают платёжные данные.",
		},
		SafeH: "Безопасность",
		Safe: []string{
			"Вход через MCTL или через Google (OAuth 2.1, api.mctl.ai). Каждый запрос проверяется. Пускают только приглашённые аккаунты.",
			"Сессия Glovo никогда не передаётся AI-клиенту.",
			"Обрабатываются история заказов и адреса, чтобы отвечать на ваши запросы.",
			"Каждый вызов инструмента оставляет одну строку аудита, без аргументов и результатов.",
		},
		Footer: "Не связан с Glovo и не одобрен Glovo.",
		Locale: "ru_RU", ProductName: "Glovo MCP",
		SkipL: "К содержимому", LangNavL: "Язык", FooterNavL: "Нижняя навигация",
		PrivacyL: "Конфиденциальность", ContactL: "Контакты",
	},
}

var landingTmpl = template.Must(template.New("landing").Parse(`<!doctype html>
<html lang="{{.T.Lang}}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.T.Title}}</title>
<meta name="description" content="{{.T.Lead}}">
<meta name="robots" content="noindex">
<meta name="color-scheme" content="dark light">
<meta name="theme-color" media="(prefers-color-scheme: dark)" content="` + landingThemeDark + `">
<meta name="theme-color" media="(prefers-color-scheme: light)" content="` + landingThemeLight + `">
<link rel="canonical" href="{{.Canonical}}">
{{range .Langs}}<link rel="alternate" hreflang="{{.Code}}" href="{{.Alt}}">
{{end}}<link rel="alternate" hreflang="x-default" href="{{.Canonical}}">
<meta property="og:type" content="website">
<meta property="og:site_name" content="MCTL">
<meta property="og:title" content="{{.T.Title}}">
<meta property="og:description" content="{{.T.Lead}}">
<meta property="og:url" content="{{.Canonical}}">
<meta property="og:locale" content="{{.T.Locale}}">
<meta name="twitter:card" content="summary">
<meta name="twitter:title" content="{{.T.Title}}">
<meta name="twitter:description" content="{{.T.Lead}}">
<style>{{.CSS}}</style>
</head>
<body>
<a class="skip-link" href="#main">{{.T.SkipL}}</a>
<header class="mctl-site-header mctl-site-header--app">
<div class="mctl-site-container mctl-site-header__inner">
<a class="mctl-site-brand" href="https://mctl.ai" aria-label="MCTL"><span class="mctl-site-brand__accent">M</span><span>CTL</span></a>
<span class="mctl-site-header__context">{{.T.ProductName}}</span>
<nav class="mctl-site-nav" aria-label="{{.T.LangNavL}}">{{range .Langs}}<a href="/?lang={{.Code}}" lang="{{.Code}}" hreflang="{{.Code}}" aria-label="{{.Name}} ({{.Label}})"{{if .Current}} aria-current="page"{{end}}>{{.Label}}</a>{{end}}</nav>
</div>
</header>
<main id="main">
<div class="mctl-site-container">
<div class="mctl-site-hero mctl-site-hero--stacked">
<div class="mctl-site-hero__text">
<h1 class="mctl-site-hero__title">{{.T.Title}}</h1>
<p class="mctl-site-hero__lede">{{.T.Lead}}</p>
<span class="mctl-site-chip">{{.T.Preview}}</span>
</div>
</div>
<section class="mctl-site-section">
<div class="mctl-site-section__narrow">
<h2 class="mctl-site-section__title">{{.T.ConnectH}}</h2>
<div class="card">
<p>{{.T.EndpointL}}: <code class="endpoint">{{.Endpoint}}</code></p>
<p>{{.T.ClaudeAI}}</p>
<p>{{.T.ClaudeCode}}</p>
<pre tabindex="0"><code>claude mcp add --transport http glovo {{.Endpoint}}</code></pre>
<p>{{.T.ClaudeCodeThen}}</p>
<p>{{.T.Other}}</p>
</div>
</div>
</section>
<section class="mctl-site-section"><div class="mctl-site-section__narrow"><h2 class="mctl-site-section__title">{{.T.CanH}}</h2><div class="card"><ul>{{range .T.Can}}<li>{{.}}</li>{{end}}</ul></div></div></section>
<section class="mctl-site-section"><div class="mctl-site-section__narrow"><h2 class="mctl-site-section__title">{{.T.NeverH}}</h2><div class="card"><ul>{{range .T.Never}}<li>{{.}}</li>{{end}}</ul></div></div></section>
<section class="mctl-site-section"><div class="mctl-site-section__narrow"><h2 class="mctl-site-section__title">{{.T.SafeH}}</h2><div class="card"><ul>{{range .T.Safe}}<li>{{.}}</li>{{end}}</ul></div></div></section>
</div>
</main>
<footer class="mctl-site-footer">
<div class="mctl-site-container mctl-site-footer__inner">
<nav class="mctl-site-footer__links" aria-label="{{.T.FooterNavL}}">
<a href="https://mctl.ai" rel="noopener">MCTL</a>
<a href="` + landingPrivacyURL + `" rel="noopener">{{.T.PrivacyL}}</a>
<a href="mailto:` + landingContactEmail + `">{{.T.ContactL}}</a>
</nav>
<p class="mctl-site-footer__copy">{{.T.Footer}}</p>
</div>
</footer>
</body>
</html>
`))

var landingCSP = func() string {
	sum := sha256.Sum256([]byte(landingCSS))
	return "default-src 'none'; style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) +
		"'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
}()

type landingLang struct {
	Code, Label, Name, Alt string
	Current                bool
}

// Page-level constants. The theme colours are --mctl-surface-{dark,light}-bg
// of the pinned design version (TestLandingThemeColorsMatchTokens keeps them
// in step); Privacy is the platform's page (trailing slash: the bare path
// redirects), Contact is the platform's public support address.
const (
	landingThemeDark    = "#0a0b0d"
	landingThemeLight   = "#f1ede4"
	landingPrivacyURL   = "https://mctl.ai/privacy/"
	landingContactEmail = "support@mctl.ai"
)

// landingLangNames are the languages' own names, for the accessible name of
// each language link.
var landingLangNames = map[string]string{"en": "English", "es": "Español", "ru": "Русский"}

// landingOrigin is the page's bare URL derived from the MCP endpoint
// ("https://host/mcp" -> "https://host/").
func landingOrigin(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "/"
	}
	return u.Scheme + "://" + u.Host + "/"
}

// pickLang chooses the page language: an explicit ?lang, then the cookie
// a previous ?lang set, then Accept-Language, else English. It reports
// whether the choice came from ?lang, so the caller can remember it.
func pickLang(r *http.Request) (lang string, explicit bool) {
	if q := strings.ToLower(r.URL.Query().Get("lang")); isLandingLang(q) {
		return q, true
	}
	if c, err := r.Cookie(langCookie); err == nil && isLandingLang(c.Value) {
		return c.Value, false
	}
	// Tags are taken in header order; a tag with q=0 is refused, other
	// weights are not ranked (browsers already send them in order).
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		tag, params, _ := strings.Cut(part, ";")
		if q := strings.ReplaceAll(strings.TrimSpace(params), " ", ""); q == "q=0" || q == "q=0.0" || q == "q=0.00" || q == "q=0.000" {
			continue
		}
		base, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(tag)), "-")
		if isLandingLang(base) {
			return base, false
		}
	}
	return "en", false
}

func isLandingLang(s string) bool {
	for _, l := range landingLangs {
		if s == l {
			return true
		}
	}
	return false
}

// RenderLanding renders the page for lang (one of LandingLangs) with the
// given MCP endpoint. The Cloudflare Worker in landing-worker/ serves the
// same bytes, generated by cmd/landing-gen.
func RenderLanding(lang, endpoint string) ([]byte, error) {
	if !isLandingLang(lang) {
		lang = "en"
	}
	origin := landingOrigin(endpoint)
	langs := make([]landingLang, 0, len(landingLangs))
	for _, l := range landingLangs {
		langs = append(langs, landingLang{
			Code: l, Label: strings.ToUpper(l), Name: landingLangNames[l],
			Alt: origin + "?lang=" + l, Current: l == lang,
		})
	}
	var buf bytes.Buffer
	err := landingTmpl.Execute(&buf, struct {
		T         landingText
		CSS       template.CSS
		Endpoint  string
		Canonical string
		Langs     []landingLang
	}{landingTexts[lang], template.CSS(landingCSS), endpoint, origin, langs}) //nolint:gosec // G203: landingCSS is a compile-time constant, never input
	return buf.Bytes(), err
}

// LandingCSP is the page's Content-Security-Policy.
func LandingCSP() string { return landingCSP }

// LandingLangs lists the page languages; the first is the default.
func LandingLangs() []string { return append([]string(nil), landingLangs...) }

// LandingLangCookie remembers an explicit ?lang choice.
const LandingLangCookie = langCookie

func (g *Gateway) handleLanding(w http.ResponseWriter, r *http.Request) {
	lang, explicit := pickLang(r)
	page, err := RenderLanding(lang, g.cfg.Resource)
	if err != nil {
		g.log.Warn("landing render failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if explicit {
		http.SetCookie(w, &http.Cookie{
			Name: langCookie, Value: lang, Path: "/", MaxAge: 365 * 24 * 3600,
			HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
		})
	}
	h := w.Header()
	h.Set("Content-Security-Policy", landingCSP)
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Language", lang)
	h.Set("Cache-Control", "private, max-age=300")
	h.Set("Vary", "Accept-Language, Cookie")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(page)
}
