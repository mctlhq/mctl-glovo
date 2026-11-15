// The locked-down login browser: one fresh Chrome per run, a CDP screencast
// of the active tab, input forwarded from the viewer, and capture of the
// Glovo auth responses. Nothing here logs URLs beyond the host, frames,
// keystrokes or token contents.

import { spawnSync } from "node:child_process";
import { hostOf, isAllowed } from "./allowlist.mjs";
import {
  buildSession,
  isAuthResponse,
  isProfileResponse,
  locationFromHeaders,
  mergeLocation,
  normalizeCredentials,
} from "./capture.mjs";

export const DEFAULT_START_URL = "https://glovoapp.com/en/login?returnPath=%2Fen%2Fprofile";
export const DEFAULT_API_ORIGIN = "https://api.glovoapp.com";

const LAUNCH_ARGS = [
  "--disable-extensions",
  "--disable-sync",
  "--no-first-run",
  "--no-default-browser-check",
  "--disable-background-networking",
  "--disable-component-update",
  "--disable-features=Translate,MediaRouter,AutofillServerCommunication",
  "--password-store=basic",
  "--use-mock-keychain",
];

// The home server runs a periodic `pkill -f "Google Chrome"`. pkill -f
// matches the whole command line, so any browser whose executable path or
// arguments contain this string would be killed mid-login. Both the system
// Chrome ("Google Chrome.app") and Playwright's bundled full Chromium
// ("Google Chrome for Testing.app") match. Playwright's chrome-headless-shell
// does not, and it is the default here.
export const KILL_PATTERN = "Google Chrome";

// assertNotKillable refuses a launch whose executable would be hit by the
// host's kill pattern, instead of starting a run that can die halfway.
export function assertNotKillable({ channel, executablePath }) {
  if (channel) throw new Error(`browser channel "${channel}" runs a Google Chrome build, which the host kills; use the default headless shell`);
  if (executablePath && executablePath.includes(KILL_PATTERN)) throw new Error("browser executable path matches the host's kill pattern");
}

// assertRunningNotKillable checks the live command line of the browser
// process. A missing pid or an unreadable command line is a refusal.
export function assertRunningNotKillable(pid, read = commandLine) {
  if (!pid) throw new Error("browser pid unknown");
  if (read(pid).includes(KILL_PATTERN)) throw new Error("browser command line matches the host's kill pattern");
}

// commandLine returns the full command line of a process as `ps` shows it
// (what pkill -f matches against), or throws if it cannot be read.
export function commandLine(pid) {
  const r = spawnSync("ps", ["-ww", "-o", "command=", "-p", String(pid)], { encoding: "utf8" });
  if (r.status !== 0 || !r.stdout.trim()) throw new Error("could not read the browser command line");
  return r.stdout.trim();
}

const VIEWPORT_LIMITS = { minW: 320, maxW: 1600, minH: 480, maxH: 1600, maxDpr: 3 };

// clampViewport turns the viewer's reported screen into safe context
// options. A phone gets isMobile + hasTouch so Glovo serves its mobile
// layout and touch events work.
export function clampViewport(v = {}) {
  const num = (x, d) => (Number.isFinite(x) ? x : d);
  const width = Math.round(Math.min(VIEWPORT_LIMITS.maxW, Math.max(VIEWPORT_LIMITS.minW, num(v.width, 1280))));
  const height = Math.round(Math.min(VIEWPORT_LIMITS.maxH, Math.max(VIEWPORT_LIMITS.minH, num(v.height, 800))));
  const deviceScaleFactor = Math.min(VIEWPORT_LIMITS.maxDpr, Math.max(1, num(v.dpr, 1)));
  const mobile = v.mobile === true;
  return { width, height, deviceScaleFactor, mobile };
}

