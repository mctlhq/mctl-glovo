// End-to-end tests against a real Chrome and local fake pages. No Glovo
// account and no network beyond 127.0.0.1 are involved. Set
// REMOTE_LOGIN_REQUIRE_BROWSER=1 (CI does) to turn "Chrome not available"
// into a failure instead of a skip.

import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { readFileSync } from "node:fs";
import { customAllowlist } from "../src/allowlist.mjs";
import { LoginBrowser, pidAlive } from "../src/browser.mjs";

const fixture = (name) => readFileSync(new URL(`./fixtures/${name}`, import.meta.url), "utf8");
const CHANNEL = process.env.GLOVO_BROWSER_CHANNEL || "chrome";

async function chromeAvailable() {
  const { chromium } = await import("playwright-core");
  try {
    const b = await chromium.launch({ channel: CHANNEL, headless: true });
    await b.close();
    return true;
  } catch {
    return false;
  }
}
const haveChrome = await chromeAvailable();
if (!haveChrome && process.env.REMOTE_LOGIN_REQUIRE_BROWSER === "1") {
  throw new Error(`Chrome channel "${CHANNEL}" is required but could not be launched`);
}
const skip = haveChrome ? false : `Chrome channel "${CHANNEL}" not available`;

function listen(handler) {
  return new Promise((resolve) => {
    const s = createServer(handler);
    s.listen(0, "127.0.0.1", () => resolve({ server: s, origin: `http://127.0.0.1:${s.address().port}`, port: s.address().port }));
  });
}

async function fakeWorld() {
  const offHits = [];
  const off = await listen((req, res) => {
    offHits.push(req.url);
    res.writeHead(200, { "Content-Type": "text/html" }).end("<h1>off-list</h1>");
  });
  const api = await listen((req, res) => {
    const cors = {
      "Access-Control-Allow-Origin": "*",
      "Access-Control-Allow-Headers": "content-type, glovo-device-urn, glovo-location-city-code",
      "Content-Type": "application/json",
    };
    if (req.method === "OPTIONS") return res.writeHead(204, cors).end();
    if (req.url.startsWith("/oauth/token")) return res.writeHead(200, cors).end(fixture("oauth-token.json"));
    if (req.url === "/v3/me") return res.writeHead(200, cors).end(fixture("me.json"));
    res.writeHead(404, cors).end("{}");
  });
  const site = await listen((req, res) => {
    const u = new URL(req.url, "http://x");
    let body = "<title>fake login</title><p>fake</p>";
    if (u.pathname === "/escape") {
      // Every way a page can try to leave the list.
      body += `<img src="${off.origin}/img.png"><iframe src="${off.origin}/frame"></iframe>
        <script>
          fetch("${off.origin}/fetch").catch(() => {});
          try { new WebSocket("${off.origin.replace("http", "ws")}/ws"); } catch {}
          window.open("${off.origin}/popup");
          setTimeout(() => { location.href = "${off.origin}/top"; }, 200);
        </script>`;
    } else if (u.pathname === "/login") {
      body += `<script>
        setTimeout(async () => {
          const h = { "content-type": "application/json", "glovo-device-urn": "glv:device:fixture", "glovo-location-city-code": "BCN" };
          await fetch("${api.origin}/oauth/token", { method: "POST", headers: h, body: "{}" });
          await fetch("${api.origin}/v3/me", { headers: h });
        }, 300);
      </script>`;
    }
    res.writeHead(200, { "Content-Type": "text/html" }).end(body);
  });
  const rule = (o) => ({ scheme: "http:", host: "127.0.0.1", port: o.port });
  const allowlist = customAllowlist({ top: [rule(site)], frame: [rule(site)], sub: [rule(site), rule(api)] });
  const close = () => Promise.all([off, api, site].map((s) => new Promise((r) => s.server.close(r))));
  return { off, api, site, offHits, allowlist, close };
}

function auditSink() {
  const events = [];
  return { events, audit: (event, f = {}) => events.push({ event, ...f }) };
}

