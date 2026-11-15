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
- restarts glovo-skill (or, with the native backend, re-reads the
  session) when `scripts/glovo-relogin.sh` installs a new
  session, without restarting itself.

Basket writes go straight through, as they do with the local tool. There is
no approval step, and glovo-skill has no checkout or payment tool.

## Backends

`GLOVO_HOME_BACKEND` picks where the tools run. Exactly one backend runs.

- `skill` (the default): glovo-home starts glovo-skill as a stdio child and
  mirrors its tools, as above.
- `native`: glovo-home serves the same 27 tools itself, in Go
  (`internal/glovotools`), with no Node process. The tool names, titles,
  descriptions, input schemas and result shapes are glovo-skill 0.2.1's,
  checked against goldens recorded from the upstream server. It reads the
  same session file (`GLOVO_SESSION_PATH`) on every call, rotates the
  refresh token when it refreshes, and writes the file atomically at mode
  0600. Basket writes go through the same mutation allowlist as the
  keepalive; there is still no checkout, payment or order tool.
  `GOOGLE_MAPS_API_KEY`, if set, enables Google Places evidence in
  `glovo_get_suggestions`, as it does upstream (the skill child never
  receives that variable).

`glovo_login` exists in both backends with the same schema, but in `native`
it never opens a browser: it returns an error that points to
`scripts/glovo-relogin.sh` (see below).

Switching is a configuration change plus a restart of glovo-home; the
session file is shared, so no re-login is needed in either direction.

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
| `GLOVO_HOME_BACKEND` | no | `skill` (default) or `native`; anything else is refused at start. |
| `GLOVO_HOME_NODE` | skill only | Node binary for the child. Ignored by `native`. |
| `GLOVO_HOME_SERVER` | skill only | glovo-skill entry point (`dist/server.mjs`). Ignored by `native`. |
| `GLOVO_SESSION_PATH` | no | Passed to glovo-skill unchanged; read and rotated directly by `native` (default `~/.glovo/session.json`). |
| `GOOGLE_MAPS_API_KEY` | no | `native` only: Google Places evidence for `glovo_get_suggestions`. |
| `GLOVO_HOME_LISTEN` | no | Listen address, default `127.0.0.1:8787`. A non-loopback address is refused unless `GLOVO_HOME_ALLOW_NON_LOOPBACK=true`. |
| `GLOVO_HOME_CALL_TIMEOUT` | no | Per-tool-call limit, default `3m`. |
| `GLOVO_HOME_BLOCKED_TOOLS` | no | Extra exact tool names to refuse, e.g. `glovo_login`. |
| `GLOVO_HOME_EXTRA_TOOLS` | no | `skill` only (ignored with a warning by `native`, which serves exactly the 27 tools). Exact tool names to mirror on top of the 27 known glovo-skill 0.2.1 tools. Any other tool glovo-skill offers is refused and logged ("not on the allowlist"); the checkout/payment/order name rules still apply to listed names. |
| `GLOVO_HOME_CHILD_STDERR` | no | `inherit` sends glovo-skill's stderr to glovo-home's stderr (the log file). Default: discarded, because glovo-skill's output is not under this repository's control and may carry request data; the "no arguments or results in the log" guarantee covers glovo-home's own lines only. |
| `GLOVO_HOME_KEEPALIVE` | no | Session keepalive interval, default `24h`; `0` turns it off (minimum `1m`). |
| `GLOVO_HOME_ALERT_CMD` | no | Absolute path of an executable run when the keepalive fails. Unset: the failure is only logged. |

The child gets an explicit environment: `HOME`, `PATH`, `USER`, `LOGNAME`,
`LANG`, `LC_ALL`, `TMPDIR`, `TZ`, every `GLOVO_*` variable except
`GLOVO_HOME_*`, and every `PLAYWRIGHT_*` variable. Nothing else is
passed.

With the `skill` backend, `glovo_login` opens a browser on the mini itself.
If remote callers should not be able to trigger that, add it to
`GLOVO_HOME_BLOCKED_TOOLS`. With `native` it only returns the re-login
pointer.

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
the remote home directory). glovo-home sees the marker within about 15s and
restarts glovo-skill (`skill`), or re-reads the session at once (`native`,
which re-reads it on every call anyway, so nothing restarts).

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