// LoginBrowser runs exactly one login attempt. Call start(), forward input
// with the input* methods, and await `result` (resolves with
// { status: "captured", session } or { status: "timeout" | "error" }).
// close() is idempotent and always kills the browser process.
export class LoginBrowser {
  constructor({
    allowlist,
    audit,
    onFrame = () => {},
    onMeta = () => {},
    timeoutMs = 10 * 60_000,
    startUrl = DEFAULT_START_URL,
    apiOrigin = DEFAULT_API_ORIGIN,
    channel,
    executablePath,
    headless = true,
    viewport = {},
    profileSettleMs = 15_000,
    launcher,
  }) {
    if (!allowlist) throw new Error("allowlist is required");
    if (!isAllowed(allowlist, "top", startUrl)) throw new Error("start URL is not on the navigation allowlist");
    this.allowlist = allowlist;
    this.audit = audit;
    this.onFrame = onFrame;
    this.onMeta = onMeta;
    this.timeoutMs = timeoutMs;
    this.startUrl = startUrl;
    this.apiOrigin = apiOrigin;
    this.channel = channel;
    this.executablePath = executablePath;
    this.headless = headless;
    this.viewport = clampViewport(viewport);
    this.profileSettleMs = profileSettleMs;
    this.launcher = launcher;
    this.browser = null;
    this.browserPid = null;
    this.activePage = null;
    this.cdp = null;
    this.closed = false;
    this.blocked = 0;
    this.result = new Promise((resolve) => {
      this._resolve = resolve;
    });
  }

  async start() {
    const launcher = this.launcher ?? (await import("playwright-core")).chromium;
    // Without a channel and headless, Playwright launches chrome-headless-shell.
    // A headful window needs full Chromium, whose bundled build is named
    // "Google Chrome for Testing" and is therefore refused below unless a
    // custom executable outside the kill pattern is given.
    assertNotKillable({ channel: this.channel, executablePath: this.executablePath });
    this.browser = await launcher.launch({
      executablePath: this.executablePath,
      headless: this.headless,
      args: LAUNCH_ARGS,
      // Playwright launches with a fresh temporary profile and removes it on
      // close; nothing is reused between runs.
    });
    this.browserPid = await this._readBrowserPid();
    // The authoritative check: the real command line of the running
    // browser. An unreadable one is a refusal too, not a pass.
    try {
      assertRunningNotKillable(this.browserPid);
    } catch (e) {
      await this.close();
      throw e;
    }
    this._timer = setTimeout(() => this._finish({ status: "timeout" }), this.timeoutMs);
    this._timer.unref?.();
    // An exit we did not ask for (the host's pkill, a crash, OOM) is a failed
    // login: nothing is written and the viewer is told the browser closed.
    this.browser.on("disconnected", () => this._finish({ status: "error", reason: "browser_exited" }));

    const v = this.viewport;
    this.context = await this.browser.newContext({
      viewport: { width: v.width, height: v.height },
      deviceScaleFactor: v.deviceScaleFactor,
      isMobile: v.mobile,
      hasTouch: v.mobile,
      acceptDownloads: false,
      serviceWorkers: "block", // otherwise a worker could fetch outside route()
      permissions: [],
      bypassCSP: false,
      javaScriptEnabled: true,
    });
    this.audit("browser_started", { viewport: `${v.width}x${v.height}${v.mobile ? "m" : ""}` });

    await this._installLockdown();
    this._installCapture();

    this.context.on("page", (page) => {
      this._adoptPage(page).catch(() => {});
    });
    const page = await this.context.newPage();
    // Start the screencast before the first navigation: Chrome only emits
    // frames when something repaints, so a stream started after a static
    // page has painted could stay empty.
    await this._adoptPage(page);
    await this._goto(page, this.startUrl);
    return this;
  }

  async _readBrowserPid() {
    try {
      const s = await this.browser.newBrowserCDPSession();
      const info = await s.send("SystemInfo.getProcessInfo");
      await s.detach().catch(() => {});
      const b = info.processInfo.find((p) => p.type === "browser");
      return b ? b.id : null;
    } catch {
      return null;
    }
  }