test("lockdown blocks every off-list request and navigation", { skip, timeout: 60_000 }, async () => {
  const w = await fakeWorld();
  const { events, audit } = auditSink();
  const b = new LoginBrowser({
    allowlist: w.allowlist,
    audit,
    startUrl: `${w.site.origin}/escape`,
    apiOrigin: w.api.origin,
    channel: CHANNEL,
    timeoutMs: 30_000,
  });
  try {
    await b.start();
    await new Promise((r) => setTimeout(r, 2000));
    assert.deepEqual(w.offHits, [], "the off-list server received no request at all");
    const blocked = events.filter((e) => e.event === "blocked" && e.host === `127.0.0.1:${w.off.port}`);
    const kinds = new Set(blocked.map((e) => e.kind));
    for (const k of ["top", "frame", "sub", "ws"]) assert.ok(kinds.has(k), `blocked a ${k} request`);
    for (const e of events) assert.ok(!JSON.stringify(e).includes("/popup"), "audit carries hosts, never paths");
    const pages = b.context.pages().filter((p) => !p.isClosed());
    for (const p of pages) {
      const url = p.url();
      assert.ok(url.startsWith(w.site.origin) || url.startsWith("chrome-error://") || url === "about:blank", url);
    }
  } finally {
    await b.close();
    await w.close();
  }
});

test("capture: a fixture token response finishes the run with a session", { skip, timeout: 60_000 }, async () => {
  const w = await fakeWorld();
  const { events, audit } = auditSink();
  const b = new LoginBrowser({
    allowlist: w.allowlist,
    audit,
    startUrl: `${w.site.origin}/login`,
    apiOrigin: w.api.origin,
    channel: CHANNEL,
    viewport: { width: 390, height: 844, dpr: 2, mobile: true },
    timeoutMs: 30_000,
    profileSettleMs: 5_000,
  });
  try {
    await b.start();
    const out = await b.result;
    assert.equal(out.status, "captured");
    assert.equal(out.session.refreshToken, "fixture-refresh-token-not-real");
    assert.equal(out.session.customerId, 42);
    assert.equal(out.session.deviceUrn, "glv:device:fixture");
    assert.equal(out.session.location.cityCode, "BCN");
    const text = JSON.stringify(events);
    assert.ok(!text.includes("fixture-refresh"), "audit never contains token material");
  } finally {
    await b.close();
    await w.close();
  }
});

test("screencast streams JPEG frames with viewport metadata", { skip, timeout: 60_000 }, async () => {
  const w = await fakeWorld();
  const { audit } = auditSink();
  const frames = [];
  const metas = [];
  const b = new LoginBrowser({
    allowlist: w.allowlist,
    audit,
    onFrame: (f) => frames.push(f),
    onMeta: (m) => metas.push(m),
    startUrl: `${w.site.origin}/`,
    apiOrigin: w.api.origin,
    channel: CHANNEL,
    viewport: { width: 390, height: 844, dpr: 2, mobile: true },
    timeoutMs: 30_000,
  });
  try {
    await b.start();
    const deadline = Date.now() + 10_000;
    while (!frames.length && Date.now() < deadline) await new Promise((r) => setTimeout(r, 100));
    assert.ok(frames.length > 0, "at least one frame arrived");
    assert.equal(frames[0][0], 0xff);
    assert.equal(frames[0][1], 0xd8, "frame is a JPEG");
    assert.equal(metas[0].w, 390);
    assert.equal(metas[0].h, 844);
  } finally {
    await b.close();
    await w.close();
  }
});

test("timeout ends the run and kills the browser process", { skip, timeout: 60_000 }, async () => {
  const w = await fakeWorld();
  const { audit } = auditSink();
  const b = new LoginBrowser({
    allowlist: w.allowlist,
    audit,
    startUrl: `${w.site.origin}/`,
    apiOrigin: w.api.origin,
    channel: CHANNEL,
    timeoutMs: 1500,
  });
  try {
    await b.start();
    const pid = b.browserPid;
    assert.ok(Number.isInteger(pid) && pid > 0, "browser pid is known");
    assert.equal(pidAlive(pid), true);
    const out = await b.result;
    assert.equal(out.status, "timeout");
    const deadline = Date.now() + 10_000;
    while (pidAlive(pid) && Date.now() < deadline) await new Promise((r) => setTimeout(r, 100));
    assert.equal(pidAlive(pid), false, "browser process is gone after timeout");
  } finally {
    await b.close();
    await w.close();
  }
});
