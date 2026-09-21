// Peek-panel reachability assertion (AC A-1): while the non-modal detail
// panel is open, every board column and every list row must stay
// pointer-reachable — the fixed overlay must never permanently cover a
// clickable target. jsdom cannot answer this (no layout, no hit-testing) and
// fireEvent.click bypasses elementFromPoint entirely, so this runs in real
// Chrome over CDP with real Input.dispatchMouseEvent clicks.
//
// Usage (from web/console):
//   node src/queue-proto/evidence/assert-peek-reach.mjs [baseUrl]
//
// Same serving rules as assert-card-fit.mjs: no argument = self-built,
// self-hosted preview on an ephemeral port; an explicit baseUrl is validated
// to serve the fixture page.
//
// Exit 0 = every target reachable; exit 2 = a covered column/row or a failed
// retarget.

import { spawn, spawnSync } from "node:child_process";
import { createServer } from "node:net";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const CHROME = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
const CDP_PORT = 9335;
const PROFILE = "/tmp/queue-proto-peekreach-profile";
const CONSOLE_DIR = resolve(dirname(fileURLToPath(import.meta.url)), "../../..");

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function freePort() {
  const srv = createServer();
  await new Promise((r) => srv.listen(0, "127.0.0.1", r));
  const { port } = srv.address();
  await new Promise((r) => srv.close(r));
  return port;
}

let BASE = process.argv[2];
let preview = null;
if (!BASE) {
  // Always rebuild — a stale dist-proto from an earlier checkout must never
  // be what this gate measures.
  const build = spawnSync("npx", ["vite", "build", "--config", "vite.proto.config.ts"], { cwd: CONSOLE_DIR, stdio: "inherit" });
  if (build.status !== 0) {
    console.error("vite build --config vite.proto.config.ts failed");
    process.exit(2);
  }
  const port = await freePort();
  preview = spawn("npx", ["vite", "preview", "--config", "vite.proto.config.ts", "--host", "127.0.0.1", "--port", String(port), "--strictPort"], { cwd: CONSOLE_DIR, stdio: "ignore" });
  process.on("exit", () => preview.kill("SIGTERM"));
  BASE = `http://127.0.0.1:${port}/queue-proto.html`;
}
{
  let html = null;
  for (let i = 0; i < 100 && html === null; i++) {
    try {
      const res = await fetch(BASE);
      if (res.ok) {
        html = await res.text();
      }
    } catch {
      // preview still starting
    }
    if (html === null) {
      await sleep(200);
    }
  }
  if (html === null || !html.includes('id="queue-proto-root"')) {
    console.error(`${BASE} is not the queue-proto fixture page — refusing to measure`);
    process.exit(2);
  }
}

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
    send(method, params = {}) {
      const id = ++seq;
      return new Promise((resolve, reject) => {
        pending.set(id, (msg) => (msg.error ? reject(new Error(`${method}: ${msg.error.message}`)) : resolve(msg.result)));
        ws.send(JSON.stringify({ id, method, params }));
      });
    },
    on(method, fn) {
      if (!listeners.has(method)) {
        listeners.set(method, []);
      }
      listeners.get(method).push(fn);
    },
    close: () => ws.close(),
  };
}

const chrome = spawn(
  CHROME,
  [`--remote-debugging-port=${CDP_PORT}`, "--headless=new", "--hide-scrollbars", `--user-data-dir=${PROFILE}`, "about:blank"],
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
    // about:blank has no storage
  }
  const loaded = new Promise((r) => (loadedResolve = r));
  await client.send("Page.navigate", { url });
  await Promise.race([loaded, sleep(15000)]);
  await sleep(600); // React commit + effects + virtual-list measurement
}

async function clickAt(x, y) {
  await client.send("Input.dispatchMouseEvent", { type: "mousePressed", x, y, button: "left", clickCount: 1 });
  await client.send("Input.dispatchMouseEvent", { type: "mouseReleased", x, y, button: "left", clickCount: 1 });
  await sleep(300);
}

// Center point of the first on-screen element matching sel, or null.
const centerOf = (sel) => `(() => {
  for (const el of document.querySelectorAll(${JSON.stringify(sel)})) {
    const r = el.getBoundingClientRect();
    if (r.width > 0 && r.bottom > 0 && r.top < innerHeight && r.right > 0 && r.left < innerWidth) {
      return { x: r.left + r.width / 2, y: r.top + Math.min(r.height / 2, 12) };
    }
  }
  return null;
})()`;

