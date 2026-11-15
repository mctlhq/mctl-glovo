// Records what Node's fetch (undici) puts on the wire on top of the headers
// glovo-skill passes it, as ../golden/undici_wire.json. It sends one
// request shaped like glovo-skill's token refresh to a listener on
// 127.0.0.1 and reads the raw request head; nothing leaves the machine.
//
//   node internal/glovotools/testdata/gen/wire.mjs
//
// Run it with the Node version glovo-home's child uses.
import net from "node:net";
import { writeFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const out = path.join(path.dirname(path.dirname(fileURLToPath(import.meta.url))), "golden", "undici_wire.json");
const given = [
  ["Accept", "application/json, text/plain, */*"],
  ["Content-Type", "application/json"],
  ["Glovo-Request-TTL", "7500"],
];
const body = JSON.stringify({ refreshToken: "refresh-test-old" });

const head = await new Promise((resolve, reject) => {
  const srv = net.createServer((sock) => {
    let buf = "";
    sock.on("data", (d) => {
      buf += d.toString("latin1");
      const end = buf.indexOf("\r\n\r\n");
      if (end < 0) return;
      sock.end("HTTP/1.1 200 OK\r\ncontent-type: application/json\r\ncontent-length: 2\r\nconnection: close\r\n\r\n{}");
      srv.close();
      resolve(buf.slice(0, end));
    });
  });
  srv.listen(0, "127.0.0.1", () => {
    fetch(`http://127.0.0.1:${srv.address().port}/oauth/refresh`, { method: "POST", headers: Object.fromEntries(given), body }).catch(reject);
  });
});

const [requestLine, ...lines] = head.split("\r\n");
const sent = lines.map((l) => {
  const i = l.indexOf(":");
  return [l.slice(0, i), l.slice(i + 1).trim()];
});
const givenNames = new Set(given.map(([k]) => k.toLowerCase()));
// Per-connection or per-body headers say nothing about the client.
const transport = new Set(["host", "connection", "content-length"]);
const added = sent.filter(([k]) => !givenNames.has(k.toLowerCase()) && !transport.has(k.toLowerCase()));
writeFileSync(out, JSON.stringify({ node: process.version, request_line: requestLine.replace(/^POST \S+ /, ""), given_kept_as_sent: given.every(([k, v]) => sent.some(([sk, sv]) => sk === k && sv === v)), added }, null, 1) + "\n");
console.log(out, added);
