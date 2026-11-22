import { test } from "node:test";
import assert from "node:assert/strict";
import worker, { CIMD_DOCUMENT, CIMD_PATH } from "../src/index.js";
import { CSP, PAGES } from "../src/pages.gen.js";

const get = (path, headers = {}, method = "GET") =>
  worker.fetch(new Request("https://glovo.mctl.ai" + path, { method, headers }));

test("language selection matches the origin", async () => {
  const cases = [
    ["/", {}, "en"],
    ["/", { "Accept-Language": "es-ES,es;q=0.9,en;q=0.8" }, "es"],
    ["/", { "Accept-Language": "de-DE, ru;q=0.7" }, "ru"],
    ["/", { "Accept-Language": "ru;q=0, es" }, "es"],
    ["/?lang=ru", { "Accept-Language": "es" }, "ru"],
    ["/", { "Accept-Language": "es", Cookie: "a=b; glovo_lang=ru" }, "ru"],
    ["/?lang=xx", { "Accept-Language": "es" }, "es"],
    ["/", { Cookie: "glovo_lang=<x>" }, "en"],
  ];
  for (const [path, hdr, want] of cases) {
    const r = await get(path, hdr);
    assert.equal(r.status, 200, path);
    assert.equal(r.headers.get("Content-Language"), want, `${path} ${JSON.stringify(hdr)}`);
    assert.equal(await r.text(), PAGES[want]);
  }
});

test("cookie only on an explicit ?lang", async () => {
  const c = (await get("/?lang=es")).headers.get("Set-Cookie");
  for (const want of ["glovo_lang=es", "HttpOnly", "Secure", "SameSite=Lax"]) assert.ok(c.includes(want), c);
  assert.equal((await get("/", { "Accept-Language": "es" })).headers.get("Set-Cookie"), null);
});

test("strict headers", async () => {
  const r = await get("/");
  assert.equal(r.headers.get("Content-Security-Policy"), CSP);
  assert.ok(CSP.startsWith("default-src 'none';") && !CSP.includes("unsafe"));
  assert.equal(r.headers.get("X-Frame-Options"), "DENY");
  assert.ok(!(await r.text()).includes("<script"));
});

test("routing", async () => {
  assert.equal((await get("/mcp")).status, 404);
  assert.equal((await get("/", {}, "POST")).status, 405);
  const h = await get("/", {}, "HEAD");
  assert.equal(h.status, 200);
  assert.equal(await h.text(), "");
});

// The CSP admits one style by hash. Recompute it from the served HTML, so a
// header/body mismatch fails even though both come from pages.gen.js.
test("CSP hash matches the served <style>", async () => {
  const { createHash } = await import("node:crypto");
  for (const lang of ["en", "es", "ru"]) {
    const r = await get("/?lang=" + lang);
    const body = await r.text();
    assert.equal(body.split("<style>").length, 2, `${lang}: exactly one <style>`);
    const style = body.slice(body.indexOf("<style>") + 7, body.indexOf("</style>"));
    const hash = createHash("sha256").update(style).digest("base64");
    assert.ok(
      r.headers.get("Content-Security-Policy").includes(`style-src 'sha256-${hash}'`),
      `${lang}: CSP does not pin the served style`,
    );
  }
});

test("head metadata for an invitation-only preview", async () => {
  for (const lang of ["en", "es", "ru"]) {
    const body = await (await get("/?lang=" + lang)).text();
    for (const want of [
      '<meta name="robots" content="noindex">',
      `<link rel="canonical" href="https://glovo.mctl.ai/?lang=${lang}">`,
      '<link rel="alternate" hreflang="x-default" href="https://glovo.mctl.ai/">',
      '<link rel="alternate" hreflang="ru" href="https://glovo.mctl.ai/?lang=ru">',
      '<meta property="og:type" content="website">',
      'class="mctl-site-header',
      'class="mctl-site-footer"',
      'href="https://mctl.ai/privacy/"',
    ]) assert.ok(body.includes(want), `${lang}: ${want}`);
    assert.ok(body.includes(`hreflang="${lang}" aria-label=`) && body.includes(" aria-current=\"page\""));
    assert.ok(!body.includes("og:image") && !body.includes('rel="icon"'));
  }
});

// The glovo-link helper's client_id is this document's own URL. mctl-api
// refuses a document whose client_id differs from the URL it was fetched
// from, one with a secret-bearing auth method, or one with no usable
// redirect URI, so each of those is pinned here.
test("CIMD document for glovo-link", async () => {
  const r = await get(CIMD_PATH);
  assert.equal(r.status, 200);
  assert.equal(r.headers.get("Content-Type"), "application/json");
  assert.match(r.headers.get("Cache-Control"), /max-age=3600/);
  const d = await r.json();
  assert.equal(d.client_id, "https://glovo.mctl.ai" + CIMD_PATH);
  assert.equal(d.token_endpoint_auth_method, "none");
  assert.deepEqual(d.redirect_uris, ["http://127.0.0.1/callback"]);
  assert.deepEqual(d, CIMD_DOCUMENT);
  for (const k of ["client_secret", "jwks", "jwks_uri"]) assert.ok(!(k in d), k);
  const h = await get(CIMD_PATH, {}, "HEAD");
  assert.equal(h.status, 200);
  assert.equal(await h.text(), "");
  assert.equal((await get(CIMD_PATH, {}, "POST")).status, 405);
  assert.equal((await get(CIMD_PATH + "/x")).status, 404);
});