  async _installLockdown() {
    await this.context.route("**/*", (route) => {
      const req = route.request();
      const url = req.url();
      let kind = "sub";
      let allowed = false;
      try {
        if (req.isNavigationRequest()) {
          // A popup's first navigation is issued before its frame exists;
          // it is a new top-level page, so it gets the strictest list.
          let frame = null;
          try {
            frame = req.frame();
          } catch {
            frame = null;
          }
          kind = frame?.parentFrame() ? "frame" : "top";
        }
        allowed = isAllowed(this.allowlist, kind, url);
      } catch {
        allowed = false; // any doubt is a block
      }
      if (allowed) return route.continue();
      this.blocked += 1;
      this.audit("blocked", { kind, host: hostOf(url) });
      return route.abort("blockedbyclient");
    });
    // route() does not see WebSockets; they get the "sub" list.
    await this.context.routeWebSocket(/.*/, (ws) => {
      const url = ws.url();
      if (isAllowed(this.allowlist, "sub", url)) return ws.connectToServer();
      this.blocked += 1;
      this.audit("blocked", { kind: "ws", host: hostOf(url) });
      return ws.close({ code: 1008, reason: "blocked" });
    });
  }

  _installCapture() {
    let credentials = null;
    let customer = null;
    let location = null;
    this._captureState = () => ({ credentials, customer, location });

    this.context.on("request", (req) => {
      let origin;
      try {
        origin = new URL(req.url()).origin;
      } catch {
        return;
      }
      if (origin !== this.apiOrigin) return;
      location = mergeLocation(location, locationFromHeaders(req.headers()));
    });

    this.context.on("response", async (res) => {
      const url = res.url();
      if (isAuthResponse(url, this.apiOrigin) && res.ok()) {
        let body;
        try {
          body = await res.json();
        } catch {
          return; // not JSON: not a token response
        }
        const c = normalizeCredentials(body);
        if (c && !credentials) {
          credentials = c;
          this.audit("token_seen");
          this._afterToken().catch(() => this._finish({ status: "error", reason: "post_capture" }));
        }
      } else if (isProfileResponse(url, this.apiOrigin) && res.ok()) {
        try {
          customer = await res.json();
        } catch {
          // a partial profile read is not fatal
        }
      }
    });
  }

  // After the token arrives, give the web app a short window to load the
  // profile (customer id, location headers), then finish. The session is
  // complete without the profile; it is only enrichment.
  async _afterToken() {
    const deadline = Date.now() + this.profileSettleMs;
    while (!this.closed && !this._captureState().customer && Date.now() < deadline) {
      await new Promise((r) => setTimeout(r, 250));
    }
    if (this.closed) return;
    const { credentials, customer, location } = this._captureState();
    this._finish({ status: "captured", session: buildSession({ credentials, customer, location }) });
  }

  // _adoptPage is called both from the context "page" event and directly
  // for the first page; both callers get the same promise, so start() really
  // waits for the screencast to be running.
  _adoptPage(page) {
    this._adopted ??= new WeakMap();
    if (!this._adopted.has(page)) this._adopted.set(page, this._adoptPageOnce(page));
    return this._adopted.get(page);
  }

  async _adoptPageOnce(page) {
    // A watchdog in addition to route(): if the main frame ever lands on a
    // URL outside the list (a scheme route() never sees, for example),
    // the page is closed rather than shown.
    page.on("framenavigated", (frame) => {
      if (frame !== page.mainFrame()) return;
      const url = frame.url();
      if (url === "about:blank" || url.startsWith("chrome-error://")) return;
      if (!isAllowed(this.allowlist, "top", url)) {
        this.audit("closed_offlist_page", { host: hostOf(url) });
        page.close().catch(() => {});
      }
    });
    page.on("dialog", (d) => d.dismiss().catch(() => {}));
    page.on("close", () => {
      if (this.activePage === page) {
        const rest = this.context.pages().filter((p) => p !== page && !p.isClosed());
        if (rest.length) this._screencast(rest[rest.length - 1]).catch(() => {});
      }
    });
    await this._screencast(page);
  }

  async _goto(page, url) {
    try {
      await page.goto(url, { waitUntil: "domcontentloaded", timeout: 30_000 });
    } catch {
      this.audit("start_navigation_failed", { host: hostOf(url) });
    }
  }