function fail(msg) {
  console.error(msg);
  process.exitCode = 2;
}

// --- Scenario 1: wide board — every column must be pointer-reachable while
// the panel is open. For each column, sweep .qp-board scrollLeft and hit-test
// card points; a column counts as reachable if any card point resolves to a
// .qp-card at any scroll position.
async function checkBoardReachable(width, height) {
  await client.send("Emulation.setDeviceMetricsOverride", { width, height, deviceScaleFactor: 1, mobile: false });
  await navigate(`${BASE}?view=all&layout=board`);
  const first = await evalJs(centerOf(".qp-card"));
  if (!first) {
    fail(`${width}x${height} board: no card to open`);
    return;
  }
  await clickAt(first.x, first.y);
  if (!(await evalJs(`!!document.querySelector(".qp-drawer")`))) {
    fail(`${width}x${height} board: click did not open the panel`);
    return;
  }
  const res = await evalJs(`(async () => {
    const frame = () => new Promise((r) => requestAnimationFrame(r));
    const board = document.querySelector(".qp-board");
    const cols = [...document.querySelectorAll(".qp-col")];
    const reachable = new Array(cols.length).fill(false);
    const step = 160;
    for (let left = 0; left <= board.scrollWidth; left += step) {
      board.scrollLeft = left;
      await frame(); await frame();
      cols.forEach((col, i) => {
        if (reachable[i]) return;
        for (const card of col.querySelectorAll(".qp-card")) {
          const r = card.getBoundingClientRect();
          for (const fx of [0.5, 0.2, 0.8]) {
            const x = r.left + r.width * fx;
            const y = r.top + Math.min(r.height / 2, 20);
            if (x < 0 || y < 0 || x > innerWidth || y > innerHeight) continue;
            const hit = document.elementFromPoint(x, y);
            if (hit && hit.closest(".qp-card") === card) { reachable[i] = true; return; }
          }
        }
      });
    }
    board.scrollLeft = 0;
    await frame();
    const heads = cols.map((c) => c.querySelector(".qp-col-head")?.textContent?.trim() ?? "?");
    return { reachable, heads, drawer: !!document.querySelector(".qp-drawer") };
  })()`);
  const covered = res.reachable.map((ok, i) => (ok ? null : res.heads[i])).filter(Boolean);
  if (covered.length > 0) {
    fail(`${width}x${height} board: panel open, columns never pointer-reachable: ${covered.join(", ")}`);
  } else {
    console.log(`${width}x${height} board: all ${res.reachable.length} columns reachable with panel open`);
  }
  // End-to-end: scroll the last column into view, then a real click on its
  // card must retarget the open panel.
  const target = await evalJs(`(async () => {
    const frame = () => new Promise((r) => requestAnimationFrame(r));
    const board = document.querySelector(".qp-board");
    board.scrollLeft = board.scrollWidth;
    await frame(); await frame();
    const col = [...document.querySelectorAll(".qp-col")].at(-1);
    const card = col.querySelector(".qp-card");
    const r = card.getBoundingClientRect();
    return { id: card.dataset.taskId, x: r.left + r.width / 2, y: r.top + Math.min(r.height / 2, 20), vw: innerWidth };
  })()`);
  if (target.x > target.vw || target.x < 0) {
    fail(`${width}x${height} board: last column card not in view after reachability sweep (x=${target.x} vw=${target.vw})`);
    return;
  }
  const before = await evalJs(`document.querySelector(".qp-drawer")?.getAttribute("aria-label")`);
  await clickAt(target.x, target.y);
  const after = await evalJs(`document.querySelector(".qp-drawer")?.getAttribute("aria-label")`);
  if (!after?.includes(`task ${target.id}`)) {
    fail(`${width}x${height} board: click on last-column card did not retarget panel (${before} -> ${after})`);
  } else {
    console.log(`${width}x${height} board: click retargeted panel to task ${target.id}`);
  }
}

