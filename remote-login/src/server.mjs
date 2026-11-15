// Local-only control server for one login run.
//
// It listens on 127.0.0.1 and is meant to be reached only through glovo-home,
// which redeems the user's single-use link, checks the session cookie and
// the Origin, and then proxies the WebSocket here adding the per-run helper
// token in the X-Remote-Login-Token header. The browser never sees that
// token. The helper checks the token and the Origin again, so a local
// process that finds the port still cannot drive the browser.
//
// Wire protocol (viewer <-> helper):
//   viewer -> helper, text frames, JSON:
//     {t:"hello", viewport:{width,height,dpr,mobile}}   first message, once
//     {t:"mouse", type, x, y, button?, clickCount?, deltaX?, deltaY?}
//     {t:"touch", type, points:[{x,y,id}]}
//     {t:"key", type:"keyDown"|"keyUp", key}             named keys only
//     {t:"text", text}                                   typed text
//   helper -> viewer:
//     binary frames: one JPEG per screencast frame
//     {t:"meta", w, h}                                   viewport in CSS px
//     {t:"status", state:"starting"|"ready"|"done"|"timeout"|"error"}

import { createServer } from "node:http";
import { createHash, timingSafeEqual } from "node:crypto";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { WebSocketServer } from "ws";

const MAX_MESSAGE_BYTES = 4096;
const MAX_TEXT_CHARS = 256;
const MAX_MESSAGES_PER_SECOND = 300;
const HELLO_TIMEOUT_MS = 5_000;
const MAX_BAD_TOKENS = 3;

// Keys that may be sent as key events. Printable characters arrive as
// {t:"text"} and go through Input.insertText instead.
export const NAMED_KEYS = Object.freeze({
  Enter: { code: "Enter", keyCode: 13 },
  Backspace: { code: "Backspace", keyCode: 8 },
  Tab: { code: "Tab", keyCode: 9 },
  Escape: { code: "Escape", keyCode: 27 },
  Delete: { code: "Delete", keyCode: 46 },
  ArrowLeft: { code: "ArrowLeft", keyCode: 37 },
  ArrowRight: { code: "ArrowRight", keyCode: 39 },
  ArrowUp: { code: "ArrowUp", keyCode: 38 },
  ArrowDown: { code: "ArrowDown", keyCode: 40 },
});

const MOUSE_TYPES = new Set(["mousePressed", "mouseReleased", "mouseMoved", "mouseWheel"]);
const TOUCH_TYPES = new Set(["touchStart", "touchMove", "touchEnd", "touchCancel"]);
const BUTTONS = new Set(["left", "middle", "right"]);

export const SECURITY_HEADERS = Object.freeze({
  "Content-Security-Policy":
    "default-src 'none'; script-src 'self'; style-src 'self'; img-src blob:; connect-src 'self'; " +
    "frame-ancestors 'none'; base-uri 'none'; form-action 'none'",
  "X-Frame-Options": "DENY",
  "Referrer-Policy": "no-referrer",
  "Cache-Control": "no-store",
  "X-Content-Type-Options": "nosniff",
  "Cross-Origin-Opener-Policy": "same-origin",
  "Cross-Origin-Resource-Policy": "same-origin",
  "Permissions-Policy": "camera=(), microphone=(), geolocation=(), clipboard-read=()",
});

function tokenEquals(a, b) {
  if (typeof a !== "string" || typeof b !== "string") return false;
  const ha = createHash("sha256").update(a).digest();
  const hb = createHash("sha256").update(b).digest();
  return timingSafeEqual(ha, hb);
}

const finite = (x) => typeof x === "number" && Number.isFinite(x);

