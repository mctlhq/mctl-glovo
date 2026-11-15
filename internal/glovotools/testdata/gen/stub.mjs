// Preloaded into the upstream glovo-skill server by gen.mjs (node --import).
// It makes one tool call deterministic and offline:
//   - globalThis.fetch answers from the case's routes (GLOVO_GEN_ROUTES) and
//     appends every request (method, URI, headers, body) to GLOVO_GEN_LOG;
//     nothing reaches the network;
//   - Date.now() and new Date() return a fixed instant (GLOVO_GEN_NOW_MS);
//   - crypto.randomUUID returns a counter, like the Go tests' newID.
// All data is synthetic. Not part of the Go build.
import { appendFileSync, readFileSync } from "node:fs";
import crypto from "node:crypto";
import { syncBuiltinESMExports } from "node:module";
import path from "node:path";

const NOW = Number(process.env.GLOVO_GEN_NOW_MS);
const RealDate = Date;
class FixedDate extends RealDate {
  constructor(...args) {
    if (args.length === 0) super(NOW);
    else super(...args);
  }
  static now() {
    return NOW;
  }
}
globalThis.Date = FixedDate;

let ids = 0;
crypto.randomUUID = () => {
  ids += 1;
  return `00000000-0000-4000-8000-${String(ids).padStart(12, "0")}`;
};
syncBuiltinESMExports();

const API = "https://api.glovoapp.com";
const routesFile = process.env.GLOVO_GEN_ROUTES;
const logFile = process.env.GLOVO_GEN_LOG;
const testdata = process.env.GLOVO_GEN_TESTDATA;
const routes = JSON.parse(readFileSync(routesFile, "utf8"));
const served = new Map();

function replyBody(reply) {
  if (reply.file) return readFileSync(path.join(testdata, reply.file), "utf8");
  if ("json" in reply) return JSON.stringify(reply.json);
  return reply.text ?? "{}";
}

function response(status, text) {
  return {
    ok: status >= 200 && status <= 299,
    status,
    headers: new Map([["content-type", "application/json"]]),
    text: async () => text,
    json: async () => JSON.parse(text),
  };
}

globalThis.fetch = async (url, options = {}) => {
  const u = String(url);
  const method = options.method || "GET";
  let body = null;
  if (options.body !== undefined) {
    try {
      body = JSON.parse(options.body);
    } catch {
      body = String(options.body);
    }
  }
  if (!u.startsWith(API)) {
    appendFileSync(logFile, JSON.stringify({ method, uri: "OFF-ORIGIN", body: null }) + "\n");
    throw new Error("off-origin fetch refused by the generator");
  }
  const uri = u.slice(API.length);
  // Headers as glovo-skill passes them to fetch, in its order and spelling.
  // What undici adds on top is recorded once by wire.mjs.
  const headers = Object.entries(options.headers || {}).map(([k, v]) => [k, String(v)]);
  appendFileSync(logFile, JSON.stringify({ method, uri, headers, body }) + "\n");
  const p = uri.split("?")[0];
  const route = routes.find((r) => r.method === method && r.path === p);
  if (!route) return response(500, JSON.stringify({ error: { exceptionName: "unrouted" } }));
  const key = `${method} ${p}`;
  const n = served.get(key) || 0;
  served.set(key, n + 1);
  const reply = route.replies[Math.min(n, route.replies.length - 1)];
  if (reply.network) throw new TypeError("fetch failed");
  return response(reply.status ?? 200, replyBody(reply));
};