## Glovo egress proxy (tinyproxy)

This section prepares slice E2 of epic #11. Nothing here is installed by
this repository; the owner runs these steps on the mini and in the Tailscale
admin console.

The hosted mctl-glovo service (cluster) reaches Glovo only through the mini,
over the tailnet, through a standard forward proxy. TLS stays end to end:
the proxy sees the CONNECT target host and the byte counts, never a request,
a header or a bearer token.

```
mctl-glovo pod --explicit proxy--> tailscale sidecar (userspace, tag:glovo-egress)
  --tailnet--> mini:3128 tinyproxy --CONNECT, TLS end to end--> api.glovoapp.com
```

glovo-home is not affected: it runs on the mini and calls Glovo directly
through glovo-skill, as before.

### Risk the owner accepts (R3, design doc #2 §8)

Every linked account leaves from one home IP. Glovo may block that IP or
ban accounts, the owner's own included, and that would also break
glovo-home on the same connection. The proxy cannot see accounts, so the
mitigations live in mctl-glovo and are not optional: a per-account and a
global request rate cap, a kill switch that stops all outbound Glovo calls,
an alert when Glovo starts answering 403 or 429, and no traffic at all when
the proxy is down (refusal or 503, never a direct call from the cluster).

### Install

```bash
brew install tinyproxy
mkdir -p ~/.config/tinyproxy
install -m 644 deploy/mac-mini/tinyproxy.conf ~/.config/tinyproxy/tinyproxy.conf
install -m 644 deploy/mac-mini/tinyproxy-filter ~/.config/tinyproxy/tinyproxy-filter
```

In `~/.config/tinyproxy/tinyproxy.conf` replace every `REPLACE_ME` with the
account name. Do not use `brew services start tinyproxy`: that runs Homebrew's
default configuration, which listens on every interface. The LaunchAgent
below runs this configuration only.

What the configuration does (each directive is annotated in the file with
its tinyproxy.conf(5) meaning, checked against tinyproxy 1.11.3):

- `Listen 127.0.0.1`, `Port 3128`, `Allow 127.0.0.1` only; the tailnet
  reaches it through `tailscale serve` (below);
- `ConnectPort 443` only;
- `Filter` with the single rule `^api\.glovoapp\.com$`, `FilterType ere`,
  `FilterURLs No` (match the host; for HTTPS that is the CONNECT target) and
  `FilterDefaultDeny Yes` (anything else is refused with 403). In 1.11
  `FilterExtended` is deprecated in favour of `FilterType ere`;
- `DisableViaHeader Yes`, no `XTinyproxy`;
- `MaxClients 20`, `Timeout 60`, `LogLevel Notice` to
  `~/Library/Logs/mctl-tinyproxy.log`.

`ConnectPort` does not restrict plain-HTTP (absolute-form) requests, so a
tailnet client could still send cleartext HTTP to `api.glovoapp.com:80`
through the proxy. The cluster client refuses that: `glovoapi.WithRequireProxy`
requires an https base URL.

### launchd

Save as `~/Library/LaunchAgents/com.mctl.tinyproxy.plist`, replace every
`REPLACE_ME`, then run
`launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.mctl.tinyproxy.plist`.
`-d` keeps tinyproxy in the foreground, which launchd needs; `-c` names the
configuration file (tinyproxy(8)). The Homebrew path is `/usr/local/bin`
on the x86_64 mini (`/opt/homebrew/bin` on Apple silicon).

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.mctl.tinyproxy</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/tinyproxy</string>
    <string>-d</string>
    <string>-c</string>
    <string>/Users/REPLACE_ME/.config/tinyproxy/tinyproxy.conf</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ThrottleInterval</key>
  <integer>10</integer>
  <key>StandardOutPath</key>
  <string>/Users/REPLACE_ME/Library/Logs/mctl-tinyproxy.stdout.log</string>
  <key>StandardErrorPath</key>
  <string>/Users/REPLACE_ME/Library/Logs/mctl-tinyproxy.stderr.log</string>
