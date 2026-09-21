// Card-fit assertion (AC A-5 / mutant-d instrument): every board card's
// rendered content must fit inside its fixed virtual-row height, at both
// densities and at 125% zoom. jsdom cannot answer this — it does not lay
// out — so the measurement runs in real Chrome over CDP and fails the
// process when any card's scrollHeight exceeds its clientHeight.
//
// Usage:
//   npx vite preview --config vite.proto.config.ts --port 5199 --strictPort &
//   node src/queue-proto/evidence/assert-card-fit.mjs [baseUrl]
//
// Exit 0 = every measured card fits; exit 2 = at least one card overflows.

import { spawn } from "node:child_process";

const CHROME = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
const CDP_PORT = 9334;
const BASE = process.argv[2] ?? "http://localhost:5199/queue-proto.html";
const PROFILE = "/tmp/queue-proto-cardfit-profile";

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
  [
    `--remote-debugging-port=${CDP_PORT}`,
    "--headless=new",
    "--hide-scrollbars",
    `--user-data-dir=${PROFILE}`,
    "about:blank",
  ],
  { stdio: "ignore" },
);
process.on("exit", () => chrome.kill("SIGTERM"));

const client = cdp(await devtoolsTarget());
await client.ready;
await client.send("Page.enable");
await client.send("Runtime.enable");

let loadedResolve = null;
client.on("Page.loadEventFired", () => loadedResolve?.());

async function navigate(url) {
  try {
    await evalJs(`localStorage.clear()`);
  } catch {
    // about:blank has no storage
  }
  const loaded = new Promise((r) => (loadedResolve = r));
  await client.send("Page.navigate", { url });
  await Promise.race([loaded, sleep(15000)]);
  await sleep(500); // React commit + effects + virtual-list measurement
}

async function evalJs(expression) {
  const res = await client.send("Runtime.evaluate", { expression, returnByValue: true, awaitPromise: true });
  if (res.exceptionDetails) {
    throw new Error(`eval failed: ${JSON.stringify(res.exceptionDetails).slice(0, 300)}`);
  }
  return res.result?.value;
}

// Scroll every column's virtual list end to end so every card is mounted and
// measured, then report any whose content height exceeds the fixed row box.
// Each scroll position waits two rAF frames: React must commit the new
// virtual window before the DOM is read, or cards below the fold are never
// measured. Coverage is cross-checked against the column-head counts so a
// silently skipped card cannot pass as fitting.
const MEASURE = `(async () => {
  const frame = () => new Promise((r) => requestAnimationFrame(r));
  const expected = [...document.querySelectorAll("[data-col-count]")].reduce((n, el) => n + Number(el.textContent), 0);
  const out = [];
  for (const list of document.querySelectorAll(".qp-col-cards")) {
    const seen = new Set();
    const step = Math.max(40, list.clientHeight - 40);
    for (let top = 0; top <= list.scrollHeight + step; top += step) {
      list.scrollTop = top;
      await frame();
      await frame();
      for (const card of list.querySelectorAll(".qp-card")) {
        const id = card.dataset.taskId;
        if (seen.has(id)) {
          continue;
        }
        seen.add(id);
        out.push({
          id,
          scrollH: card.scrollHeight,
          clientH: card.clientHeight,
          fits: card.scrollHeight <= card.clientHeight,
        });
      }
    }
    list.scrollTop = 0;
    await frame();
  }
  return { expected, cards: out };
})()`;

async function setDensity(want) {
  // #qp-density toggles compact↔comfortable; aria-pressed = comfortable.
  const isComfortable = await evalJs(`document.getElementById("qp-density")?.getAttribute("aria-pressed")`);
  if ((isComfortable === "true") !== (want === "comfortable")) {
    await evalJs(`document.getElementById("qp-density")?.click()`);
    await sleep(300);
  }
}

async function measureBoard(label) {
  const res = await evalJs(MEASURE);
  const cards = res?.cards ?? [];
  if (cards.length === 0) {
    console.error(`${label}: no .qp-card elements measured — UNVERIFIED`);
    process.exitCode = 2;
    return;
  }
  if (cards.length !== res.expected) {
    console.error(`${label}: measured ${cards.length} cards but column heads count ${res.expected} — UNVERIFIED`);
    process.exitCode = 2;
  }
  const bad = cards.filter((c) => !c.fits);
  console.log(`${label}: ${cards.length}/${res.expected} cards measured, ${bad.length} overflow`);
  for (const c of bad.slice(0, 10)) {
    console.error(`  card #${c.id}: scrollHeight=${c.scrollH} > clientHeight=${c.clientH}`);
  }
  if (bad.length > 0) {
    process.exitCode = 2;
  }
}

for (const [label, scale] of [
  ["100%", 1],
  ["125%", 1.25],
]) {
  await client.send("Emulation.setDeviceMetricsOverride", { width: 1440, height: 900, deviceScaleFactor: scale, mobile: false });
  await navigate(`${BASE}?view=all&layout=board`);
  for (const density of ["comfortable", "compact"]) {
    await setDensity(density);
    await sleep(200);
    await measureBoard(`${label} ${density}`);
  }
}

await client.close();
chrome.kill("SIGTERM");
if (process.exitCode === 2) {
  console.error("CARD-FIT: FAIL");
} else {
  console.log("CARD-FIT: PASS");
}
