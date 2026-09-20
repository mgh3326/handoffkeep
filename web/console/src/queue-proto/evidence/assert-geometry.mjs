// Automated geometry/density assertions (K5.1) against diag JSON produced by
// capture.mjs. Reads one or more diag-*.json files and exits nonzero on any
// violation. With --full, every file is checked against the 1440×900 density
// contract; without it, files get the "core controls survive" subset used for
// narrow/zoom captures (collapse/reflow is allowed there).
//
// Usage:
//   node assert-geometry.mjs --full evidence/diag-iter1-grouped-1440x900.json
//   node assert-geometry.mjs evidence/diag-390x844.json evidence/diag-zoom200.json

import { readFileSync } from "node:fs";

const full = process.argv.includes("--full");
const files = process.argv.slice(2).filter((a) => a !== "--full");

if (files.length === 0) {
  console.log("usage: assert-geometry.mjs [--full] <diag.json> ...");
  process.exit(2);
}

let failures = 0;
const check = (file, name, ok, detail) => {
  if (ok) {
    console.log(`PASS ${file}: ${name} (${detail})`);
  } else {
    failures += 1;
    console.log(`FAIL ${file}: ${name} (${detail})`);
  }
};

for (const file of files) {
  const facts = JSON.parse(readFileSync(file, "utf8"));
  const g = facts.geometry ?? {};
  const vp = facts.viewport ?? {};
  const controls = facts.controls ?? {};

  check(file, "zero horizontal page scroll", vp.bodyHorizontalScroll === 0, `scroll-client=${vp.bodyHorizontalScroll}`);

  if (full) {
    check(file, "viewport is 1440x900", vp.innerWidth === 1440 && vp.innerHeight === 900, `${vp.innerWidth}x${vp.innerHeight}`);
    check(file, "sidebar width 224–240px", g.railWidth !== null && g.railWidth >= 224 && g.railWidth <= 240, `rail=${g.railWidth}px`);
    check(file, "left content margin ≤24px", g.contentMarginLeft !== null && g.contentMarginLeft <= 24, `left=${g.contentMarginLeft}px`);
    check(file, "right content margin ≤24px", g.contentMarginRight !== null && g.contentMarginRight <= 24, `right=${g.contentMarginRight}px`);
    check(file, "main available width ≥1152px", g.mainAvailableWidth !== null && g.mainAvailableWidth >= 1152, `main=${g.mainAvailableWidth}px`);
    check(file, "title+toolbar ≤112px", g.chromeHeight !== null && g.chromeHeight <= 112, `chrome=${g.chromeHeight}px`);
    check(file, "≥16 data rows in first viewport", g.firstViewportRows >= 16, `rows=${g.firstViewportRows}`);
    check(file, "row height 36–40px", g.rowHeight !== null && g.rowHeight >= 36 && g.rowHeight <= 40, `row=${g.rowHeight}px`);
    check(file, "body text ≥14px", g.rowFontSizePx !== null && g.rowFontSizePx >= 14, `font=${g.rowFontSizePx}px`);
  }

  // every viewport: core controls must remain reachable (the rail may be
  // collapsed to its toggle — presence of the toggle is the invariant)
  check(file, "rail toggle reachable", controls.railToggle?.present === true && controls.railToggle.inViewport !== false, "toggle");
  check(file, "search reachable", controls.search?.present === true && controls.search.inViewport !== false, "search");
  check(file, "layout toggle reachable", controls.layoutToggle?.present === true && controls.layoutToggle.inViewport !== false, "layout");
}

console.log(failures === 0 ? "ALL GEOMETRY ASSERTIONS PASSED" : `${failures} geometry assertion(s) FAILED`);
process.exitCode = failures === 0 ? 0 : 1;
