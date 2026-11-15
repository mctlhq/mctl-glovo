import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import {
  buildSession,
  isAuthResponse,
  isProfileResponse,
  locationFromHeaders,
  mergeLocation,
  normalizeCredentials,
} from "../src/capture.mjs";

const fixture = (name) => JSON.parse(readFileSync(new URL(`./fixtures/${name}`, import.meta.url), "utf8"));
const API = "https://api.glovoapp.com";

test("auth responses are recognised only on the API origin and exact paths", () => {
  assert.equal(isAuthResponse(`${API}/oauth/token`, API), true);
  assert.equal(isAuthResponse(`${API}/oauth/refresh`, API), true);
  assert.equal(isAuthResponse(`${API}/oauth/2fa/verify`, API), true);
  assert.equal(isAuthResponse(`${API}/oauth/token?x=1`, API), true);
  assert.equal(isAuthResponse("https://evil.example/oauth/token", API), false);
  assert.equal(isAuthResponse(`https://glovoapp.com/oauth/token`, API), false);
  assert.equal(isAuthResponse(`${API}/x/oauth/token`, API), false);
  assert.equal(isAuthResponse(`${API}/oauth/tokenx`, API), false);
  assert.equal(isProfileResponse(`${API}/v3/me`, API), true);
  assert.equal(isProfileResponse(`${API}/v3/me/addresses`, API), false);
});

test("flat token response is normalised", () => {
  const c = normalizeCredentials(fixture("oauth-token.json"));
  assert.equal(c.refreshToken, "fixture-refresh-token-not-real");
  assert.equal(c.tokenType, "bearer");
  assert.equal(typeof c.createdAt, "number");
});

test("2FA response shape {access:{...}} is normalised", () => {
  const c = normalizeCredentials(fixture("oauth-2fa.json"));
  assert.equal(c.refreshToken, "fixture-refresh-token-2fa-not-real");
});

test("responses without both tokens are not credentials", () => {
  assert.equal(normalizeCredentials(null), null);
  assert.equal(normalizeCredentials({}), null);
  assert.equal(normalizeCredentials({ accessToken: "a" }), null);
  assert.equal(normalizeCredentials({ accessToken: "", refreshToken: "r" }), null);
  assert.equal(normalizeCredentials({ accessToken: 1, refreshToken: "r" }), null);
  assert.equal(normalizeCredentials({ error: { message: "bad password" } }), null);
});

test("location headers are read case-insensitively and empty values dropped", () => {
  const loc = locationFromHeaders({
    "Glovo-Location-City-Code": "BCN",
    "glovo-device-urn": "glv:device:fixture",
    "glovo-language-code": "",
    "content-type": "application/json",
  });
  assert.deepEqual(loc, { cityCode: "BCN", deviceUrn: "glv:device:fixture" });
  assert.equal(locationFromHeaders({ accept: "*/*" }), null);
  assert.deepEqual(mergeLocation(loc, { countryCode: "ES" }), { ...loc, countryCode: "ES" });
});

test("buildSession produces the shape glovo-skill loads", () => {
  const credentials = normalizeCredentials(fixture("oauth-token.json"));
  const s = buildSession({ credentials, customer: fixture("me.json"), location: { deviceUrn: "glv:device:fixture" } });
  assert.equal(s.exp, 4102444800);
  assert.equal(s.customerId, 42);
  assert.equal(s.deviceUrn, "glv:device:fixture");
  assert.equal(s.refreshToken, credentials.refreshToken);
  const s2 = buildSession({ credentials: normalizeCredentials(fixture("oauth-2fa.json")), customer: null, location: null });
  assert.equal(s2.customerId, "77");
  assert.equal(s2.customer, null);
});