</dict>
</plist>
```

### Tailnet exposure (`tailscale serve`)

tinyproxy listens on loopback only. Tailscale carries tailnet connections to
it:

```bash
tailscale serve --bg --tcp 3128 tcp://127.0.0.1:3128
tailscale serve status        # shows tcp://<mini>:3128 -> 127.0.0.1:3128
tailscale serve --tcp=3128 off  # to stop exposing it
```

Why not `Listen` on the Tailscale address: with the macOS application
firewall on in stealth mode (the mini's setting), a listener on the
Tailscale interface accepts connections but no data reaches tinyproxy
(`read_request_line: Client ... closed socket before read`, curl `Proxy
CONNECT aborted`). Allowing tinyproxy through the firewall would be a
system security setting change; `tailscale serve` needs none, because
tailscaled is already allowed, and it leaves nothing listening on a
network interface.

A connection from the mini to its own Tailscale address through
`serve` does not pass data either, so test from another tailnet node.

After editing the configuration or the plist, `launchctl bootout
gui/$(id -u)/com.mctl.tinyproxy` and wait until `launchctl print
gui/$(id -u)/com.mctl.tinyproxy` reports the service gone before
`launchctl bootstrap`; bootstrapping while the old instance is still
unloading fails with `5: Input/output error`.

### Tailnet ACL

tinyproxy sees every tailnet client as 127.0.0.1, so the tailnet policy
alone limits the proxy to the cluster's sidecar. Until that grant exists,
every node the tailnet ACL lets reach the mini (including shared devices)
can use the proxy, for `api.glovoapp.com:443` only. In the Tailscale admin console
(Access controls), tag the mini (here `tag:glovo-proxy-host`, an example
name) and add a grant that lets only `tag:glovo-egress` reach it, on
`tcp:3128` only:

```json
{
  "tagOwners": {
    "tag:glovo-egress":     ["autogroup:admin"],
    "tag:glovo-proxy-host": ["autogroup:admin"]
  },
  "grants": [
    {
      "src": ["tag:glovo-egress"],
      "dst": ["tag:glovo-proxy-host"],
      "ip":  ["tcp:3128"]
    }
  ]
}
```

Merge this into the existing policy rather than replacing it, and check
that no broader rule (for example the default allow-all) also lets other
nodes reach the mini on 3128. Tagging a node that was owned by a user
changes its ownership to the tag; check what else relies on the mini's
current identity first. `tag:glovo-egress` should carry nothing but this
grant, and the sidecar's auth key should be ephemeral and pre-tagged.

### Checks

From another tailnet node that the ACL allows (or, before the ACL is
narrowed, any tailnet node):

```bash
# reaches Glovo: any HTTP status from api.glovoapp.com (often 404 for /) means the tunnel works
curl -sS -o /dev/null -w '%{http_code}\n' -x http://<mini-tailscale-ip>:3128 https://api.glovoapp.com/
# refused by the filter: curl reports "CONNECT tunnel failed, response 403"
curl -sS -o /dev/null -x http://<mini-tailscale-ip>:3128 https://example.com/
# refused by ConnectPort: a port other than 443
curl -sS -o /dev/null -x http://<mini-tailscale-ip>:3128 https://api.glovoapp.com:8443/
```

`http://api.glovoapp.com/` passes (Glovo answers 301): that is the known
plain-HTTP gap above, closed on the client by `glovoapi.WithRequireProxy`.

On the mini, testing through its own Tailscale address does not work (see
above); `curl -x http://127.0.0.1:3128 ...` tests tinyproxy alone, and `grep -i filtered
~/Library/Logs/mctl-tinyproxy.log` shows the refused requests. After the
ACL is in place, the same curl from a node without `tag:glovo-egress` must
time out.

### Cluster side (out of scope here)

The mctl-glovo pod reaches the proxy through a Tailscale userspace sidecar
(ephemeral node, `tag:glovo-egress`, auth key in Vault via ExternalSecret).
That is slice E3 in mctl-gitops. A userspace tailscaled gives the pod no
route to tailnet addresses, so E3 has to expose the mini's port 3128 to the
service container as a plain TCP forward on loopback (glovoapi talks to one
HTTP proxy and does not chain proxies). The service then builds its
glovoapi client with `WithProxy(http://<that address>:3128)` and
`WithRequireProxy()`, so `HTTPS_PROXY`/`NO_PROXY` are never consulted, a
missing proxy setting is a start-up error, and an unreachable proxy fails
the call. JWKS, mctl-api and Vault traffic does not use this proxy.
