# glovo-home on the Mac mini

`glovo-home` is an owner-only MCP gateway. It runs on the owner's Mac mini
next to the open-source local Glovo MCP server
[`glovo-skill`](https://github.com/denya/glovo-skill) (stdio, Node) and is exposed through a
Cloudflare tunnel as `https://glovo.mctl.ai/mcp`.

```
MCP client --HTTPS--> Cloudflare tunnel --> 127.0.0.1:8787 glovo-home --stdio--> node glovo-skill
```

What it does:
- serves a public landing page at `/` (EN, ES, RU; `?lang=`, then a
  cookie, then `Accept-Language`): what the server is, how to connect a
  client, what it never does. No JavaScript; the only inline style is
  admitted by its CSP hash;
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
  duration). Arguments and results are never logged;
- keeps the Glovo session alive with one `glovo_get_basket` call a day and
  runs an alert hook when that fails;
- restarts glovo-skill when `scripts/glovo-relogin.sh` installs a new
  session, without restarting itself.

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
| `GLOVO_HOME_EXTRA_TOOLS` | no | Exact tool names to mirror on top of the 27 known glovo-skill 0.2.1 tools. Any other tool glovo-skill offers is refused and logged ("not on the allowlist"); the checkout/payment/order name rules still apply to listed names. |
| `GLOVO_HOME_CHILD_STDERR` | no | `inherit` sends glovo-skill's stderr to glovo-home's stderr (the log file). Default: discarded, because glovo-skill's output is not under this repository's control and may carry request data; the "no arguments or results in the log" guarantee covers glovo-home's own lines only. |
| `GLOVO_HOME_KEEPALIVE` | no | Session keepalive interval, default `24h`; `0` turns it off (minimum `1m`). |
| `GLOVO_HOME_ALERT_CMD` | no | Absolute path of an executable run when the keepalive fails. Unset: the failure is only logged. |

The child gets an explicit environment: `HOME`, `PATH`, `USER`, `LOGNAME`,
`LANG`, `LC_ALL`, `TMPDIR`, `TZ`, every `GLOVO_*` variable except
`GLOVO_HOME_*`, and every `PLAYWRIGHT_*` variable. Nothing else is
passed.

`glovo_login` opens a browser on the mini itself. If remote callers should
not be able to trigger that, add it to `GLOVO_HOME_BLOCKED_TOOLS`.

## Session keepalive

Once per `GLOVO_HOME_KEEPALIVE` (the first time one interval after start),
glovo-home calls `glovo_get_basket` through the same path as a remote
call. That tool needs a valid Glovo session, so glovo-skill refreshes an
expiring access token and rotates the refresh token. (`glovo_auth_status`
does not work for this: it only reads the local file.)

Only a result that is not `isError` and has content counts as `ok`. An
auth error, any other error, an empty result, or glovo-skill being down is
a failure. On failure, glovo-home runs `GLOVO_HOME_ALERT_CMD` without a
shell, with exactly one argument, which is one of these fixed messages:

- `glovo-home: Glovo session keepalive failed; re-login may be needed`
- `glovo-home: Glovo session keepalive could not run; glovo-skill is unavailable`

The hook runs with glovo-home's environment and a 30s limit, and its output
is discarded. It never receives token, session or result content. Every
check writes one `session_keepalive` log line with its outcome and
duration.

`deploy/mac-mini/glovo-alert` is the hook used on the mini. It sends the
message to the owner through a dedicated Telegram bot:

```sh
install -m 755 deploy/mac-mini/glovo-alert ~/.local/bin/glovo-alert
umask 077; mkdir -p ~/.config/glovo-alert
# bot-token: the BotFather token (pipe it from the clipboard, never type it)
# chat-id:   the owner's Telegram user id; the owner sends /start to the bot once
/usr/libexec/PlistBuddy -c "Add :EnvironmentVariables:GLOVO_HOME_ALERT_CMD string $HOME/.local/bin/glovo-alert" \
  ~/Library/LaunchAgents/com.mctl.glovo-home.plist
```

The token never appears in argv (curl reads its URL from stdin). The hook
exits non-zero when a file is missing or Telegram refuses the message, so
the failure shows up as `alert command failed` in glovo-home's log. After
editing the plist, `launchctl bootout` and wait until
`launchctl print gui/$(id -u)/com.mctl.glovo-home` reports the service gone
before `launchctl bootstrap`; bootstrapping while the old instance is still
unloading fails with `5: Input/output error`.

## Session hot-reload and re-login

glovo-home checks a reload marker next to the session file
(`<GLOVO_SESSION_PATH>.reload`, by default `~/.glovo/session.json.reload`)
every 5s, using stat only (size and mtime). It never opens, reads, logs or
hashes the marker, and it does not touch the session file at all. When the
marker's size or mtime has changed and then held steady for one more check,
glovo-home refuses new calls, waits for in-flight calls to finish, and
restarts glovo-skill. A stat that fails is logged and is not treated as a
change. The session file itself is not watched: glovo-skill rewrites it on
every token refresh, and restarting on that would cause a short outage per
refresh. glovo-skill already re-reads the session on every tool call, so
the restart is a safety net. To force one by hand, run
`date > ~/.glovo/session.json.reload` on the host.

When the keepalive alert says a re-login is needed, run this on a machine
with a browser and glovo-skill installed:

```bash
GLOVO_HOME_HOST=<ssh destination of the mini> scripts/glovo-relogin.sh
```

The script:

1. runs glovo-skill's `scripts/login.mjs` into a session file inside a
   fresh 0700 temporary directory;
2. streams the file over ssh into a 0600 temporary file in a 0700
   directory on the host, then renames it over the old session (atomic),
   then writes a timestamp to the reload marker;
3. deletes the local temporary directory, also on error or Ctrl-C.

The file is never printed. Optional variables: `GLOVO_SKILL_DIR` (default
`~/.local/share/glovo-skill`), `GLOVO_NODE` (default `node`) and
`GLOVO_HOME_REMOTE_SESSION` (default `.glovo/session.json`, relative to
the remote home directory). glovo-home sees the marker and restarts glovo-skill
within about 15s.

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
