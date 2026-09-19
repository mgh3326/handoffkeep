// Headless-Chrome evidence capture via CDP (no external deps — Node ≥21
// built-in WebSocket + fetch). Drives the local prototype preview server,
// captures screenshots at exact viewports, extracts #diag JSON facts, and
// collects the ?perf=1 20-run filter-to-paint result with real rAF frames.
//
// Usage:
//   npx vite preview --config vite.proto.config.ts --port 5199 --strictPort &
//   node src/queue-proto/evidence/capture.mjs [outDir] [baseUrl]

import { spawn } from "node:child_process";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const CHROME = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
const CDP_PORT = 9333;
const BASE = process.argv[3] ?? "http://localhost:5199/queue-proto.html";
const OUT = process.argv[2] ?? new URL(".", import.meta.url).pathname;
const PROFILE = "/tmp/queue-proto-chrome-profile";

mkdirSync(OUT, { recursive: true });

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

async function navigate(url) {
  const loaded = new Promise((r) => (loadedResolve = r));
  await client.send("Page.navigate", { url });
  await Promise.race([loaded, sleep(15000)]);
  await sleep(400); // let React commit + effects settle
}

async function evalJs(expression) {
  const res = await client.send("Runtime.evaluate", { expression, returnByValue: true, awaitPromise: true });
  if (res.exceptionDetails) {
    throw new Error(`eval failed: ${JSON.stringify(res.exceptionDetails).slice(0, 300)}`);
  }
  return res.result?.value;
}

async function shot(name) {
  const res = await client.send("Page.captureScreenshot", { format: "png" });
  writeFileSync(join(OUT, name), Buffer.from(res.data, "base64"));
  console.log(`wrote ${name}`);
}

async function viewport(width, height, scale = 1) {
  await client.send("Emulation.setDeviceMetricsOverride", {
    width,
    height,
    deviceScaleFactor: scale,
    mobile: width < 500,
  });
}

async function diagJson(name) {
  const text = await evalJs(`document.getElementById("diag")?.textContent ?? ""`);
  if (!text) {
    throw new Error(`diag empty for ${name}`);
  }
  writeFileSync(join(OUT, name), text);
  const facts = JSON.parse(text);
  console.log(
    `${name}: scroll=${facts.viewport.scrollWidth}/${facts.viewport.clientWidth} ` +
      `rows=${facts.rendered.listRows} cols=${facts.rendered.populatedColumns} ` +
      `controls=${Object.entries(facts.controls)
        .map(([k, v]) => `${k}:${v.present ? (v.inViewport === false ? "offscreen" : "ok") : "absent"}`)
        .join(",")}`,
  );
  return facts;
}

// deterministic state: clear persisted presentation state up front.
await navigate(BASE);
await evalJs(`localStorage.clear()`);

// --- 1440×900 ----------------------------------------------------------
await viewport(1440, 900);
await navigate(`${BASE}?view=backlog&layout=list`);
await shot("list-1440x900.png");
await navigate(`${BASE}?view=active&layout=board`);
await shot("board-active-1440x900.png");
await navigate(`${BASE}?view=backlog&layout=list&group=area`);
await shot("grouped-1440x900.png");
await navigate(`${BASE}?diag=1&view=backlog&layout=list`);
await sleep(300);
await diagJson("diag-1440x900.json");

// drawer at desktop width (same diag page — drawer controls included)
await evalJs(`document.querySelector("[data-task-id]")?.click()`);
await sleep(300);
await shot("drawer-1440x900.png");
await diagJson("diag-drawer-1440x900.json");
console.log("inert:", await evalJs(`document.getElementById("qp-main")?.hasAttribute("inert")`));
await evalJs(`document.querySelector(".qp-drawer-close")?.click()`);
await sleep(200);

// board diag: populated columns + per-column card counts
await navigate(`${BASE}?diag=1&view=active&layout=board`);
await sleep(300);
await diagJson("diag-board-1440x900.json");

// --- 390×844 -----------------------------------------------------------
await viewport(390, 844, 1);
await navigate(`${BASE}?view=backlog&layout=list`);
await shot("list-390x844.png");
await navigate(`${BASE}?diag=1&view=backlog&layout=list`);
await sleep(300);
const narrow = await diagJson("diag-390x844.json");
if (narrow.viewport.scrollWidth !== narrow.viewport.clientWidth) {
  console.error("HORIZONTAL SCROLL AT 390px — FAIL");
}
await evalJs(`document.querySelector("[data-task-id]")?.click()`);
await sleep(300);
await shot("drawer-390x844.png");
await diagJson("diag-drawer-390x844.json");
console.log("mobile drawer inert:", await evalJs(`document.getElementById("qp-main")?.hasAttribute("inert")`));
await evalJs(`document.querySelector(".qp-drawer-close")?.click()`);

// --- 200% zoom ---------------------------------------------------------
await viewport(1440, 900, 2);
await navigate(`${BASE}?diag=1`);
await sleep(300);
await diagJson("diag-zoom200.json");

// --- perf: 5000-task set ------------------------------------------------
await viewport(1440, 900, 1);
await navigate(`${BASE}?perf=1&set=perf5000`);
let perfText = "";
for (let i = 0; i < 240; i++) {
  await sleep(500);
  perfText = await evalJs(`document.getElementById("perf")?.textContent ?? ""`);
  if (perfText) {
    break;
  }
}
if (!perfText) {
  console.error("PERF: no result captured — UNVERIFIED");
  process.exitCode = 2;
} else {
  writeFileSync(join(OUT, "perf-5000.json"), perfText);
  const perf = JSON.parse(perfText);
  console.log(`perf: n=${perf.runs.length} p50=${perf.p50}ms p95=${perf.p95}ms max=${perf.max}ms frameDriven=${perf.env.frameDriven} pass=${perf.pass}`);
  await shot("perf-5000.png");
}

await client.close();
chrome.kill("SIGTERM");
console.log("capture done");
