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
URL params: `?set=sample200|edge|stale63|perf5000` (default sample200; perf
defaults to perf5000), `?view=operator|active|backlog|all`,
`?layout=list|board`, `?group=area|none`, `?diag=1` (renders measured facts
into `<pre id="diag">`), `?perf=1` (auto-runs warmup + 20 filter-to-paint
timings into `<pre id="perf">`).

## Automated capture (screenshots + diag JSON + perf JSON)

```bash
node src/queue-proto/evidence/capture.mjs
```

`capture.mjs` spawns Chrome `--headless=new --remote-debugging-port=9333`
with a scratch profile and drives it over CDP: `Emulation.setDeviceMetricsOverride`
for exact viewports, `Page.captureScreenshot` for PNGs, `Runtime.evaluate`
to extract `#diag` / `#perf` JSON. Each scenario clears `localStorage` first
so persisted presentation state never leaks between captures, and each diag
read is preceded by a `resize` dispatch so `<pre id="diag">` reflects
steady-state layout rather than first-commit measurements. Local only — no
production session.

## Automated geometry assertions (K5.1)

```bash
node src/queue-proto/evidence/assert-geometry.mjs --full \
  src/queue-proto/evidence/diag-iter1-grouped-1440x900.json
node src/queue-proto/evidence/assert-geometry.mjs \
  src/queue-proto/evidence/diag-390x844.json \
  src/queue-proto/evidence/diag-zoom200.json
```

`--full` checks the 1440×900 density contract (rail 224–240px, margins ≤24px,
main ≥1152px, title+toolbar ≤112px, ≥16 data rows in first viewport, row
36–40px, text ≥14px, zero horizontal scroll). Without `--full` each file gets
the narrow/zoom subset (zero horizontal scroll + core controls reachable).

## Real-pointer rail assertion (390×844)

```bash
node src/queue-proto/evidence/assert-rail-pointer.mjs
```

Spawns Chrome `--headless=new --remote-debugging-port=9334` and drives the
rail through `Input.dispatchMouseEvent` — the real hit-test path, not DOM
`.click()`, which would bypass pointer occlusion. At 390×844 it requires: a
pointer click on `#qp-rail-toggle` opens the rail; while open,
`document.elementFromPoint` at the toggle's center still returns the toggle
(the fixed `.qp-rail` overlay starts at `--qp-head-h`, below the header, so
it never covers the only close control); the `.qp-head` and `.qp-rail-h`
anchors must be present and non-empty — a missing anchor is a failure, never
a vacuous pass; the rail's top edge must clear the header's bottom edge
(`rail_rect.top >= head_rect.bottom`, the repair invariant asserted
directly); the toggle rect does not overlap the first `.qp-rail-h` content
rect; and a second pointer click closes the rail. Exits nonzero on any
failure.

## Card-fit assertion (board clipping gate)

```bash
node src/queue-proto/evidence/assert-card-fit.mjs
```

With no argument the script builds `dist-proto/` if absent and serves it on
its own ephemeral port — never trust a shared default port for this check.
An explicit base URL is accepted but validated to serve the fixture page.

Spawns Chrome `--headless=new --remote-debugging-port=9334` and measures real
layout — jsdom computes none, so this is the only fit check that counts. For
each density (`comfortable`, `compact`) at 100% and 125% zoom
(`deviceScaleFactor`), it scrolls every `.qp-col-cards` virtual list end to
end — two rAF frames per position so React commits each window before the
read — and asserts `scrollHeight <= clientHeight` for every `.qp-card`. The
measured count is cross-checked against the `data-col-count` column heads so
a skipped card is a failure, not a vacuous pass. Exits nonzero when any card
overflows or coverage is incomplete.

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

- `iter1-grouped-1440x900.png` / `diag-iter1-grouped-1440x900.json` — the
  K5.1 surface: `?set=stale63&view=all&layout=list&group=area`. Rail 232px,
  margins 12px/12px, main 1208px, chrome 84px, 20 data rows in first
  viewport, 38px rows, 14px text, zero horizontal scroll — all asserted by
  `assert-geometry.mjs --full`.
- `list-1440x900.png` / `diag-1440x900.json` — grouped backlog (area→bundle
  is the default); scrollWidth=clientWidth=1440.
- `board-active-1440x900.png` / `diag-board-1440x900.json` — 4 populated
  columns (claimed 10, in_progress 12, verifying 5, needs_decision 8), never
  all nine.
- `grouped-1440x900.png` — area→bundle grouping with collapsed signal
  rollups.
- `drawer-1440x900.png` / `diag-drawer-1440x900.json` — shared peek panel
  open; the panel is non-modal so `#qp-main` is never inert.
- `list-390x844.png` / `diag-390x844.json` — scrollWidth=clientWidth=390,
  zero body horizontal scroll; rail overlay + toggle keeps core controls
  reachable.
- `drawer-390x844.png` / `diag-drawer-390x844.json` — narrow viewport: the
  peek panel docks as a bottom sheet so the list above stays clickable.
- `diag-zoom200.json` — `--force-device-scale-factor=2` equivalent via CDP
  deviceScaleFactor=2: essential controls in viewport.
- `perf-5000.json` / `perf-5000.png` — 20 raw filter-to-paint observations
  on the 5000-task set, frameDriven=true, p95 < 200ms.

## Operator trial

`TRIAL.md` holds the reproducible I1–I5 script protocol (twice each,
alternating baseline/iter1 order) against the sanitized `stale63` fixture.
The later operator gate stays pending until an operator actually runs it.
