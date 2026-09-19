# Evidence capture — exact commands

All artifacts in this directory were produced on macOS (Darwin 25.6.0),
Node v22.22.2, Chrome headless (`/Applications/Google Chrome.app/Contents/MacOS/Google Chrome`),
against the prototype-only Vite build (`dist-proto/`, gitignored).

## Build + serve

```bash
cd web/console
npm ci
npm run build:proto          # vite build --config vite.proto.config.ts → dist-proto/
npx vite preview --config vite.proto.config.ts --port 5199 --strictPort &
```

The page is `http://localhost:5199/queue-proto.html`.
URL params: `?set=sample200|edge|perf5000` (default sample200; perf defaults
to perf5000), `?view=operator|active|backlog|all`, `?layout=list|board`,
`?group=area`, `?diag=1` (renders measured facts into `<pre id="diag">`),
`?perf=1` (auto-runs warmup + 20 filter-to-paint timings into `<pre id="perf">`).

## Automated capture (screenshots + diag JSON + perf JSON)

```bash
node src/queue-proto/evidence/capture.mjs
```

`capture.mjs` spawns Chrome `--headless=new --remote-debugging-port=9333`
with a scratch profile and drives it over CDP: `Emulation.setDeviceMetricsOverride`
for exact viewports, `Page.captureScreenshot` for PNGs, `Runtime.evaluate`
to extract `#diag` / `#perf` JSON. Local only — no production session.

## Manual equivalents

```bash
CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
"$CHROME" --headless=new --disable-gpu --screenshot=list-1440x900.png \
  --window-size=1440,900 --hide-scrollbars \
  "http://localhost:5199/queue-proto.html?view=backlog&layout=list"
"$CHROME" --headless=new --disable-gpu --dump-dom --window-size=390,844 \
  "http://localhost:5199/queue-proto.html?diag=1&view=backlog&layout=list" > diag-390.dom.html
"$CHROME" --headless=new --disable-gpu --dump-dom --window-size=1440,900 \
  --virtual-time-budget=30000 \
  "http://localhost:5199/queue-proto.html?perf=1&set=perf5000" > perf.dom.html
```

(Note: plain `--dump-dom` captures the DOM before React's passive effects
flush, so `#diag`/`#perf` may be empty in raw dumps — the CDP path above is
the supported evidence route; it waits for the elements to populate.)

## Results (latest capture)

- `list-1440x900.png` / `diag-1440x900.json` — 46 rendered list rows (≥12),
  scrollWidth=clientWidth=1440.
- `board-active-1440x900.png` / `diag-board-1440x900.json` — 4 populated
  columns (claimed 10, in_progress 12, verifying 5, needs_decision 8), never
  all nine.
- `grouped-1440x900.png` — area→bundle grouping with collapsed signal
  rollups.
- `drawer-1440x900.png` / `diag-drawer-1440x900.json` — shared drawer open,
  `#qp-main` inert, close control in viewport.
- `list-390x844.png` / `diag-390x844.json` — scrollWidth=clientWidth=390,
  zero body horizontal scroll.
- `drawer-390x844.png` / `diag-drawer-390x844.json` — full-screen drawer,
  `inert` on background.
- `diag-zoom200.json` — `--force-device-scale-factor=2` equivalent via CDP
  deviceScaleFactor=2: essential controls in viewport.
- `perf-5000.json` / `perf-5000.png` — 20 raw filter-to-paint observations
  on the 5000-task set, frameDriven=true, p95 < 200ms.
