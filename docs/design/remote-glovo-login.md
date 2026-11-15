# Remote Glovo login

Status: design plus a prototype helper (`remote-login/`). The glovo-home
integration is a follow-up and is described here, not implemented.

## Problem

The Glovo connector reaches the owner's real Glovo account through a home
server: Cloudflare tunnel, then `glovo-home` (Go, loopback only), then a
stdio child `glovo-skill` (node), then `api.glovoapp.com`. The Glovo session
(access token, refresh token, device urn, location headers) lives in a 0600
`session.json` on that server.

Today the session is created by `glovo-skill`'s `glovo_login`: it opens a
headful Chrome with playwright-core on the server's own display, and the user
signs in there while the helper captures the `/oauth/token|refresh|2fa`
responses. Re-linking therefore needs someone in front of that display, or
Screen Sharing. That does not work from a phone.

## Goal

Re-linking works from any device, phone included, with no Screen Sharing:

1. The user connects the connector.
2. Glovo is not linked, so a browser page opens.
3. The user signs in to Glovo in that page.
4. The session is saved on the home server and the connector works.

## Decision: a CDP screencast of one locked-down tab

The home server runs a fresh Chrome per attempt. The view of its single
active tab is streamed to a page served by glovo-home, and the user's taps
and typing are sent back.

### Alternatives considered

| | CDP screencast (chosen) | noVNC of a dedicated display | Glovo login in the user's own browser |
|---|---|---|---|
| What the remote user can reach | The page area of one tab. No address bar, no tabs UI, no OS, no file dialogs. | A whole display: the browser UI, any window that opens on it, OS dialogs. | Nothing on the server. |
| macOS fit | Works headless; nothing to install beyond Chrome. | macOS has no Xvfb. A dedicated display means a virtual display driver or a VNC of the real screen, which is Screen Sharing again. | n/a |
| Phone use | Canvas sized to the phone's viewport; the remote page gets the mobile layout and real touch events. | A desktop-sized framebuffer shrunk onto a phone; pinch-zoom to type. | Native. |
| Capturing tokens | We see the responses in the browser we control. | Same. | Impossible: the tokens land in a browser we do not control, and Glovo has no OAuth for third parties. |
| New attack surface | One WebSocket carrying JPEGs out and validated input events in. | The VNC protocol and a websockify bridge, plus everything on the display. | n/a |

noVNC loses on both stated criteria: it is worse on a phone and it exposes
more than a page. The "user's own browser" option would be ideal but cannot
work, because the token must end up on the server.

Direct CDP exposure (DevTools frontend, or proxying the CDP WebSocket) is
rejected outright: CDP is full control of the browser, including
`Runtime.evaluate`, `Network.getCookies` and `Target.createTarget` to any URL.
The helper speaks CDP itself and exposes only a tiny input protocol.

## Flow

```
MCP client          glovo-home (Go)                 remote-login helper (node)      Chrome
    | tools/call glovo_link_start                          |                          |
    |------------------->| auth: MCTL JWT + allowlist      |                          |
    |                    | mint link (256-bit, 10 min,     |                          |
    |                    |   bound to principal, memory)   |                          |
    |<-- result: URL ----| (+ URL elicitation if supported)|                          |
    |                                                      |                          |
 user's browser opens https://<glovo host>/link/<code>     |                          |
    |------------------->| redeem code once; set cookie    |                          |
    |                    | spawn helper, token on stdin -->|                          |
    |<-- 302 /link/viewer| read {"port":N} from stdout     |                          |
    |---- GET viewer, WS /link/ws ---------->| proxy + X-Remote-Login-Token -->| launch, fresh context ->|
    |<========= JPEG frames / input events =================================>| CDP screencast/input ->|
    |                    |                                 | /oauth/token seen        |
    |                    |                                 | write session.json 0600  |
    |                    |<-- {"result":"captured"}, exit 0| kill browser             |
    |                    | mtime watch -> restart glovo-skill child                    |
```

