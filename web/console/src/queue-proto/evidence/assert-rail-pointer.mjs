// Real-pointer rail reachability assertion at 390x844. Drives headless
// Chrome over CDP and opens/closes the view rail with actual
// Input.dispatchMouseEvent clicks — the real hit-test path. A DOM .click()
// call bypasses pointer occlusion entirely, so this is the check that fails
// when the narrow-screen rail overlay covers the only close control.
//
// Contract (rail open at <=900px):
//   1. closed -> open -> closed works through real pointer input.
//   2. While open, the topmost element at the toggle's center is the toggle
//      itself or a descendant (elementFromPoint), i.e. the overlay does not
//      cover the control's clickable space.
//   3. While open, the toggle rect does not overlap the first rail content
//      rect (.qp-rail-h "views").
//
// Usage:
//   npx vite preview --config vite.proto.config.ts --port 5199 --strictPort &
//   node src/queue-proto/evidence/assert-rail-pointer.mjs [baseUrl]

import { spawn } from "node:child_process";

const CHROME = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
const CDP_PORT = 9334;
const BASE = process.argv[2] ?? "http://localhost:5199/queue-proto.html";
const PROFILE = "/tmp/queue-proto-chrome-profile-rail";

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function devtoolsTarget() {
  for (let i = 0; i < 100; i++) {
    try {
      const res = await fetch(`http://localhost:${CDP_PORT}/json`);
      const targets = await res.json();
      const page = targets.find((t) => t.type === "page");
      if (page) {
        return page.webSocketDebuggerUrl;
      }
    } catch {
      // chrome not up yet
    }
    await sleep(200);
  }
  throw new Error("devtools endpoint never came up");
}

function cdp(wsUrl) {
  const ws = new WebSocket(wsUrl);
  let seq = 0;
  const pending = new Map();
  const listeners = new Map();
  ws.addEventListener("message", (event) => {
    const msg = JSON.parse(event.data);
    if (msg.id && pending.has(msg.id)) {
      pending.get(msg.id)(msg);
      pending.delete(msg.id);
    } else if (msg.method && listeners.has(msg.method)) {
      for (const fn of listeners.get(msg.method)) {
        fn(msg.params);
      }
    }
  });
  const ready = new Promise((resolve) => ws.addEventListener("open", resolve));
  return {
    ready,
    on(method, fn) {
      if (!listeners.has(method)) {
        listeners.set(method, []);
      }
      listeners.get(method).push(fn);
    },
    send(method, params = {}) {
      const id = ++seq;
      return new Promise((resolve, reject) => {
        pending.set(id, (msg) => (msg.error ? reject(new Error(JSON.stringify(msg.error))) : resolve(msg.result)));
        ws.send(JSON.stringify({ id, method, params }));
      });
    },
    close: () => ws.close(),
  };
}

const chrome = spawn(
  CHROME,
  ["--headless=new", "--disable-gpu", `--remote-debugging-port=${CDP_PORT}`, `--user-data-dir=${PROFILE}`, "--no-first-run", "about:blank"],
  { stdio: "ignore" },
);
process.on("exit", () => chrome.kill("SIGTERM"));

const client = cdp(await devtoolsTarget());
await client.ready;
await client.send("Page.enable");
await client.send("Runtime.enable");

let loadedResolve = null;
client.on("Page.loadEventFired", () => loadedResolve?.());

async function evalJs(expression) {
  const res = await client.send("Runtime.evaluate", { expression, returnByValue: true, awaitPromise: true });
  if (res.exceptionDetails) {
    throw new Error(`eval failed: ${JSON.stringify(res.exceptionDetails).slice(0, 300)}`);
  }
  return res.result?.value;
}

async function navigate(url) {
  try {
    await evalJs(`localStorage.clear()`);
  } catch {
    // first navigation happens from about:blank — storage access may fail
  }
  const loaded = new Promise((r) => (loadedResolve = r));
  await client.send("Page.navigate", { url });
  await Promise.race([loaded, sleep(15000)]);
  await sleep(400); // let React commit + effects settle
}

// Real input pipeline: the click goes through Chrome's hit testing, so an
// overlay covering the control swallows it exactly like a user's tap.
async function pointerClick(x, y) {
  await client.send("Input.dispatchMouseEvent", { type: "mouseMoved", x, y });
  await client.send("Input.dispatchMouseEvent", { type: "mousePressed", x, y, button: "left", buttons: 1, clickCount: 1 });
  await client.send("Input.dispatchMouseEvent", { type: "mouseReleased", x, y, button: "left", buttons: 1, clickCount: 1 });
  await sleep(250); // let React process the click
}