// validateInput returns a sanitized command or null. Anything unexpected is
// rejected, not coerced. `vp` is the current viewport in CSS pixels.
export function validateInput(msg, vp) {
  if (!msg || typeof msg !== "object") return null;
  const inside = (x, y) => finite(x) && finite(y) && x >= 0 && y >= 0 && x <= vp.width && y <= vp.height;
  switch (msg.t) {
    case "mouse": {
      if (!MOUSE_TYPES.has(msg.type) || !inside(msg.x, msg.y)) return null;
      const button = msg.button ?? "left";
      if (!BUTTONS.has(button)) return null;
      const clickCount = msg.clickCount ?? 1;
      if (!Number.isInteger(clickCount) || clickCount < 0 || clickCount > 3) return null;
      const dx = msg.deltaX ?? 0;
      const dy = msg.deltaY ?? 0;
      if (!finite(dx) || !finite(dy) || Math.abs(dx) > 5000 || Math.abs(dy) > 5000) return null;
      return { t: "mouse", type: msg.type, x: msg.x, y: msg.y, button, clickCount, deltaX: dx, deltaY: dy };
    }
    case "touch": {
      if (!TOUCH_TYPES.has(msg.type) || !Array.isArray(msg.points) || msg.points.length > 5) return null;
      if (msg.type !== "touchEnd" && msg.type !== "touchCancel" && msg.points.length === 0) return null;
      const points = [];
      for (const p of msg.points) {
        if (!p || !inside(p.x, p.y) || !Number.isInteger(p.id) || p.id < 0 || p.id > 31) return null;
        points.push({ x: p.x, y: p.y, id: p.id });
      }
      return { t: "touch", type: msg.type, points };
    }
    case "key": {
      if (msg.type !== "keyDown" && msg.type !== "keyUp") return null;
      const k = Object.hasOwn(NAMED_KEYS, msg.key) ? NAMED_KEYS[msg.key] : null;
      if (!k) return null;
      return { t: "key", type: msg.type === "keyDown" ? "rawKeyDown" : "keyUp", key: msg.key, ...k };
    }
    case "text": {
      if (typeof msg.text !== "string" || msg.text.length === 0 || msg.text.length > MAX_TEXT_CHARS) return null;
      // No control characters; Enter/Tab are named keys.
      if (/[\u0000-\u001f\u007f]/.test(msg.text)) return null;
      return { t: "text", text: msg.text };
    }
    default:
      return null;
  }
}

const VIEWER_DIR = fileURLToPath(new URL("../viewer/", import.meta.url));
const STATIC = {
  "/viewer": { file: "index.html", type: "text/html; charset=utf-8" },
  "/viewer/viewer.js": { file: "viewer.js", type: "text/javascript; charset=utf-8" },
  "/viewer/viewer.css": { file: "viewer.css", type: "text/css; charset=utf-8" },
};

