import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import { fileURLToPath } from "node:url";

const MAIN = fileURLToPath(new URL("../src/main.mjs", import.meta.url));

function run(env, stdin) {
  const child = spawn(process.execPath, [MAIN], { env: { PATH: process.env.PATH, ...env }, stdio: ["pipe", "pipe", "pipe"] });
  let out = "";
  let err = "";
  child.stdout.on("data", (d) => (out += d));
  child.stderr.on("data", (d) => (err += d));
  if (stdin != null) child.stdin.end(stdin);
  const exit = new Promise((r) => child.on("exit", (code) => r(code)));
  return { child, exit, out: () => out, err: () => err };
}

const OK_ENV = {
  GLOVO_REMOTE_LOGIN_SESSION_OUT: "/nonexistent-dir/session.json",
  GLOVO_REMOTE_LOGIN_ORIGIN: "https://glovo.example.test",
};

test("bad configuration exits 64 before reading the token", async () => {
  assert.equal(await run({}, "").exit, 64);
  assert.equal(await run({ ...OK_ENV, GLOVO_REMOTE_LOGIN_SESSION_OUT: "relative.json" }, "").exit, 64);
  assert.equal(await run({ ...OK_ENV, GLOVO_REMOTE_LOGIN_ORIGIN: "https://x/path" }, "").exit, 64);
  assert.equal(await run({ ...OK_ENV, GLOVO_REMOTE_LOGIN_PROVIDERS: "apple" }, "").exit, 64);
  assert.equal(await run({ ...OK_ENV, GLOVO_REMOTE_LOGIN_TIMEOUT_MS: "1" }, "").exit, 64);
  assert.equal(await run({ ...OK_ENV, GLOVO_REMOTE_LOGIN_HEADLESS: "0" }, "").exit, 64);
  assert.equal(
    await run({ ...OK_ENV, GLOVO_REMOTE_LOGIN_EXECUTABLE: "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" }, "").exit,
    64,
  );
  assert.equal(await run({ ...OK_ENV, GLOVO_REMOTE_LOGIN_EXECUTABLE: "chrome" }, "").exit, 64);
});

test("GLOVO_SESSION_PATH, glovo-home's name, is accepted as the session path", async () => {
  const env = { GLOVO_SESSION_PATH: OK_ENV.GLOVO_REMOTE_LOGIN_SESSION_OUT, GLOVO_REMOTE_LOGIN_ORIGIN: OK_ENV.GLOVO_REMOTE_LOGIN_ORIGIN };
  const r = run(env, "short\n");
  assert.equal(await r.exit, 64);
  assert.match(r.err(), /helper token/, "got past the path check to the token check");
});

test("a short or missing helper token exits 64", async () => {
  assert.equal(await run(OK_ENV, "short\n").exit, 64);
  assert.equal(await run(OK_ENV, "").exit, 64);
});

test("listens on loopback, then a signal ends the run as an error with no session", async () => {
  const token = randomBytes(32).toString("hex");
  const r = run(OK_ENV, `${token}\n`);
  const deadline = Date.now() + 10_000;
  while (!r.out().includes("port") && Date.now() < deadline) await new Promise((s) => setTimeout(s, 50));
  const { port } = JSON.parse(r.out().split("\n")[0]);
  assert.ok(port > 0);
  const res = await fetch(`http://127.0.0.1:${port}/viewer`);
  assert.equal(res.status, 200);
  r.child.kill("SIGTERM");
  assert.equal(await r.exit, 3);
  assert.match(r.out(), /"result":"error"/);
  assert.ok(!r.err().includes(token) && !r.out().includes(token), "the helper token is never echoed");
});