const MEASURE = `(() => {
  const toggle = document.getElementById("qp-rail-toggle");
  const rail = document.getElementById("qp-rail");
  const head = document.querySelector(".qp-head");
  const firstContent = rail?.querySelector(".qp-rail-h");
  const tr = toggle?.getBoundingClientRect();
  const cx = tr ? tr.left + tr.width / 2 : 0;
  const cy = tr ? tr.top + tr.height / 2 : 0;
  const at = tr ? document.elementFromPoint(cx, cy) : null;
  const rr = rail?.getBoundingClientRect();
  const hr = head?.getBoundingClientRect();
  const fr = firstContent?.getBoundingClientRect();
  const rect = (r) => (r ? { left: r.left, top: r.top, width: r.width, height: r.height, right: r.right, bottom: r.bottom } : null);
  return {
    aria_expanded: toggle?.getAttribute("aria-expanded") ?? null,
    toggle_rect: rect(tr),
    toggle_center: { x: cx, y: cy },
    element_at_toggle_center: at ? { tag: at.tagName, id: at.id || null, class: at.className || null, is_toggle: at.closest("#qp-rail-toggle") !== null } : null,
    rail_rect: rect(rr),
    head_rect: rect(hr),
    first_rail_content_rect: rect(fr),
    overlap: !!(tr && fr && tr.left < fr.right && tr.right > fr.left && tr.top < fr.bottom && tr.bottom > fr.top),
  };
})()`;

let failures = 0;
const check = (name, ok, detail) => {
  if (ok) {
    console.log(`PASS ${name} (${detail})`);
  } else {
    failures += 1;
    console.log(`FAIL ${name} (${detail})`);
  }
};

await client.send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 1, mobile: true });
await navigate(`${BASE}?view=backlog&layout=list`);

const closed = await evalJs(MEASURE);
console.log("closed state:", JSON.stringify(closed));
check("toggle hit-testable before open", closed.element_at_toggle_center?.is_toggle === true, JSON.stringify(closed.element_at_toggle_center));

// closed -> open via a real pointer click on the toggle
await pointerClick(closed.toggle_center.x, closed.toggle_center.y);
const open = await evalJs(MEASURE);
console.log("open state:", JSON.stringify(open));
check("pointer click opens the rail", open.aria_expanded === "true", `aria-expanded=${open.aria_expanded}`);
check("rail overlay is painted", open.rail_rect !== null && open.rail_rect.width > 0 && open.rail_rect.height > 0, `rail=${JSON.stringify(open.rail_rect)}`);
check(
  "toggle stays the topmost hit target while open",
  open.element_at_toggle_center?.is_toggle === true,
  `elementFromPoint=${JSON.stringify(open.element_at_toggle_center)}`,
);
// Anchor checks must see their own subject: a missing .qp-rail-h or .qp-head
// is a failure, never a vacuous pass.
check(
  "header anchor present",
  open.head_rect !== null && open.head_rect.height > 0,
  `head=${JSON.stringify(open.head_rect)}`,
);
check(
  "first rail content anchor present",
  open.first_rail_content_rect !== null,
  `first=${JSON.stringify(open.first_rail_content_rect)}`,
);
check(
  "rail top clears the header bottom",
  open.rail_rect !== null && open.head_rect !== null && open.rail_rect.top >= open.head_rect.bottom,
  `rail.top=${open.rail_rect?.top} head.bottom=${open.head_rect?.bottom}`,
);
check(
  "toggle rect does not overlap first rail content",
  open.toggle_rect !== null && open.first_rail_content_rect !== null && open.overlap === false,
  `toggle=${JSON.stringify(open.toggle_rect)} first=${JSON.stringify(open.first_rail_content_rect)}`,
);

// open -> closed via a second real pointer click on the same control
await pointerClick(open.toggle_center.x, open.toggle_center.y);
const after = await evalJs(MEASURE);
console.log("after close click:", JSON.stringify(after));
check("pointer click closes the rail", after.aria_expanded === "false", `aria-expanded=${after.aria_expanded}`);

await client.close();
chrome.kill("SIGTERM");
console.log(failures === 0 ? "ALL RAIL POINTER ASSERTIONS PASSED" : `${failures} rail pointer assertion(s) FAILED`);
process.exitCode = failures === 0 ? 0 : 1;
