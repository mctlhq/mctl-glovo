// Edge copy of glovo-home's landing page (internal/gateway/landing.go), plus
// the OAuth Client ID Metadata Document of the glovo-link helper.
// mctl-gitops routes glovo.mctl.ai/* here; /mcp*, /link* and /.well-known/*
// have no-script routes and keep going to the origin.
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

// CIMD_PATH is the client_id of the glovo-link helper: an https URL whose
// document names the client (OAuth Client ID Metadata Documents, as accepted
// by mctl-api when this host is in OAUTH_CIMD_ALLOWED_HOSTS). The document
// must carry client_id equal to its own URL. The helper listens on a random
// loopback port, so the redirect is the port-less loopback form (RFC 8252
// section 7.3); mctl-api matches any port on an exact scheme, host and path.
// It is a public client (no secret): token_endpoint_auth_method is none.
export const CIMD_PATH = "/glovo-link/client.json";
export const CIMD_DOCUMENT = {
  client_id: "https://glovo.mctl.ai" + CIMD_PATH,
  client_name: "glovo-link",
  redirect_uris: ["http://127.0.0.1/callback"],
  grant_types: ["authorization_code"],
  response_types: ["code"],
  token_endpoint_auth_method: "none",
};

export default {
  async fetch(request) {
    const url = new URL(request.url);
    if (url.pathname === CIMD_PATH) {
      if (request.method !== "GET" && request.method !== "HEAD") {
        return new Response("method not allowed\n", {
          status: 405,
          headers: { ...BASE_HEADERS, Allow: "GET, HEAD" },
        });
      }
      // A short max-age: mctl-api honours it, so a changed redirect URI
      // reaches the authorization server within the hour.
      const headers = new Headers({
        ...BASE_HEADERS,
        "Content-Type": "application/json",
        "Cache-Control": "public, max-age=3600",
      });
      const body = JSON.stringify(CIMD_DOCUMENT) + "\n";
      return new Response(request.method === "HEAD" ? null : body, { status: 200, headers });
    }
    if (url.pathname !== "/") {
      // Anything else that reaches the Worker is a routing mistake.
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
