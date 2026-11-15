#!/usr/bin/env node
// Entry point for one remote login run, started by glovo-home.
//
// Configuration (environment):
//   GLOVO_REMOTE_LOGIN_SESSION_OUT   absolute path the session is written to (required)
//   GLOVO_REMOTE_LOGIN_ORIGIN        the only Origin the WebSocket accepts (required)
//   GLOVO_REMOTE_LOGIN_PORT          127.0.0.1 port, default 0 (any free port)
//   GLOVO_REMOTE_LOGIN_TIMEOUT_MS    hard timeout for the whole run, default 600000
//   GLOVO_REMOTE_LOGIN_PROVIDERS     comma list of social providers: google,facebook (default none)
//   GLOVO_REMOTE_LOGIN_HEADLESS      "0" for a visible window, default headless
//   GLOVO_BROWSER_CHANNEL            playwright channel, default "chrome"
//
// The helper token arrives as the first line on stdin, never in the
// environment or argv, where other processes could read it.
//
// stdout: exactly one JSON line {"port":N} once listening, and one JSON
// line {"result":"captured"|"timeout"|"error"} at the end. stderr: audit
// lines (src/audit.mjs).
//
// Exit codes: 0 captured and written, 2 timeout, 3 error, 64 bad config.

import { createInterface } from "node:readline";
import { isAbsolute } from "node:path";
import { buildAllowlist } from "./allowlist.mjs";
import { makeAudit } from "./audit.mjs";
import { LoginBrowser } from "./browser.mjs";
import { startServer } from "./server.mjs";
import { writeSessionAtomic } from "./session-write.mjs";

function fail(msg) {
  process.stderr.write(`${JSON.stringify({ ts: new Date().toISOString(), event: "config_error", reason: msg })}\n`);
  process.exit(64);
}

async function readTokenLine() {
  const rl = createInterface({ input: process.stdin });
  for await (const line of rl) {
    rl.close();
    return line.trim();
  }
  return "";
}

async function main() {
  const env = process.env;
  const out = env.GLOVO_REMOTE_LOGIN_SESSION_OUT;
  if (!out || !isAbsolute(out)) fail("GLOVO_REMOTE_LOGIN_SESSION_OUT must be an absolute path");
  const origin = env.GLOVO_REMOTE_LOGIN_ORIGIN;
  if (!origin || !/^https?:\/\/[^/]+$/.test(origin)) fail("GLOVO_REMOTE_LOGIN_ORIGIN must be an origin");
  const timeoutMs = Number(env.GLOVO_REMOTE_LOGIN_TIMEOUT_MS || 600_000);
  if (!Number.isFinite(timeoutMs) || timeoutMs < 10_000 || timeoutMs > 900_000) fail("timeout out of range");
  const providers = (env.GLOVO_REMOTE_LOGIN_PROVIDERS || "").split(",").map((s) => s.trim()).filter(Boolean);
  let allowlist;
  try {
    allowlist = buildAllowlist({ providers });
  } catch (e) {
    fail(e.message);
  }

  const helperToken = await readTokenLine();
  if (helperToken.length < 32) fail("helper token missing or too short");

  const audit = makeAudit();
  for (const p of providers) audit("provider_enabled", { provider: p });

  const server = await startServer({
    helperToken,
    allowedOrigin: origin,
    audit,
    port: Number(env.GLOVO_REMOTE_LOGIN_PORT || 0),
    createBrowser: async (viewport, sinks) => {
      const b = new LoginBrowser({
        allowlist,
        audit,
        viewport,
        timeoutMs,
        channel: env.GLOVO_BROWSER_CHANNEL || "chrome",
        headless: env.GLOVO_REMOTE_LOGIN_HEADLESS !== "0",
        ...sinks,
      });
      try {
        return await b.start();
      } catch (e) {
        await b.close();
        throw e;
      }
    },
  });
  process.stdout.write(`${JSON.stringify({ port: server.port })}\n`);

  // The run's hard deadline also covers "nobody ever connected".
  const hard = setTimeout(() => server.finish({ status: "timeout" }), timeoutMs);
  hard.unref();
  for (const sig of ["SIGTERM", "SIGINT"]) process.on(sig, () => server.finish({ status: "error", reason: "signal" }));

  const outcome = await server.done;
  clearTimeout(hard);
  if (outcome.status === "captured") {
    try {
      writeSessionAtomic(out, outcome.session);
      audit("session_written");
    } catch (e) {
      audit("session_write_failed", { code: e.code || "error" });
      process.stdout.write(`${JSON.stringify({ result: "error" })}\n`);
      process.exitCode = 3;
      return;
    }
    process.stdout.write(`${JSON.stringify({ result: "captured" })}\n`);
    process.exitCode = 0;
    return;
  }
  process.stdout.write(`${JSON.stringify({ result: outcome.status })}\n`);
  process.exitCode = outcome.status === "timeout" ? 2 : 3;
}

main().catch(() => {
  process.stderr.write(`${JSON.stringify({ ts: new Date().toISOString(), event: "fatal" })}\n`);
  process.exit(3);
});
