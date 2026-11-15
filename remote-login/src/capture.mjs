// Turning Glovo's own auth responses into a session file.
//
// Ported from glovo-skill src/auth/login.mjs, with one tightening: a
// response counts only if it comes from the configured API origin, not from
// any URL that merely contains "/oauth/token".

const AUTH_PATH = /^\/oauth\/(token|refresh|2fa\/[^?#]*)$/;
const ME_PATH = /^\/v3\/me$/;

export function isAuthResponse(url, apiOrigin) {
  return matchPath(url, apiOrigin, AUTH_PATH);
}

export function isProfileResponse(url, apiOrigin) {
  return matchPath(url, apiOrigin, ME_PATH);
}

function matchPath(url, apiOrigin, re) {
  let u;
  try {
    u = new URL(url);
  } catch {
    return false;
  }
  return u.origin === apiOrigin && re.test(u.pathname);
}

// normalizeCredentials accepts both shapes Glovo returns ({access:{...}} on
// 2FA, flat on token/refresh). It returns null unless an access token and a
// refresh token are both strings: a session without a refresh token would
// stop working within hours, and saving it would look like success.
export function normalizeCredentials(body) {
  const c = body?.access?.accessToken ? body.access : body;
  if (typeof c?.accessToken !== "string" || c.accessToken === "") return null;
  if (typeof c?.refreshToken !== "string" || c.refreshToken === "") return null;
  return {
    accessToken: c.accessToken,
    refreshToken: c.refreshToken,
    expiresIn: c.expiresIn,
    scope: c.scope ?? null,
    tokenType: c.tokenType ?? "bearer",
    createdAt: Date.now(),
  };
}

const HEADER_MAP = {
  "glovo-location-country-code": "countryCode",
  "glovo-location-city-code": "cityCode",
  "glovo-delivery-location-latitude": "latitude",
  "glovo-delivery-location-longitude": "longitude",
  "glovo-delivery-location-accuracy": "accuracy",
  "glovo-language-code": "languageCode",
  "glovo-app-version": "appVersion",
  "glovo-client-info": "clientInfo",
  "glovo-device-urn": "deviceUrn",
  "glovo-perseus-client-id": "perseusClientId",
  "glovo-perseus-session-id": "perseusSessionId",
  "glovo-perseus-session-timestamp": "perseusSessionTimestamp",
  "glovo-perseus-consent": "perseusConsent",
};

// locationFromHeaders reads the device and location headers the web app
// sends to the API. glovo-skill replays them, so the session keeps them.
export function locationFromHeaders(headers) {
  const out = {};
  for (const [key, value] of Object.entries(headers || {})) {
    const field = HEADER_MAP[key.toLowerCase()];
    if (field && value != null && value !== "") out[field] = value;
  }
  return Object.keys(out).length ? out : null;
}

export function mergeLocation(current, next) {
  if (!next) return current;
  return { ...(current || {}), ...next };
}

export function decodeJwtClaims(token) {
  const parts = String(token || "").replace(/^Bearer\s+/i, "").split(".");
  if (parts.length < 2) return null;
  try {
    return JSON.parse(Buffer.from(parts[1], "base64url").toString("utf8"));
  } catch {
    return null;
  }
}

// buildSession produces the object glovo-skill's loadSession expects.
export function buildSession({ credentials, customer, location }) {
  const claims = decodeJwtClaims(credentials.accessToken);
  return {
    ...credentials,
    exp: claims?.exp,
    customerId: customer?.id ?? claims?.customerId ?? claims?.customer_id ?? claims?.sub ?? null,
    customer: customer ? { id: customer.id, name: customer.name, email: customer.email } : null,
    deviceUrn: location?.deviceUrn,
    perseusClientId: location?.perseusClientId,
    perseusSessionId: location?.perseusSessionId,
    perseusSessionTimestamp: location?.perseusSessionTimestamp,
    location: location ?? null,
    linkedVia: "remote-login",
  };
}