// --- Scenario 2: wide list — rows keep a clickable area while the panel is
// open (the panel reserves its width, so every row is fully left of it).
async function checkListReachable(width, height) {
  await client.send("Emulation.setDeviceMetricsOverride", { width, height, deviceScaleFactor: 1, mobile: false });
  await navigate(`${BASE}?view=all&layout=list`);
  const first = await evalJs(centerOf(".qp-row"));
  if (!first) {
    fail(`${width}x${height} list: no row to open`);
    return;
  }
  await clickAt(first.x, first.y);
  if (!(await evalJs(`!!document.querySelector(".qp-drawer")`))) {
    fail(`${width}x${height} list: click did not open the panel`);
    return;
  }
  // A row counts as covered only if no point inside its VISIBLE band (row
  // clipped by its own scroller) hit-tests back to it — partial clipping by
  // the list viewport edge is not panel occlusion.
  const bad = await evalJs(`(() => {
    const list = document.querySelector(".qp-list");
    const lb = list.getBoundingClientRect();
    const out = [];
    for (const row of document.querySelectorAll(".qp-row")) {
      const r = row.getBoundingClientRect();
      const visTop = Math.max(r.top, lb.top, 0);
      const visBot = Math.min(r.bottom, lb.bottom, innerHeight);
      if (visBot - visTop < 4) continue;
      const x = r.left + Math.min(r.width / 2, 200);
      const y = (visTop + visBot) / 2;
      const hit = document.elementFromPoint(x, y);
      if (!hit || hit.closest(".qp-row") !== row) out.push(row.dataset.taskId);
    }
    return out;
  })()`);
  if (bad.length > 0) {
    fail(`${width}x${height} list: ${bad.length} visible rows covered by panel (ids ${bad.slice(0, 5).join(",")})`);
  } else {
    console.log(`${width}x${height} list: all visible rows hit-test clean with panel open`);
  }
}

// --- Scenario 3: narrow (bottom sheet) — at least one row/card must stay
// tappable in the band above the sheet, and a click must retarget.
async function checkNarrow(width, height, layout) {
  await client.send("Emulation.setDeviceMetricsOverride", { width, height, deviceScaleFactor: 1, mobile: true });
  await navigate(`${BASE}?view=all&layout=${layout}`);
  const sel = layout === "board" ? ".qp-card" : ".qp-row";
  const first = await evalJs(centerOf(sel));
  if (!first) {
    fail(`${width}x${height} ${layout}: nothing to open`);
    return;
  }
  await clickAt(first.x, first.y);
  if (!(await evalJs(`!!document.querySelector(".qp-drawer")`))) {
    fail(`${width}x${height} ${layout}: click did not open the panel`);
    return;
  }
  // Any visible target above the sheet that hit-tests to itself counts.
  // Scroll every real scroller (.qp-list / .qp-col-cards / document) so rows
  // clipped by their own viewport can still be reached.
  const res = await evalJs(`(async () => {
    const frame = () => new Promise((r) => requestAnimationFrame(r));
    const sel = ${JSON.stringify(sel)};
    const scrollers = [document.scrollingElement, ...document.querySelectorAll(".qp-list, .qp-col-cards")];
    const reachable = new Set();
    for (let top = 0; top <= 4000; top += 160) {
      for (const s of scrollers) {
        if (s) s.scrollTop = top;
      }
      await frame(); await frame();
      for (const el of document.querySelectorAll(sel)) {
        if (reachable.has(el.dataset.taskId)) continue;
        const r = el.getBoundingClientRect();
        // sample inside the part of the element that is actually rendered
        const visTop = Math.max(r.top, 0), visBot = Math.min(r.bottom, innerHeight);
        if (visBot - visTop < 6) continue;
        for (const fx of [0.5, 0.2, 0.8]) {
          const x = r.left + r.width * fx;
          const y = (visTop + visBot) / 2;
          if (x < 0 || x > innerWidth) continue;
          const hit = document.elementFromPoint(x, y);
          if (hit && (hit === el || hit.closest(sel) === el)) {
            reachable.add(el.dataset.taskId);
            break;
          }
        }
      }
      if (reachable.size >= 2) break;
    }
    return [...reachable];
  })()`);
  if (res.length === 0) {
    fail(`${width}x${height} ${layout}: no ${sel} tappable while the sheet is open`);
    return;
  }
  console.log(`${width}x${height} ${layout}: ${res.length} targets tappable above the sheet`);
}

await checkBoardReachable(1512, 982); // MacBook 14" default — was uncovered
await checkBoardReachable(1440, 900);
await checkListReachable(1440, 900);
await checkNarrow(360, 640, "list");
await checkNarrow(390, 844, "list");
await checkNarrow(360, 640, "board");

await client.close();
chrome.kill("SIGTERM");
preview?.kill("SIGTERM");
if (process.exitCode === 2) {
  console.error("PEEK-REACH: FAIL");
} else {
  console.log("PEEK-REACH: PASS");
}
// Live child-process handles keep the event loop alive — exit explicitly or
// the no-arg path never delivers its documented exit code.
process.exit(process.exitCode ?? 0);
