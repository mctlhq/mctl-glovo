import { test } from "node:test";
import assert from "node:assert/strict";
import { buildAllowlist, customAllowlist, hostOf, isAllowed } from "../src/allowlist.mjs";

const prod = buildAllowlist();

test("top-level navigation: only glovoapp.com hosts", () => {
  assert.equal(isAllowed(prod, "top", "https://glovoapp.com/en/login"), true);
  assert.equal(isAllowed(prod, "top", "https://www.glovoapp.com/"), true);
  for (const url of [
    "https://evil.example/",
    "https://api.glovoapp.com/oauth/token", // API host is not a page
    "https://evilglovoapp.com/",
    "https://glovoapp.com.evil.example/",
    "http://glovoapp.com/", // no plain http
    "https://glovoapp.com:8443/",
    "https://user:pw@glovoapp.com/",
    "https://accounts.google.com/", // social providers are off by default
    "https://www.facebook.com/",
    "https://oauth.kaspi.kz/",
    "file:///etc/passwd",
    "data:text/html,hi",
    "javascript:alert(1)",
    "chrome://settings",
    "blob:https://glovoapp.com/x",
    "not a url",
  ]) {
    assert.equal(isAllowed(prod, "top", url), false, url);
  }
});

test("subresources: suffix rules do not match look-alike hosts", () => {
  assert.equal(isAllowed(prod, "sub", "https://api.glovoapp.com/v3/me"), true);
  assert.equal(isAllowed(prod, "sub", "https://glovo.dhmedia.io/image/x.png"), true);
  assert.equal(isAllowed(prod, "sub", "https://xglovoapp.com/a.js"), false);
  assert.equal(isAllowed(prod, "sub", "https://www.googletagmanager.com/gtm.js"), false);
  assert.equal(isAllowed(prod, "sub", "https://app.adjust.com/x"), false);
});

test("path-scoped rules: reCAPTCHA yes, the rest of www.google.com no", () => {
  assert.equal(isAllowed(prod, "frame", "https://www.google.com/recaptcha/api2/anchor?k=x"), true);
  assert.equal(isAllowed(prod, "sub", "https://www.gstatic.com/recaptcha/releases/x/recaptcha__en.js"), true);
  assert.equal(isAllowed(prod, "frame", "https://www.google.com/search?q=x"), false);
  assert.equal(isAllowed(prod, "top", "https://www.google.com/recaptcha/api2/anchor"), false);
  assert.equal(isAllowed(prod, "sub", "https://www.gstatic.com/other/x.js"), false);
});

test("websocket URLs are judged against the https rules", () => {
  assert.equal(isAllowed(prod, "sub", "wss://api.glovoapp.com/ws"), true);
  assert.equal(isAllowed(prod, "sub", "ws://api.glovoapp.com/ws"), false);
  assert.equal(isAllowed(prod, "sub", "wss://evil.example/ws"), false);
});

test("providers are opt-in and unknown names are an error", () => {
  const g = buildAllowlist({ providers: ["google"] });
  assert.equal(isAllowed(g, "top", "https://accounts.google.com/o/oauth2/v2/auth"), true);
  assert.equal(isAllowed(g, "top", "https://www.facebook.com/"), false);
  assert.throws(() => buildAllowlist({ providers: ["apple"] }), /unknown identity provider/);
});

test("custom lists match scheme and port exactly", () => {
  const l = customAllowlist({ top: [{ scheme: "http:", host: "127.0.0.1", port: 4000 }] });
  assert.equal(isAllowed(l, "top", "http://127.0.0.1:4000/x"), true);
  assert.equal(isAllowed(l, "top", "http://127.0.0.1:4001/x"), false);
  assert.equal(isAllowed(l, "top", "http://localhost:4000/x"), false);
  assert.throws(() => isAllowed(l, "nope", "http://127.0.0.1:4000/"));
});

test("hostOf never returns a path or query", () => {
  assert.equal(hostOf("https://glovoapp.com/en/login?email=a@b.c#t"), "glovoapp.com");
  assert.equal(hostOf("::"), "(unparseable)");
});
