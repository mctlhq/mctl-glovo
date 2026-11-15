// Golden generator for internal/glovotools: runs the upstream glovo-skill
// MCP server (github.com/denya/glovo-skill, MIT) once per case in
// ../cases.json, over stdio, with stub.mjs preloaded, and records the
// tools/list answer and each tools/call result as JSON under ../golden.
//
//   git clone https://github.com/denya/glovo-skill && cd glovo-skill
//   git checkout v0.2.1 && npm ci --ignore-scripts
//   GLOVO_SKILL_DIR=$PWD node <repo>/internal/glovotools/testdata/gen/gen.mjs [case ...]
//
// dist/server.mjs is the committed v0.2.1 bundle. Every input is synthetic.
import { mkdtempSync, readFileSync, rmSync, statSync, writeFileSync, existsSync, mkdirSync, chmodSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const testdata = path.dirname(here);
const skill = process.env.GLOVO_SKILL_DIR;
if (!skill) throw new Error("set GLOVO_SKILL_DIR to a glovo-skill v0.2.1 checkout");
const sdk = (p) => pathToFileURL(path.join(skill, "node_modules/@modelcontextprotocol/sdk/dist/esm", p)).href;
const { Client } = await import(sdk("client/index.js"));
const { StdioClientTransport } = await import(sdk("client/stdio.js"));

const spec = JSON.parse(readFileSync(path.join(testdata, "cases.json"), "utf8"));
const only = new Set(process.argv.slice(2));
const goldenDir = path.join(testdata, "golden");
mkdirSync(goldenDir, { recursive: true });

function readJSONLines(file) {
  if (!existsSync(file)) return [];
  return readFileSync(file, "utf8").split("\n").filter(Boolean).map((l) => JSON.parse(l));
}

function readMaybeJSON(file) {
  if (!existsSync(file)) return { exists: false };
  const text = readFileSync(file, "utf8");
  const mode = (statSync(file).mode & 0o777).toString(8);
  try {
    return { exists: true, mode, json: JSON.parse(text) };
  } catch {
    return { exists: true, mode, text };
  }
}

async function runCase(c, listTools) {
  const dir = mkdtempSync(path.join(os.tmpdir(), "glovo-gen-"));
  chmodSync(dir, 0o700);
  const sessionPath = path.join(dir, "session.json");
  const session = typeof c.session === "string" && c.session in spec.sessions ? spec.sessions[c.session] : c.session;
  if (c.session_text !== undefined) writeFileSync(sessionPath, c.session_text, { mode: 0o600 });
  else if (session) writeFileSync(sessionPath, JSON.stringify(session, null, 2), { mode: 0o600 });
  const routes = path.join(dir, "routes.json");
  writeFileSync(routes, JSON.stringify(c.routes || []));
  const log = path.join(dir, "requests.log");
  const transport = new StdioClientTransport({
    command: process.execPath,
    args: ["--import", pathToFileURL(path.join(here, "stub.mjs")).href, path.join(skill, "dist/server.mjs")],
    env: {
      PATH: process.env.PATH,
      HOME: dir,
      GLOVO_SESSION_PATH: sessionPath,
      GLOVO_GEN_ROUTES: routes,
      GLOVO_GEN_LOG: log,
      GLOVO_GEN_TESTDATA: testdata,
      GLOVO_GEN_NOW_MS: String(spec.now_ms),
      ...(c.env || {}),
    },
    stderr: "ignore",
  });
  const client = new Client({ name: "glovo-golden-gen", version: "1" });
  await client.connect(transport);
  let tools = null;
  if (listTools) tools = (await client.listTools()).tools;
  let result = null;
  if (c.tool) {
    result = await client.callTool({ name: c.tool, arguments: c.args || {} }, undefined, { timeout: 120_000 });
  }
  await client.close();
  const out = {
    case: c.name,
    tool: c.tool,
    result,
    requests: readJSONLines(log),
    session_after: readMaybeJSON(sessionPath),
    history_cache_after: readMaybeJSON(path.join(dir, "history-cache.json")),
  };
  rmSync(dir, { recursive: true, force: true });
  return { out, tools };
}

let listed = false;
for (const c of spec.cases) {
  if (only.size && !only.has(c.name)) continue;
  const { out, tools } = await runCase(c, !listed && !only.size);
  if (tools) {
    writeFileSync(path.join(goldenDir, "tools.json"), JSON.stringify(tools, null, 2) + "\n");
    listed = true;
  }
  writeFileSync(path.join(goldenDir, `${c.name}.json`), JSON.stringify(out, null, 2) + "\n");
  console.log(`${c.name}: ${out.result?.isError ? "isError" : "ok"} (${out.requests.length} requests)`);
}
