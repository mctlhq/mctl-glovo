# remote-login (prototype)

Links a Glovo session from any device. A fresh, locked-down Chrome runs on
the home server, the view of its single tab is streamed to a page, and the
user's taps and typing are sent back. The design, threat model and the
glovo-home integration are in
[docs/design/remote-glovo-login.md](../docs/design/remote-glovo-login.md).

This is a helper process, meant to be started and proxied by glovo-home. It
listens on 127.0.0.1 only. It runs Playwright's chrome-headless-shell and
refuses any Google Chrome build, because the home server periodically runs
`pkill -f "Google Chrome"`. After writing the session it writes
`<session>.reload`, the marker glovo-home watches.

```bash
npm ci --ignore-scripts
npx --no-install playwright-core install chromium-headless-shell
npm test        # REMOTE_LOGIN_REQUIRE_BROWSER=1 makes a missing browser fail instead of skip

# one run (the helper token goes on stdin, never in argv or env)
GLOVO_SESSION_PATH=/abs/path/session.json \
GLOVO_REMOTE_LOGIN_ORIGIN=https://glovo.example \
  node src/main.mjs <<< "$(openssl rand -hex 32)"
```

| File | Role |
|---|---|
| `src/allowlist.mjs` | navigation, subframe and subresource allowlists |
| `src/browser.mjs` | Chrome lifecycle, lockdown, screencast, input, capture, timeout and kill |
| `src/capture.mjs` | turning Glovo auth responses into a session |
| `src/session-write.mjs` | atomic 0600 write |
| `src/server.mjs` | loopback HTTP + WebSocket, origin and token checks, input validation |
| `src/main.mjs` | entry point and exit codes |
| `viewer/` | the page shown on the phone |

Tests use local fake pages and fixture responses only. No Glovo account is
involved.