  // The newest page (a social sign-in popup, for example) becomes the one
  // streamed and controlled; when it closes, the previous one returns.
  async _screencast(page) {
    if (this.closed) return;
    if (this.cdp) {
      await this.cdp.send("Page.stopScreencast").catch(() => {});
      await this.cdp.detach().catch(() => {});
    }
    this.activePage = page;
    const cdp = await this.context.newCDPSession(page);
    this.cdp = cdp;
    cdp.on("Page.screencastFrame", (f) => {
      cdp.send("Page.screencastFrameAck", { sessionId: f.sessionId }).catch(() => {});
      if (this.cdp !== cdp || this.closed) return;
      const m = f.metadata;
      this._emit(Buffer.from(f.data, "base64"), m.deviceWidth, m.deviceHeight);
    });
    const v = this.viewport;
    await cdp.send("Page.startScreencast", {
      format: "jpeg",
      quality: 60,
      maxWidth: Math.round(v.width * v.deviceScaleFactor),
      maxHeight: Math.round(v.height * v.deviceScaleFactor),
      everyNthFrame: 1,
    });
    this._dirty = true;
    page.on("load", () => {
      this._dirty = true;
    });
    if (!this._keyframeTimer) {
      this._keyframeTimer = setInterval(() => this._keyframe().catch(() => {}), 500);
      this._keyframeTimer.unref?.();
    }
  }

  _emit(jpeg, w, h) {
    this._lastFrameAt = Date.now();
    this._dirty = false;
    this.onMeta({ w, h });
    this.onFrame(jpeg);
  }

  // Headless Chrome emits screencast frames only when the compositor
  // produces one, and after a navigation it sometimes produces none for a
  // static page. When the view may have changed (navigation, input) and no
  // frame came for a moment, take one JPEG screenshot instead, so the
  // viewer never stares at a stale or empty canvas.
  async _keyframe() {
    if (this.closed || !this.cdp || !this._dirty) return;
    if (Date.now() - (this._lastFrameAt ?? 0) < 400) return;
    const cdp = this.cdp;
    this._dirty = false;
    const shot = await cdp.send("Page.captureScreenshot", { format: "jpeg", quality: 60 });
    if (this.cdp !== cdp || this.closed) return;
    this._emit(Buffer.from(shot.data, "base64"), this.viewport.width, this.viewport.height);
  }

  _touched() {
    this._dirty = true;
  }

  // ---- input forwarding. Coordinates are CSS pixels of the viewport; the
  // caller (server.mjs) has already validated types and ranges.

  async inputMouse({ type, x, y, button = "left", clickCount = 1, deltaX = 0, deltaY = 0 }) {
    if (!this.cdp) return;
    this._touched();
    const params = { type, x, y, button: type === "mouseMoved" || type === "mouseWheel" ? "none" : button, clickCount };
    if (type === "mouseWheel") Object.assign(params, { deltaX, deltaY });
    await this.cdp.send("Input.dispatchMouseEvent", params);
  }

  async inputTouch({ type, points }) {
    if (!this.cdp) return;
    this._touched();
    await this.cdp.send("Input.dispatchTouchEvent", { type, touchPoints: points });
  }

  async inputKey({ type, key, code, keyCode }) {
    if (!this.cdp) return;
    this._touched();
    await this.cdp.send("Input.dispatchKeyEvent", {
      type,
      key,
      code,
      windowsVirtualKeyCode: keyCode,
      nativeVirtualKeyCode: keyCode,
    });
  }

  async inputText(text) {
    if (!this.cdp) return;
    this._touched();
    await this.cdp.send("Input.insertText", { text });
  }

  _finish(outcome) {
    if (this.closed) return;
    this.closed = true;
    clearTimeout(this._timer);
    this.audit("finished", { reason: outcome.status, count: this.blocked });
    this._resolve(outcome);
    this.close().catch(() => {});
  }

  // close() asks the browser to exit, then SIGKILLs its process if it is
  // still alive after a grace period. Playwright deletes the temporary
  // profile when the browser exits.
  async close() {
    this.closed = true;
    clearTimeout(this._timer);
    clearInterval(this._keyframeTimer);
    const b = this.browser;
    if (!b) return;
    this.browser = null;
    await Promise.race([b.close().catch(() => {}), new Promise((r) => setTimeout(r, 5_000))]);
    if (this.browserPid && pidAlive(this.browserPid)) {
      try {
        process.kill(this.browserPid, "SIGKILL");
      } catch {
        // already gone
      }
    }
  }
}

export function pidAlive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch (e) {
    return e.code === "EPERM";
  }
}
