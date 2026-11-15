// Navigation and request lockdown for the remote login browser.
//
// Every request the page makes is classified as one of:
//   - "top":   a main-frame navigation (what the user sees);
//   - "frame": a subframe navigation (reCAPTCHA renders in iframes);
//   - "sub":   any other request (scripts, XHR, images, WebSockets).
// Each class has its own list. Anything not listed is blocked. Only https
// is allowed unless a rule names another scheme explicitly (tests use
// http://127.0.0.1).
//
// A rule is { scheme, host, port?, suffix?, pathPrefix? }:
//   - host + suffix:false  -> exact host match;
//   - host + suffix:true   -> host itself or any subdomain of it;
//   - pathPrefix           -> the URL path must start with it.

const CORE = {
  top: [
    { host: "glovoapp.com" },
    { host: "www.glovoapp.com" },
  ],
  frame: [
    { host: "www.google.com", pathPrefix: "/recaptcha/" },
    { host: "www.recaptcha.net", pathPrefix: "/recaptcha/" },
  ],
  sub: [
    { host: "glovoapp.com", suffix: true }, // web app, api.glovoapp.com, cards.glovoapp.com
    { host: "glovo.dhmedia.io" }, // images
    { host: "www.google.com", pathPrefix: "/recaptcha/" },
    { host: "www.gstatic.com", pathPrefix: "/recaptcha/" },
    { host: "www.recaptcha.net", pathPrefix: "/recaptcha/" },
    { host: "incognia.com", suffix: true }, // device-risk SDK the login page loads
    { host: "usercentrics.eu", suffix: true }, // consent banner that can cover the form
  ],
};

// Social sign-in is off by default (see docs/design/remote-glovo-login.md):
// Google refuses sign-in from automation-controlled browsers, and every
// enabled provider widens the surface a leaked link exposes.
const PROVIDERS = {
  google: {
    top: [{ host: "accounts.google.com" }],
    frame: [{ host: "accounts.google.com" }],
    sub: [
      { host: "accounts.google.com" },
      { host: "ssl.gstatic.com" },
      { host: "www.gstatic.com" },
      { host: "fonts.gstatic.com" },
      { host: "lh3.googleusercontent.com" },
    ],
  },
  facebook: {
    top: [{ host: "www.facebook.com" }, { host: "m.facebook.com" }, { host: "web.facebook.com" }],
    frame: [{ host: "www.facebook.com" }],
    sub: [
      { host: "facebook.com", suffix: true },
      { host: "connect.facebook.net" },
      { host: "fbcdn.net", suffix: true },
    ],
  },
};

export const KNOWN_PROVIDERS = Object.freeze(Object.keys(PROVIDERS));

function normRule(rule) {
  return {
    scheme: rule.scheme ?? "https:",
    host: String(rule.host).toLowerCase(),
    port: rule.port == null ? "" : String(rule.port),
    suffix: Boolean(rule.suffix),
    pathPrefix: rule.pathPrefix ?? "/",
  };
}

// buildAllowlist({ providers }) returns the production lists plus the
// requested social providers. An unknown provider name is an error, not a
// silent no-op.
export function buildAllowlist({ providers = [] } = {}) {
  const lists = { top: [...CORE.top], frame: [...CORE.frame], sub: [...CORE.sub] };
  for (const name of providers) {
    const p = PROVIDERS[name];
    if (!p) throw new Error(`unknown identity provider: ${name}`);
    for (const k of ["top", "frame", "sub"]) lists[k].push(...p[k]);
  }
  return customAllowlist(lists);
}

// customAllowlist is used by tests to point the lockdown at local servers.
export function customAllowlist({ top = [], frame = [], sub = [] }) {
  return Object.freeze({
    top: Object.freeze(top.map(normRule)),
    frame: Object.freeze(frame.map(normRule)),
    sub: Object.freeze(sub.map(normRule)),
  });
}

function matches(rule, u) {
  if (u.protocol !== rule.scheme) return false;
  if (u.username || u.password) return false;
  const host = u.hostname.toLowerCase();
  const hostOk = rule.suffix ? host === rule.host || host.endsWith(`.${rule.host}`) : host === rule.host;
  if (!hostOk) return false;
  if (u.port !== rule.port) return false;
  return u.pathname.startsWith(rule.pathPrefix);
}

// WebSocket URLs are judged as their https/http equivalent, so a rule for
// https://x also covers wss://x and nothing else does.
function parse(url) {
  let u;
  try {
    u = new URL(url);
  } catch {
    return null;
  }
  if (u.protocol === "wss:") u = new URL(url.replace(/^wss:/, "https:"));
  else if (u.protocol === "ws:") u = new URL(url.replace(/^ws:/, "http:"));
  return u;
}

// classifyAllowed returns true only when the URL matches a rule of the
// given kind. Unparseable URLs and every other scheme (file:, data:,
// chrome:, javascript:, blob:) are refused.
export function isAllowed(allowlist, kind, url) {
  const rules = allowlist[kind];
  if (!rules) throw new Error(`unknown request kind: ${kind}`);
  const u = parse(url);
  if (!u) return false;
  return rules.some((r) => matches(r, u));
}

// hostOf returns only the host of a URL, for audit lines. Paths and query
// strings can carry emails, codes or tokens and are never logged.
export function hostOf(url) {
  try {
    return new URL(url).host || "(none)";
  } catch {
    return "(unparseable)";
  }
}
