// Edge copy of glovo-home's landing page (internal/gateway/landing.go).
// Routed by mctl-gitops to the root path of glovo.mctl.ai only; /mcp and
// /.well-known/* never reach this Worker and keep going to the origin.
// Pages, CSP and cookie name come from pages.gen.js (cmd/landing-gen), so
// the bytes match what the origin would serve.
import { COOKIE, CSP, LANGS, PAGES } from "./pages.gen.js";

const isLang = (s) => LANGS.includes(s);

// pickLang mirrors pickLang in landing.go: ?lang, then the cookie, then
// Accept-Language in header order (q=0 refused), else the first language.
export function pickLang(request) {
  const url = new URL(request.url);
  const q = (url.searchParams.get("lang") || "").toLowerCase();
  if (isLang(q)) return { lang: q, explicit: true };
  for (const part of (request.headers.get("Cookie") || "").split(";")) {
    const [k, ...v] = part.trim().split("=");
    if (k === COOKIE && isLang(v.join("="))) return { lang: v.join("="), explicit: false };
  }
  for (const part of (request.headers.get("Accept-Language") || "").split(",")) {
    const [tag, params = ""] = part.split(";");
    if (/^q=0(\.0{0,3})?$/.test(params.replace(/\s/g, ""))) continue;
    const base = tag.trim().toLowerCase().split("-")[0];
    if (isLang(base)) return { lang: base, explicit: false };
  }
  return { lang: LANGS[0], explicit: false };
}

const BASE_HEADERS = {
  "X-Content-Type-Options": "nosniff",
  "X-Frame-Options": "DENY",
  "Referrer-Policy": "no-referrer",
  "Permissions-Policy": "geolocation=(), microphone=(), camera=()",
  "X-Permitted-Cross-Domain-Policies": "none",
  "Strict-Transport-Security": "max-age=31536000; includeSubDomains",
};

export default {
  async fetch(request) {
    const url = new URL(request.url);
    if (url.pathname !== "/") {
      // The route admits only "/"; anything else is a routing mistake.
      return new Response("not found\n", { status: 404, headers: BASE_HEADERS });
    }
    if (request.method !== "GET" && request.method !== "HEAD") {
      return new Response("method not allowed\n", {
        status: 405,
        headers: { ...BASE_HEADERS, Allow: "GET, HEAD" },
      });
    }
    const { lang, explicit } = pickLang(request);
    const headers = new Headers({
      ...BASE_HEADERS,
      "Content-Security-Policy": CSP,
      "Content-Type": "text/html; charset=utf-8",
      "Content-Language": lang,
      "Cache-Control": "private, max-age=300",
      Vary: "Accept-Language, Cookie",
    });
    if (explicit) {
      headers.append(
        "Set-Cookie",
        `${COOKIE}=${lang}; Path=/; Max-Age=31536000; HttpOnly; Secure; SameSite=Lax`,
      );
    }
    return new Response(request.method === "HEAD" ? null : PAGES[lang], { status: 200, headers });
  },
};
