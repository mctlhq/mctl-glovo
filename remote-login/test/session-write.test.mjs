import { test } from "node:test";
import assert from "node:assert/strict";
import { chmodSync, mkdtempSync, readdirSync, readFileSync, statSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { writeReloadMarker, writeSessionAtomic } from "../src/session-write.mjs";

function privateDir() {
  const d = mkdtempSync(join(process.env.TMPDIR || tmpdir(), "rl-session-"));
  chmodSync(d, 0o700);
  return d;
}

test("session file is 0600 even under a permissive umask, and complete", () => {
  const dir = privateDir();
  const target = join(dir, "session.json");
  const old = process.umask(0o000);
  try {
    writeSessionAtomic(target, { accessToken: "a", refreshToken: "r" });
  } finally {
    process.umask(old);
  }
  assert.equal(statSync(target).mode & 0o777, 0o600);
  const body = JSON.parse(readFileSync(target, "utf8"));
  assert.equal(body.refreshToken, "r");
  assert.ok(body.updatedAt);
  assert.deepEqual(readdirSync(dir), ["session.json"], "no temp files left behind");
});

test("replacing an existing wider-mode file still ends at 0600", () => {
  const dir = privateDir();
  const target = join(dir, "session.json");
  writeFileSync(target, "{}", { mode: 0o644 });
  writeSessionAtomic(target, { accessToken: "a", refreshToken: "r2" });
  assert.equal(statSync(target).mode & 0o777, 0o600);
  assert.equal(JSON.parse(readFileSync(target, "utf8")).refreshToken, "r2");
});

test("refuses relative paths, symlink targets and shared directories", () => {
  assert.throws(() => writeSessionAtomic("session.json", {}), /absolute/);
  const dir = privateDir();
  const real = join(dir, "real.json");
  writeFileSync(real, "{}", { mode: 0o600 });
  const link = join(dir, "session.json");
  symlinkSync(real, link);
  assert.throws(() => writeSessionAtomic(link, {}), /symlink/);

  const shared = privateDir();
  chmodSync(shared, 0o777);
  assert.throws(() => writeSessionAtomic(join(shared, "session.json"), {}), /writable/);
  assert.deepEqual(readdirSync(shared), []);
});

test("refuses a missing directory instead of creating it", () => {
  const dir = privateDir();
  assert.throws(() => writeSessionAtomic(join(dir, "nope", "session.json"), {}), { code: "ENOENT" });
});

test("reload marker is written next to the session, 0600, timestamp only", () => {
  const dir = privateDir();
  const target = join(dir, "session.json");
  writeSessionAtomic(target, { accessToken: "a", refreshToken: "r" });
  writeReloadMarker(target);
  const marker = join(dir, "session.json.reload");
  assert.equal(statSync(marker).mode & 0o777, 0o600);
  assert.match(readFileSync(marker, "utf8"), /^\d{4}-\d{2}-\d{2}T[\d:.]+Z\n$/);
  assert.deepEqual(readdirSync(dir).sort(), ["session.json", "session.json.reload"]);
});