// startServer wires one LoginBrowser to at most one viewer connection at a
// time. `createBrowser(viewport, sinks)` must return a started LoginBrowser.
// It resolves to { port, done } where `done` resolves with the run outcome.
export function startServer({ helperToken, allowedOrigin, createBrowser, audit, port = 0 }) {
  if (typeof helperToken !== "string" || helperToken.length < 32) throw new Error("helper token too short");
  if (!allowedOrigin) throw new Error("allowedOrigin is required");

  let resolveDone;
  const done = new Promise((r) => {
    resolveDone = r;
  });
  let browser = null;
  let starting = false;
  let current = null; // the one live viewer socket
  let badTokens = 0;
  let finished = false;
  let vp = null;
  // The newest frame, kept in memory only, so a viewer that reconnects sees
  // the page at once even if nothing repaints. Dropped when the run ends.
  let lastFrame = null;

  const http = createServer((req, res) => {
    const entry = req.method === "GET" ? STATIC[new URL(req.url, "http://x").pathname] : null;
    if (!entry) {
      res.writeHead(404, SECURITY_HEADERS).end();
      return;
    }
    res.writeHead(200, { ...SECURITY_HEADERS, "Content-Type": entry.type });
    res.end(readFileSync(VIEWER_DIR + entry.file));
  });

  const wss = new WebSocketServer({ noServer: true, maxPayload: MAX_MESSAGE_BYTES, perMessageDeflate: false });

  const reject = (socket, code, reason) => {
    audit("ws_rejected", { reason, code });
    socket.write(`HTTP/1.1 ${code} ${reason}\r\nConnection: close\r\nContent-Length: 0\r\n\r\n`);
    socket.destroy();
  };

  http.on("upgrade", (req, socket, head) => {
    if (finished) return reject(socket, 410, "Gone");
    if (new URL(req.url, "http://x").pathname !== "/ws") return reject(socket, 404, "Not Found");
    if (req.headers.origin !== allowedOrigin) return reject(socket, 403, "Forbidden");
    if (!tokenEquals(req.headers["x-remote-login-token"], helperToken)) {
      badTokens += 1;
      if (badTokens >= MAX_BAD_TOKENS) finish({ status: "error", reason: "bad_token_limit" });
      return reject(socket, 401, "Unauthorized");
    }
    if (current) return reject(socket, 409, "Conflict");
    wss.handleUpgrade(req, socket, head, (ws) => onConnection(ws));
  });

  function send(ws, obj) {
    if (ws && ws.readyState === ws.OPEN) ws.send(JSON.stringify(obj));
  }

  function onConnection(ws) {
    current = ws;
    audit("viewer_connected");
    let helloSeen = false;
    let windowStart = Date.now();
    let windowCount = 0;
    const helloTimer = setTimeout(() => {
      if (!helloSeen) ws.close(4408, "hello timeout");
    }, HELLO_TIMEOUT_MS);

    ws.on("message", async (data, isBinary) => {
      const now = Date.now();
      if (now - windowStart >= 1000) {
        windowStart = now;
        windowCount = 0;
      }
      if (++windowCount > MAX_MESSAGES_PER_SECOND) return ws.close(4429, "rate");
      if (isBinary) return ws.close(4400, "binary not accepted");
      let msg;
      try {
        msg = JSON.parse(data.toString("utf8"));
      } catch {
        return ws.close(4400, "bad json");
      }
      if (!helloSeen) {
        if (msg?.t !== "hello") return ws.close(4400, "hello expected");
        helloSeen = true;
        clearTimeout(helloTimer);
        await onHello(ws, msg.viewport);
        return;
      }
      if (!browser || !vp) return; // still starting; drop input
      const cmd = validateInput(msg, vp);
      if (!cmd) return ws.close(4400, "bad input");
      try {
        if (cmd.t === "mouse") await browser.inputMouse(cmd);
        else if (cmd.t === "touch") await browser.inputTouch(cmd);
        else if (cmd.t === "key") await browser.inputKey(cmd);
        else if (cmd.t === "text") await browser.inputText(cmd.text);
      } catch {
        // The page may be navigating; a lost input event is not fatal.
      }
    });
    ws.on("close", () => {
      clearTimeout(helloTimer);
      if (current === ws) current = null;
      audit("viewer_disconnected");
    });
  }

  async function onHello(ws, viewport) {
    if (browser || starting) {
      // A reconnect: the browser keeps its state; just resume the stream.
      send(ws, { t: "status", state: browser ? "ready" : "starting" });
      if (vp) send(ws, { t: "meta", w: vp.width, h: vp.height });
      if (lastFrame && ws.readyState === ws.OPEN) ws.send(lastFrame);
      return;
    }
    starting = true;
    send(ws, { t: "status", state: "starting" });
    try {
      browser = await createBrowser(viewport, {
        onFrame: (jpeg) => {
          lastFrame = jpeg;
          if (current && current.readyState === current.OPEN && current.bufferedAmount < 2_000_000) current.send(jpeg);
        },
        onMeta: (m) => {
          const changed = !vp || vp.width !== m.w || vp.height !== m.h;
          vp = { width: m.w, height: m.h };
          if (changed) send(current, { t: "meta", w: m.w, h: m.h });
        },
      });
    } catch {
      finish({ status: "error", reason: "browser_start" });
      return;
    }
    vp = vp ?? { width: browser.viewport.width, height: browser.viewport.height };
    send(current, { t: "status", state: "ready" });
    browser.result.then((outcome) => finish(outcome));
  }

  function finish(outcome) {
    if (finished) return;
    finished = true;
    lastFrame = null;
    const state = outcome.status === "captured" ? "done" : outcome.status;
    send(current, { t: "status", state });
    if (current) current.close(1000, state);
    if (browser) browser.close().catch(() => {});
    setTimeout(() => {
      wss.close();
      http.close();
    }, 100).unref();
    resolveDone(outcome);
  }

  return new Promise((resolve, reject) => {
    http.once("error", reject);
    http.listen(port, "127.0.0.1", () => {
      resolve({ port: http.address().port, done, finish });
    });
  });
}
