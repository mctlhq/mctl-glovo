import { closeSync, fsyncSync, lstatSync, mkdirSync, openSync, renameSync, rmSync, writeSync } from "node:fs";
import { randomBytes } from "node:crypto";
import { dirname, isAbsolute } from "node:path";

// writeSessionAtomic writes the session to `target` so that a reader sees
// either the old file or the complete new one, never a partial file, and the
// file is 0600 from its first byte (created with O_EXCL and mode 0600, so it
// never exists with wider permissions, whatever the umask).
//
// The directory must already exist and must not be group- or
// world-writable: creating it here would hide a misconfigured path.
export function writeSessionAtomic(target, session) {
  if (!target || !isAbsolute(target)) throw new Error("session path must be absolute");
  const dir = dirname(target);
  const st = lstatSync(dir);
  if (!st.isDirectory()) throw new Error("session directory is not a directory");
  if (st.mode & 0o022) throw new Error("session directory is group- or world-writable");
  try {
    if (lstatSync(target).isSymbolicLink()) throw new Error("session path is a symlink");
  } catch (e) {
    if (e.code !== "ENOENT") throw e;
  }

  const body = JSON.stringify({ ...session, updatedAt: new Date().toISOString() }, null, 2);
  const tmp = `${target}.${randomBytes(6).toString("hex")}.tmp`;
  const fd = openSync(tmp, "wx", 0o600);
  try {
    writeSync(fd, body);
    fsyncSync(fd);
  } catch (e) {
    closeSync(fd);
    rmSync(tmp, { force: true });
    throw e;
  }
  closeSync(fd);
  try {
    renameSync(tmp, target);
  } catch (e) {
    rmSync(tmp, { force: true });
    throw e;
  }
  // Make the rename durable before reporting success.
  const dfd = openSync(dir, "r");
  try {
    fsyncSync(dfd);
  } finally {
    closeSync(dfd);
  }
}

export function ensurePrivateDir(dir) {
  mkdirSync(dir, { recursive: true, mode: 0o700 });
}
