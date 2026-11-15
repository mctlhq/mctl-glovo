import { test } from "node:test";
import assert from "node:assert/strict";
import worker from "../src/index.js";
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
