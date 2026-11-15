import { test } from "node:test";
import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import WebSocket from "ws";
import { SECURITY_HEADERS, startServer, validateInput } from "../src/server.mjs";

const ORIGIN = "https://glovo.example.test";
const VP = { width: 400, height: 800 };

function fakeBrowser() {
  let resolve;
  const calls = [];
  const b = {
    viewport: VP,
    calls,
    closed: false,
    result: new Promise((r) => {
      resolve = r;
    }),
    finish: (o) => resolve(o),
    inputMouse: async (c) => calls.push(["mouse", c.type]),
    inputTouch: async (c) => calls.push(["touch", c.type]),
    inputKey: async (c) => calls.push(["key", c.key]),
    inputText: async (t) => calls.push(["text", t.length]),
    close: async () => {
      b.closed = true;
    },
  };
  return b;
}

async function setup() {
  const token = randomBytes(32).toString("hex");
  const audits = [];
  const browsers = [];
  const srv = await startServer({
    helperToken: token,
    allowedOrigin: ORIGIN,
    audit: (event, f) => audits.push({ event, ...f }),
    createBrowser: async (_viewport, sinks) => {
      const b = fakeBrowser();
      b.sinks = sinks;
      browsers.push(b);
      return b;
    },
  });
  return { token, audits, browsers, srv, url: `ws://127.0.0.1:${srv.port}/ws` };
}

function connect(url, headers) {
  return new Promise((resolve) => {
    const ws = new WebSocket(url, { headers });
    ws.once("open", () => resolve({ ws, status: 101 }));
    ws.once("unexpected-response", (_req, res) => resolve({ ws, status: res.statusCode }));
    ws.once("error", () => {});
  });
}

function nextJson(ws, pred) {
  return new Promise((resolve) => {
    const on = (data, bin) => {
      if (bin) return;
      const m = JSON.parse(data.toString());
      if (pred(m)) {
        ws.off("message", on);
        resolve(m);
      }
    };
    ws.on("message", on);
  });
}

test("validateInput accepts well-formed input and rejects everything else", () => {
  assert.ok(validateInput({ t: "mouse", type: "mousePressed", x: 10, y: 10 }, VP));
  assert.ok(validateInput({ t: "touch", type: "touchStart", points: [{ x: 1, y: 2, id: 0 }] }, VP));
  assert.ok(validateInput({ t: "touch", type: "touchEnd", points: [] }, VP));
  assert.deepEqual(validateInput({ t: "key", type: "keyDown", key: "Enter" }, VP).type, "rawKeyDown");
  assert.ok(validateInput({ t: "text", text: "a@b.c" }, VP));
  for (const bad of [
    null,
    { t: "eval", code: "1" },
    { t: "mouse", type: "mousePressed", x: -1, y: 0 },
    { t: "mouse", type: "mousePressed", x: 401, y: 0 },
    { t: "mouse", type: "mousePressed", x: Number.NaN, y: 0 },
    { t: "mouse", type: "dragIntercepted", x: 1, y: 1 },
    { t: "mouse", type: "mousePressed", x: 1, y: 1, button: "back" },
    { t: "mouse", type: "mousePressed", x: 1, y: 1, clickCount: 9 },
    { t: "touch", type: "touchStart", points: [] },
    { t: "touch", type: "touchStart", points: new Array(6).fill({ x: 1, y: 1, id: 0 }) },
    { t: "key", type: "keyDown", key: "F12" },
    { t: "key", type: "keyDown", key: "toString" },
    { t: "key", type: "char", key: "Enter" },
    { t: "text", text: "" },
    { t: "text", text: "x".repeat(257) },
    { t: "text", text: "a\nb" },
  ]) {
    assert.equal(validateInput(bad, VP), null, JSON.stringify(bad));
  }
});

