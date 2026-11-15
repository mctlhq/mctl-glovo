// Audit lines: one JSON object per line on stderr. They carry an event name
// and a few enumerated or numeric fields, never URLs beyond the host, never
// frame data, keystrokes, form values, tokens, or hashes of tokens.

const ALLOWED_FIELDS = new Set(["event", "kind", "host", "reason", "code", "count", "ms", "viewport", "provider"]);

export function makeAudit(write = (line) => process.stderr.write(line)) {
  return function audit(event, fields = {}) {
    const rec = { ts: new Date().toISOString(), event };
    for (const [k, v] of Object.entries(fields)) {
      if (!ALLOWED_FIELDS.has(k)) continue;
      if (typeof v === "string") rec[k] = v.slice(0, 120);
      else if (typeof v === "number" || typeof v === "boolean") rec[k] = v;
    }
    write(`${JSON.stringify(rec)}\n`);
  };
}