### 1. `glovo_link_start` (glovo-home, follow-up)

An MCP tool served by glovo-home itself, not by glovo-skill, so it works
exactly when the Glovo session is missing or broken. It requires the same
MCTL OAuth JWT and principal allowlist as every other tool.

It mints a link code:

- 32 bytes from `crypto/rand`, base64url, so 256 bits;
- TTL 10 minutes, single use, bound to the calling principal (`sub` and
  `iss` of the verified JWT);
- stored only in glovo-home's memory, as a SHA-256 of the code keyed to
  `{principal, expires, state}`. A restart forgets every link, which is the
  safe direction;
- at most one outstanding link. Minting a new one revokes the previous one,
  and if a login run is in progress it is aborted first.

The tool result is text plus the URL
(`https://<glovo host>/link/<code>`) and an instruction to open it. When the
client supports URL-mode elicitation (below), glovo-home also sends
`elicitation/create` with `mode: "url"` and the same URL, so the client shows
a native "open this link" prompt. The tool result is always the fallback,
never the elicitation alone.

Other tools keep answering "not linked" with a hint to call
`glovo_link_start`. A tool should not start a link implicitly: minting a
credential-bearing URL is an explicit user action.

### MCP URL-mode elicitation: who supports it

URL mode was added in MCP spec revision 2025-11-25
(<https://modelcontextprotocol.io/specification/2025-11-25/client/elicitation>).
A client advertises it with `capabilities.elicitation.url`; a server must not
send a mode the client did not declare. The spec says URL mode is meant for
out-of-band steps such as third-party credentials, and that it is not for
authorizing the client to the server itself, which is exactly our case: the
MCP connection is already authorized, and the URL collects a Glovo sign-in.

State as checked on 2026-11-15:

| Client | Form mode | URL mode | Source |
|---|---|---|---|
| Claude Code CLI | yes, since 2.1.76 | yes: 2026-07-28 connections since 2.1.281, 2025-11-25 servers since 2.1.287 | Claude Code CHANGELOG; code.claude.com/docs/en/mcp |
| Claude Code on the web (claude.ai/code), Desktop Code tab | yes | yes | code.claude.com/docs/en/mcp ("Answer an elicitation request in a cloud session") |
| claude.ai connectors in chat (web, Desktop chat, mobile) | no evidence | no evidence: the declared capability is the bare `elicitation: {}`, which means form only | code.claude.com/docs/en/mcp; the claude.ai custom connector help does not mention elicitation |

So URL elicitation helps Claude Code users today, and does nothing for
claude.ai chat, which is where a phone user is most likely to be. The design
therefore never depends on it: the tool result carries the link in every
client, and glovo-home sends the elicitation only when the client's
`initialize` declared `elicitation.url`.

### 2. Redeeming the link

`GET /link/<code>` on glovo-home:

- looks up SHA-256(code) in constant time; unknown, expired or already used
  gets the same 404 page. A used code is deleted on first redemption, before
  the helper starts;
- the redeeming request carries no MCTL token (it comes from a plain
  browser), so the code is the bearer credential for these 10 minutes. This
  is why it is single use, short-lived, and unusable once a viewer session
  exists;
- sets a viewer cookie: 256-bit random, `__Host-glovo_link`, `Secure`,
  `HttpOnly`, `SameSite=Strict`, `Path=/`, expiry = the link's remaining
  TTL. It is bound to the run;
- answers `303 See Other` to `/link/viewer`, so the code leaves the address
  bar and history entry right away, with `Referrer-Policy: no-referrer`;
- spawns the helper (one at a time; a second redemption while a run is live
  is refused with 409).

Optional hardening for a later step: also require an MCTL sign-in on the
viewer page (the OAuth flow already exists at api.mctl.ai), so a leaked link
alone is not enough. It costs one more redirect on the phone; it is not in
the first cut.

### 3. The helper (`remote-login/`, prototype in this PR)

A node ESM program on playwright-core. One process per attempt:

- configuration from the environment (`GLOVO_REMOTE_LOGIN_SESSION_OUT`,
  `GLOVO_REMOTE_LOGIN_ORIGIN`, timeout, providers); the per-run helper token
  on **stdin**, never argv or env, which other local processes can read;
- listens on `127.0.0.1` only, on a random port it prints as
  `{"port":N}`;
- serves the viewer (`/viewer`, `/viewer/viewer.js`, `/viewer/viewer.css`)
  and one WebSocket at `/ws`;
- accepts the WebSocket only with `Origin` equal to the configured origin
  **and** `X-Remote-Login-Token` equal to the helper token (constant-time).
  glovo-home adds that header when it proxies; the viewer page never has
  it. Three bad tokens end the run;
- one viewer connection at a time (409 for a second); a viewer may reconnect
  while the run is alive and gets the last frame immediately;
- prints `{"result":"captured"|"timeout"|"error"}` and exits 0, 2 or 3.

glovo-home's proxy for `/link/viewer` and `/link/ws` requires the viewer
cookie and checks `Origin` itself before it proxies; the helper's checks are
a second layer, not the only one.

### 4. Streaming and input

- `Page.startScreencast` with JPEG quality 60, `maxWidth/maxHeight` = the
  viewer's viewport times its device pixel ratio. Each frame is acked and
  forwarded as one binary WebSocket message; frames are dropped, not
  queued, when the socket has more than 2 MB buffered.
- Headless Chrome emits frames only when the compositor produces one, and
  after a navigation of a static page it sometimes produces none (seen in
  the tests). The helper therefore takes one `Page.captureScreenshot` when
  the view may have changed (navigation, input) and no frame arrived for
  400 ms.
- The viewer sends `hello` with its viewport (`width`, `height`, `dpr`,
  `mobile` from `pointer: coarse`). The helper clamps it (320-1600 x
  480-1600, dpr <= 3) and creates the context with `isMobile` and
  `hasTouch` for a phone, so Glovo serves its mobile layout. The viewport is
  a parameter of the run, fixed at the first `hello`.
- Input from the viewer is validated field by field and mapped to:
  `Input.dispatchMouseEvent` (press, release, move, wheel; left, middle,
  right; click count <= 3), `Input.dispatchTouchEvent` (up to 5 points),
  `Input.dispatchKeyEvent` for a fixed set of named keys (Enter, Backspace,
  Tab, Escape, Delete, arrows), and `Input.insertText` for typed text (at
  most 256 characters per message, no control characters). Coordinates must
  lie inside the viewport. Anything else closes the socket with 4400. No
  modifier shortcuts are forwarded, so there is no Ctrl+L, Ctrl+T or
  devtools key.
- On a phone, a hidden textarea brings up the soft keyboard; its
  `beforeinput` events become `text` and named keys, and it is cleared after
  every event.
- Messages are capped at 4 KiB and 300 per second.

### 5. Navigation lockdown

Every request the page makes passes `context.route()`, and every WebSocket
passes `context.routeWebSocket()`. Requests fall into three classes, each
with its own allowlist; anything not listed is aborted. Only `https:` (and
`wss:` judged as `https:`) is allowed. Service workers are blocked so that
no request can bypass `route()`.

| Class | Allowed |
|---|---|
| Top-level navigation (what the user sees, including popups) | `glovoapp.com`, `www.glovoapp.com` |
| Subframe navigation | `www.google.com/recaptcha/*`, `www.recaptcha.net/recaptcha/*` |
| Subresources and WebSockets | `glovoapp.com` and subdomains (web app, `api.`, `cards.`), `glovo.dhmedia.io` (images), `www.google.com/recaptcha/*`, `www.gstatic.com/recaptcha/*`, `www.recaptcha.net/recaptcha/*`, `incognia.com` and subdomains, `usercentrics.eu` and subdomains |

How the list was derived:

- `glovo-skill` `src/auth/login.mjs`: the start page is
  `https://glovoapp.com/en/login`, tokens come from
  `https://api.glovoapp.com/oauth/{token,refresh,2fa/...}`, the profile from
  `/v3/me`, and the device and location headers are read from requests to
  `api.glovoapp.com`.
- The public, signed-out login page (`glovoapp.com/en/login`, fetched
  anonymously on 2026-11-15) offers exactly three methods: **Google**,
  **Facebook** and **Email**. Its runtime config carries a reCAPTCHA key
  (the page says "protected by reCAPTCHA"), the Incognia web SDK URL
  (`repo.incognia.com`, a device-risk SDK), a Usercentrics consent manager
  (`web.eu1.cmp.usercentrics.eu`), and flags
  `CUSTOMER_WEB_IS_GOOGLE_OAUTH_REMOVED: true` (the legacy Google OAuth
  redirect is gone; Google now runs through Google Identity Services, the
  `google-button-prompt-target` element) and `WEB_FACEBOOK_LOGIN_ENABLED:
  true`. There is no Apple sign-in on the web. Kaspi
  (`oauth.kaspi.kz`) is configured but is a Kazakhstan-only method and is
  not allowed.
- Not listed on purpose: Google Tag Manager, Adjust, Datadog RUM and other
  analytics. Blocking them does not stop a sign-in.

**Identity providers.** The email method needs no third-party identity
provider at all: the email or phone, the password or the one-time code all
go to `glovoapp.com` and `api.glovoapp.com`. That is the default and the
only method enabled out of the box. Social sign-in is opt-in per run
(`GLOVO_REMOTE_LOGIN_PROVIDERS=google,facebook`):

- `google`: top and frame `accounts.google.com`; subresources
  `accounts.google.com`, `ssl.gstatic.com`, `www.gstatic.com`,
  `fonts.gstatic.com`, `lh3.googleusercontent.com`. It is off by default
  because Google refuses sign-in from browsers under automation ("This
  browser or app may not be secure"), and the helper does not try to hide
  that it is automated.
- `facebook`: top `www.facebook.com`, `m.facebook.com`,
  `web.facebook.com`; frame `www.facebook.com`; subresources `facebook.com`
  and subdomains, `connect.facebook.net`, `fbcdn.net` and subdomains.

Enabling a provider widens what a leaked link can show: a Google or
Facebook account page, signed out, in a fresh profile. It is still not a
general browser.

This list is derived, not observed in a real sign-in (the rules for this
work forbid signing in to Glovo as an agent). The first real run must be
read through the audit lines: every block is logged as
`{"event":"blocked","kind":...,"host":...}`, host only. A host the sign-in
really needs is added by a reviewed change, never by widening to a wildcard.

Defense in depth on top of `route()`:

- a `framenavigated` watchdog closes any page whose main frame lands on an
  off-list URL (a scheme `route()` never sees, for example);
- `acceptDownloads: false`, no permissions granted, JavaScript dialogs
  dismissed;
- Chrome flags: no extensions, no sync, no background networking, no
  component updates, no first-run UI.

### 6. Browser lifecycle

- **Fresh every time.** `chromium.launch()` creates a new temporary profile,
  and every run uses one `newContext()` with no storage state. Nothing is
  reused, so no Glovo cookie, Google cookie or autofill survives between
  runs. Playwright deletes the profile directory when the browser exits.
- **One run at a time.** glovo-home starts at most one helper; the helper
  starts at most one browser.
- **Hard timeout.** 10 minutes by default (bounded 10 s to 15 min), counted
  from helper start, so it also covers "nobody ever opened the link". The
  link's own TTL is the same 10 minutes.
- **Killed after capture or timeout.** `close()` asks the browser to exit,
  waits at most 5 s, then sends SIGKILL to the browser PID it read from
  `SystemInfo.getProcessInfo` at start. glovo-home additionally kills the
  helper's process group if the helper itself outlives the timeout plus a
  grace period.
- Capture rule: the first response from the API origin on
  `/oauth/token`, `/oauth/refresh` or `/oauth/2fa/*` with HTTP 2xx and a JSON
  body holding **both** an access token and a refresh token. A token-only
  response is ignored rather than saved, since such a session dies within
  hours and would look like success. After the token, the helper waits up to
  15 s for `/v3/me` (customer id) and then finishes.

### 7. Writing the session and reloading glovo-skill

The helper writes only to `GLOVO_REMOTE_LOGIN_SESSION_OUT`:

- a temp file in the same directory, opened with `O_CREAT|O_EXCL` and mode
  0600, so it never exists with wider permissions whatever the umask;
  `fsync`, `rename` over the target, `fsync` of the directory;
- it refuses a relative path, a symlink target, a missing directory and a
  group- or world-writable directory, instead of creating or fixing them;
- the session shape is what `glovo-skill`'s `loadSession` reads (same fields
  as `login.mjs`, plus `linkedVia: "remote-login"`).

glovo-home watches the file's mtime (the hot-reload is being built in the
glovo-home gateway change) and restarts the `glovo-skill` child, which then
loads the new session. glovo-home learns the outcome from the helper's
result line and exit code and records it; the MCP client learns it the next
time it calls a tool (`glovo_auth_status`).