test("upgrade is refused without the right Origin or helper token", async () => {
  const { token, srv, url, browsers } = await setup();
  const opened = [];
  const status = async (headers) => {
    const r = await connect(url, headers);
    opened.push(r.ws);
    return r.status;
  };
  try {
    assert.equal(await status({ Origin: "https://evil.example", "X-Remote-Login-Token": token }), 403);
    assert.equal(await status({ "X-Remote-Login-Token": token }), 403);
    assert.equal(await status({ Origin: ORIGIN, "X-Remote-Login-Token": "x".repeat(64) }), 401);
    assert.equal(await status({ Origin: ORIGIN }), 401);
    assert.equal(browsers.length, 0, "no browser was started");
  } finally {
    for (const ws of opened) ws.terminate();
    srv.finish({ status: "error" });
  }
});

test("too many bad tokens end the run", async () => {
  const { srv, url } = await setup();
  for (let i = 0; i < 3; i++) await connect(url, { Origin: ORIGIN, "X-Remote-Login-Token": `bad${i}`.padEnd(64, "x") });
  const outcome = await srv.done;
  assert.equal(outcome.reason, "bad_token_limit");
});

test("one viewer at a time; input flows; capture ends the run", async () => {
  const { token, srv, url, browsers } = await setup();
  const headers = { Origin: ORIGIN, "X-Remote-Login-Token": token };
  const a = await connect(url, headers);
  assert.equal(a.status, 101);
  const ready = nextJson(a.ws, (m) => m.t === "status" && m.state === "ready");
  a.ws.send(JSON.stringify({ t: "hello", viewport: { width: 400, height: 800, mobile: true } }));
  await ready;
  assert.equal((await connect(url, headers)).status, 409);

  const b = browsers[0];
  const frames = [];
  a.ws.on("message", (d, bin) => bin && frames.push(d.length));
  b.sinks.onMeta({ w: 400, h: 800 });
  b.sinks.onFrame(Buffer.from([0xff, 0xd8, 0xff, 0xd9]));
  a.ws.send(JSON.stringify({ t: "mouse", type: "mousePressed", x: 5, y: 5 }));
  a.ws.send(JSON.stringify({ t: "text", text: "secret-ish" }));
  await new Promise((r) => setTimeout(r, 100));
  assert.deepEqual(b.calls, [["mouse", "mousePressed"], ["text", 10]]);
  assert.deepEqual(frames, [4]);

  const done = nextJson(a.ws, (m) => m.t === "status" && m.state === "done");
  b.finish({ status: "captured", session: {} });
  await done;
  assert.equal((await srv.done).status, "captured");
  assert.equal(b.closed, true);
});

test("malformed input closes the viewer socket", async () => {
  const { token, srv, url } = await setup();
  const a = await connect(url, { Origin: ORIGIN, "X-Remote-Login-Token": token });
  const ready = nextJson(a.ws, (m) => m.state === "ready");
  a.ws.send(JSON.stringify({ t: "hello", viewport: {} }));
  await ready;
  const closed = new Promise((r) => a.ws.once("close", (code) => r(code)));
  a.ws.send(JSON.stringify({ t: "key", type: "keyDown", key: "F12" }));
  assert.equal(await closed, 4400);
  srv.finish({ status: "error" });
});

test("viewer page is served with a strict CSP and no framing", async () => {
  const { srv } = await setup();
  const res = await fetch(`http://127.0.0.1:${srv.port}/viewer`);
  assert.equal(res.status, 200);
  const csp = res.headers.get("content-security-policy");
  assert.match(csp, /frame-ancestors 'none'/);
  assert.match(csp, /default-src 'none'/);
  assert.doesNotMatch(csp, /unsafe-inline|unsafe-eval/);
  assert.equal(res.headers.get("x-frame-options"), "DENY");
  assert.equal(res.headers.get("cache-control"), "no-store");
  assert.equal((await fetch(`http://127.0.0.1:${srv.port}/../src/main.mjs`)).status, 404);
  assert.ok(SECURITY_HEADERS["Referrer-Policy"]);
  srv.finish({ status: "error" });
});
