// Viewer for the remote Glovo login. It draws JPEG frames from the helper on
// a canvas and sends pointer, touch and keyboard input back. It keeps no
// state beyond the page and stores nothing.

const canvas = document.getElementById("screen");
const ctx = canvas.getContext("2d");
const statusEl = document.getElementById("status");
const ime = document.getElementById("ime");
const kbd = document.getElementById("kbd");

const STATUS_TEXT = {
  starting: "Starting the browser...",
  ready: "Sign in to Glovo below.",
  done: "Glovo is linked. You can close this page.",
  timeout: "The link expired. Ask for a new one.",
  error: "Something went wrong. Ask for a new link.",
};

let vp = null; // remote viewport in CSS px
let live = false;
const ws = new WebSocket(new URL("ws", location.href).href.replace(/^http/, "ws"));
ws.binaryType = "blob";

ws.addEventListener("open", () => {
  ws.send(JSON.stringify({
    t: "hello",
    viewport: {
      width: Math.round(window.innerWidth),
      height: Math.round(window.innerHeight - 90),
      dpr: window.devicePixelRatio || 1,
      mobile: matchMedia("(pointer: coarse)").matches,
    },
  }));
});

ws.addEventListener("message", async (ev) => {
  if (typeof ev.data === "string") {
    let m;
    try { m = JSON.parse(ev.data); } catch { return; }
    if (m.t === "meta") vp = { w: m.w, h: m.h };
    if (m.t === "status") {
      statusEl.textContent = STATUS_TEXT[m.state] || m.state;
      live = m.state === "ready";
    }
    return;
  }
  const bmp = await createImageBitmap(ev.data);
  if (canvas.width !== bmp.width || canvas.height !== bmp.height) {
    canvas.width = bmp.width;
    canvas.height = bmp.height;
  }
  ctx.drawImage(bmp, 0, 0);
  bmp.close();
});

ws.addEventListener("close", () => {
  live = false;
  if (!/linked|expired|wrong/.test(statusEl.textContent)) statusEl.textContent = "Disconnected. Reload to reconnect.";
});

function send(obj) {
  if (live && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(obj));
}

// Map a client point on the canvas to remote CSS pixels, clamped inside.
function toRemote(clientX, clientY) {
  const r = canvas.getBoundingClientRect();
  const w = vp ? vp.w : canvas.width;
  const h = vp ? vp.h : canvas.height;
  const x = Math.min(w, Math.max(0, ((clientX - r.left) / r.width) * w));
  const y = Math.min(h, Math.max(0, ((clientY - r.top) / r.height) * h));
  return { x, y };
}

const BUTTON = ["left", "middle", "right"];
const touches = new Map();

function touchPoints() {
  return [...touches.entries()].map(([id, p]) => ({ id, x: p.x, y: p.y }));
}

canvas.addEventListener("pointerdown", (e) => {
  e.preventDefault();
  canvas.setPointerCapture(e.pointerId);
  const p = toRemote(e.clientX, e.clientY);
  if (e.pointerType === "touch") {
    touches.set(e.pointerId % 32, p);
    send({ t: "touch", type: "touchStart", points: touchPoints() });
  } else {
    canvas.focus();
    send({ t: "mouse", type: "mousePressed", ...p, button: BUTTON[e.button] || "left", clickCount: e.detail || 1 });
  }
});
canvas.addEventListener("pointermove", (e) => {
  const p = toRemote(e.clientX, e.clientY);
  if (e.pointerType === "touch") {
    if (!touches.has(e.pointerId % 32)) return;
    touches.set(e.pointerId % 32, p);
    send({ t: "touch", type: "touchMove", points: touchPoints() });
  } else {
    send({ t: "mouse", type: "mouseMoved", ...p });
  }
});
function pointerEnd(e, cancel) {
  const p = toRemote(e.clientX, e.clientY);
  if (e.pointerType === "touch") {
    touches.delete(e.pointerId % 32);
    send({ t: "touch", type: cancel ? "touchCancel" : "touchEnd", points: touchPoints() });
  } else if (!cancel) {
    send({ t: "mouse", type: "mouseReleased", ...p, button: BUTTON[e.button] || "left", clickCount: e.detail || 1 });
  }
}
canvas.addEventListener("pointerup", (e) => pointerEnd(e, false));
canvas.addEventListener("pointercancel", (e) => pointerEnd(e, true));
canvas.addEventListener("wheel", (e) => {
  e.preventDefault();
  send({ t: "mouse", type: "mouseWheel", ...toRemote(e.clientX, e.clientY), deltaX: e.deltaX, deltaY: e.deltaY });
}, { passive: false });
canvas.addEventListener("contextmenu", (e) => e.preventDefault());

// Keyboard. Desktop: keydown on the canvas. Phone: a hidden textarea that
// brings up the soft keyboard; its beforeinput events become text and
// named keys, and it is cleared after each event so nothing accumulates.
const NAMED = new Set(["Enter", "Backspace", "Tab", "Escape", "Delete", "ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown"]);

function namedKey(key) {
  send({ t: "key", type: "keyDown", key });
  send({ t: "key", type: "keyUp", key });
}

canvas.addEventListener("keydown", (e) => {
  if (e.metaKey || e.ctrlKey || e.altKey) return;
  if (NAMED.has(e.key)) {
    e.preventDefault();
    namedKey(e.key);
  } else if (e.key.length === 1) {
    e.preventDefault();
    send({ t: "text", text: e.key });
  }
});
canvas.addEventListener("paste", (e) => {
  const text = (e.clipboardData && e.clipboardData.getData("text/plain")) || "";
  e.preventDefault();
  for (let i = 0; i < text.length; i += 256) send({ t: "text", text: text.slice(i, i + 256).replace(/[\u0000-\u001f\u007f]/g, "") });
});

kbd.addEventListener("click", () => ime.focus());
ime.addEventListener("beforeinput", (e) => {
  e.preventDefault();
  if (e.inputType === "insertText" || e.inputType === "insertReplacementText" || e.inputType === "insertFromPaste") {
    const text = (e.data || "").replace(/[\u0000-\u001f\u007f]/g, "");
    if (text) send({ t: "text", text: text.slice(0, 256) });
  } else if (e.inputType === "deleteContentBackward") {
    namedKey("Backspace");
  } else if (e.inputType === "deleteContentForward") {
    namedKey("Delete");
  } else if (e.inputType === "insertLineBreak" || e.inputType === "insertParagraph") {
    namedKey("Enter");
  }
  ime.value = "";
});
ime.addEventListener("keydown", (e) => {
  if (e.key === "Enter" || e.key === "Tab" || e.key === "Escape") {
    e.preventDefault();
    namedKey(e.key);
  }
});