The helper never prints, logs or hashes token contents. Its stdout carries
only the port and the result word.

## Threat model

Assets: the Glovo session (it can fill the owner's basket and read order
history and addresses), and the remote browser itself while it runs.

| Threat | Mitigation |
|---|---|
| **Link leakage** (chat transcript, client logs, shoulder-surfing, a phone's history sync) | 256-bit, single use, 10-minute TTL, at most one outstanding, bound to the principal that minted it, stored only as a hash in memory. Redeemed once, then replaced by an HttpOnly `__Host-` cookie and a 303 so the code leaves the address bar. A leaked link that was already used is useless. A leaked unused link lets the holder sign in to **their own** Glovo account into the owner's slot, which would be visible on the next tool call; the optional MCTL sign-in on the viewer closes this. |
| **Someone else drives the browser** | The WebSocket needs the viewer cookie (glovo-home), the exact Origin (both layers) and the helper token (helper). The helper listens on loopback only. One viewer at a time. |
| **The remote browser as an open proxy** | Navigation and request allowlists, no address bar, no modifier keys, no downloads, popups judged as top-level, watchdog closes off-list pages. |
| **Frame data** (frames show the email, masked password field, the one-time code and the profile) | Frames go only to the authenticated viewer over TLS (Cloudflare tunnel, then loopback). They are never written to disk, logged or hashed. The helper keeps only the newest frame in memory, to repaint a reconnecting viewer, and drops it when the run ends. Cloudflare terminates TLS and can technically see frames; that is the same trust already placed in it for the MCP traffic. |
| **Keystrokes** | Forwarded into CDP and not retained. Audit lines carry an event name and a few enumerated fields only (`src/audit.mjs` drops any other key), never text, coordinates or URLs beyond the host. |
| **CSRF / cross-site WebSocket hijacking** | WebSockets are not covered by CORS, so the Origin is checked explicitly in glovo-home and again in the helper. The cookie is `SameSite=Strict`. The redemption endpoint is a GET that only consumes a 256-bit secret; there is no state-changing endpoint reachable without it. |
| **Clickjacking** (framing the viewer and tricking taps) | `Content-Security-Policy: frame-ancestors 'none'` and `X-Frame-Options: DENY`. |
| **Script injection into the viewer** | Strict CSP: `default-src 'none'; script-src 'self'; style-src 'self'; img-src blob:; connect-src 'self'; base-uri 'none'; form-action 'none'`. No inline script or style. Frames are drawn with `createImageBitmap` from binary messages, never as HTML. |
| **Token leaks through logs or argv** | The helper token arrives on stdin. The session path is the only output. Tests assert that neither the helper token nor fixture tokens appear in stdout, stderr or audit lines. |
| **Session file tampering or wide permissions** | 0600 from creation, atomic rename, refuses symlinks and shared directories. |
| **A malicious page inside the allowlist** (an XSS on glovoapp.com) | It runs in a throwaway profile with nothing else signed in, cannot download, cannot reach other hosts, and dies with the run. |
| **Glovo flags the sign-in** (reCAPTCHA, Incognia device risk) | Not a security threat but a usability one. The reCAPTCHA image challenge works through the stream. The Incognia SDK is allowed so the device check does not fail closed on Glovo's side. |
| **Resource exhaustion** | One run, one viewer, message size and rate caps, frame dropping under backpressure, hard timeout. |

## Failure modes (all fail closed)

| Situation | Behaviour |
|---|---|
| Link unknown, expired or used | 404, same page for all three; nothing starts. |
| A run is already active | Second redemption gets 409; minting a new link aborts the old run first. |
| Chrome fails to launch | Helper ends with `error`, exit 3; no session written; the viewer shows "Something went wrong". |
| Bad Origin, missing or bad helper token | 403 / 401 on upgrade; three bad tokens end the run. |
| Malformed or unknown input message | Socket closed with 4400; the run continues and the viewer may reconnect. |
| Viewer drops (phone locks, network change) | The browser keeps its state until the timeout; reconnecting shows the last frame. |
| No token within the timeout | `timeout`, exit 2, browser killed, nothing written. The existing session.json, if any, is left untouched. |
| Token response without a refresh token, non-2xx, or not JSON | Ignored; the run continues until a complete one or the timeout. |
| Session directory missing, shared or the target a symlink | Write refused, exit 3; the old file stays as it was. |
| Write or rename fails mid-way | The temp file is removed; the target is either the old or the new complete file. |
| Off-list host needed by the real login | Blocked and audited by host. The run fails visibly; the fix is a reviewed allowlist change. |
| Helper hangs | glovo-home kills its process group after the timeout plus a grace period. |
| glovo-home restarts mid-run | Links are memory-only and vanish; the helper's own timeout still kills the browser. |

## Integration with glovo-home (follow-up)

Not part of this PR (glovo-home is being built in a separate change):

1. `glovo_link_start` MCP tool, link store, `/link/<code>` redemption,
   cookie, 303.
2. Spawning the helper: `node remote-login/src/main.mjs` with the env above,
   the helper token written to its stdin, its own process group; read the
   port line; proxy `/link/viewer*` to `/viewer*` and `/link/ws` to `/ws`
   adding `X-Remote-Login-Token` and passing `Origin` through unchanged.
3. Security headers on the proxied responses (the helper already sets them;
   glovo-home must not strip them).
4. URL elicitation when the client declared `elicitation.url`.
5. Session hot-reload on mtime (already in progress in the gateway change).
6. Audit: link minted / redeemed / expired, run result; principal id, never
   the code or the cookie.

## Open risks

- **The allowlist is derived, not observed.** The first real sign-in may
  need a host that is blocked. The audit lines will name it.
- **Bot detection.** reCAPTCHA scores headless Chrome low, so a challenge is
  likely; it can be solved through the stream, but it may be worse than on
  a desktop. If it is unusable, the next step is a headful window on the
  home server (`GLOVO_REMOTE_LOGIN_HEADLESS=0`), not stealth patches.
- **Google sign-in** likely refuses automated browsers. Email sign-in is the
  supported path.
- **claude.ai chat has no URL elicitation.** The user must tap the link in
  the tool result. On mobile the link opens in the system browser, which is
  what we want.
- **Leaked unused link.** Until the optional MCTL sign-in on the viewer is
  added, the 10-minute link is a bearer credential.
- **Session continuity.** glovo-skill replays the device and Perseus headers
  captured at sign-in; a session created in a headless mobile context may
  carry a different device profile than the old desktop one. This needs one
  real check after the first remote link.
