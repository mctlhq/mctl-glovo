# glovo-home on the Mac mini

`glovo-home` is an owner-only MCP gateway. It runs on the owner's Mac mini
next to the open-source local Glovo MCP server
[`glovo-skill`](https://github.com/denya/glovo-skill) (stdio, Node) and is exposed through a
Cloudflare tunnel as `https://glovo.mctl.ai/mcp`.

```
MCP client --HTTPS--> Cloudflare tunnel --> 127.0.0.1:8787 glovo-home --stdio--> node glovo-skill
```

What it does:
- serves RFC 9728 metadata at `/.well-known/oauth-protected-resource` and
  `/.well-known/oauth-protected-resource/mcp`, with the same body,
  headers and 401 challenge that mctl-api serves for this host today;
- verifies every `/mcp` request's bearer token: EdDSA signature against
  `https://api.mctl.ai/.well-known/jwks.json`, `iss` exactly
  `https://api.mctl.ai`, `aud` exactly `https://glovo.mctl.ai/mcp` (a single
  string, or a one-element array; anything else is refused), `exp`, `nbf`
  and `iat` with 30s leeway, and a `pid` claim on
  `GLOVO_HOME_ALLOWED_PRINCIPALS`;
- mirrors glovo-skill's tools and forwards calls unchanged. It adds no
  tools, and it refuses any tool whose name looks like checkout, payment or
  order placement;
- writes one audit line per tool call (principal, tool, outcome,
  duration). Arguments and results are never logged.

Basket writes go straight through, as they do with the local tool. There is
no approval step, and glovo-skill has no checkout or payment tool.

## Build

The mini is macOS 15 on x86_64:

```bash
GOTOOLCHAIN=go1.26.11 GOOS=darwin GOARCH=amd64 \
  go build -ldflags "-X main.version=$(git describe --tags --always)" \
  -o glovo-home ./cmd/glovo-home
```

Install it as `~/.local/bin/glovo-home`.

## Configuration

Configuration comes from the environment only. Issuer, resource and JWKS URL
are fixed in the binary and cannot be configured.

| Variable | Required | Meaning |
|---|---|---|
| `GLOVO_HOME_ALLOWED_PRINCIPALS` | yes | Comma-separated mctl principal ids (`prn_...`). Empty or unset refuses everyone. |
| `GLOVO_HOME_NODE` | yes | Node binary for the child. |
| `GLOVO_HOME_SERVER` | yes | glovo-skill entry point (`dist/server.mjs`). |
| `GLOVO_SESSION_PATH` | no | Passed to glovo-skill unchanged (its default is `~/.glovo/session.json`). |
| `GLOVO_HOME_LISTEN` | no | Listen address, default `127.0.0.1:8787`. A non-loopback address is refused unless `GLOVO_HOME_ALLOW_NON_LOOPBACK=true`. |
| `GLOVO_HOME_CALL_TIMEOUT` | no | Per-tool-call limit, default `3m`. |
| `GLOVO_HOME_BLOCKED_TOOLS` | no | Extra exact tool names to refuse, e.g. `glovo_login`. |
| `GLOVO_HOME_CHILD_STDERR` | no | `discard` drops glovo-skill's stderr; by default it goes to the log. |

The child gets an explicit environment: `HOME`, `PATH`, `USER`, `LOGNAME`,
`LANG`, `LC_ALL`, `TMPDIR`, `TZ`, every `GLOVO_*` variable except
`GLOVO_HOME_*`, and every `PLAYWRIGHT_*` variable. Nothing else is
passed.

`glovo_login` opens a browser on the mini itself. If remote callers should
not be able to trigger that, add it to `GLOVO_HOME_BLOCKED_TOOLS`.

## launchd

Save as `~/Library/LaunchAgents/com.mctl.glovo-home.plist`, replace every
`REPLACE_ME`, then run
`launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.mctl.glovo-home.plist`.
A LaunchAgent (not a LaunchDaemon) runs as the owner, so glovo-skill can
read the owner's session file and open a browser for `glovo_login`.

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.mctl.glovo-home</string>
  <key>ProgramArguments</key>
  <array>
    <string>/Users/REPLACE_ME/.local/bin/glovo-home</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>GLOVO_HOME_ALLOWED_PRINCIPALS</key>
    <string>REPLACE_ME</string>
    <key>GLOVO_HOME_NODE</key>
    <string>/usr/local/lib/mctl-slot-tools/node-22.23.4/bin/node</string>
    <key>GLOVO_HOME_SERVER</key>
    <string>/Users/REPLACE_ME/.local/share/glovo-skill/dist/server.mjs</string>
    <key>GLOVO_SESSION_PATH</key>
    <string>/Users/REPLACE_ME/.glovo/session.json</string>
    <key>GLOVO_HOME_LISTEN</key>
    <string>127.0.0.1:8787</string>
    <key>PATH</key>
    <string>/usr/local/lib/mctl-slot-tools/node-22.23.4/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ThrottleInterval</key>
  <integer>10</integer>
  <key>ExitTimeOut</key>
  <integer>30</integer>
  <key>StandardOutPath</key>
  <string>/Users/REPLACE_ME/Library/Logs/mctl-glovo-home.log</string>
  <key>StandardErrorPath</key>
  <string>/Users/REPLACE_ME/Library/Logs/mctl-glovo-home.log</string>
</dict>
</plist>
```

launchd does not expand `~` or `$HOME`, so the paths must be absolute.
`ExitTimeOut` gives the graceful shutdown (25s) time to finish before
launchd sends SIGKILL.

## Checks

```bash
curl -s http://127.0.0.1:8787/livez     # ok
curl -s http://127.0.0.1:8787/readyz    # ok once the child is up, its tools are mirrored and a JWKS key is cached
curl -si -X POST http://127.0.0.1:8787/mcp | grep -i www-authenticate
tail -f ~/Library/Logs/mctl-glovo-home.log | grep '"log":"audit"'
```

Behaviour on failure:
- JWKS unreachable and no usable cached key (none fetched yet, or the cache
  is more than 24h old): `/mcp` answers 503 and `/readyz` is not ready. A
  token is never accepted unchecked.
- glovo-skill exits: it is restarted with backoff (1s doubling to 30s). A
  call in flight fails, and calls during the restart get an "unavailable"
  error. Neither is reported as a success.
- SIGTERM: stop accepting connections, let in-flight tool calls finish,
  close the MCP sessions, then stop the child.

## Cloudflare tunnel

Out of scope here. The tunnel must forward `glovo.mctl.ai` to
`http://127.0.0.1:8787` and keep the `Host: glovo.mctl.ai` header.
glovo-home accepts only that host and loopback names, and answers 421 for
any other host.
