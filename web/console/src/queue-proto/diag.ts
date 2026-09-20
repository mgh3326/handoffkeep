import type { Dataset } from "./types";

function bounds(selector: string): { present: boolean; inViewport?: boolean; rect?: { top: number; left: number; width: number; height: number } } {
  const el = document.querySelector(selector);
  if (!el) {
    return { present: false };
  }
  const r = el.getBoundingClientRect();
  const inViewport = r.bottom >= 0 && r.right >= 0 && r.top <= window.innerHeight && r.left <= window.innerWidth;
  return { present: true, inViewport, rect: { top: Math.round(r.top), left: Math.round(r.left), width: Math.round(r.width), height: Math.round(r.height) } };
}

function px(selector: string, prop: "width" | "height"): number | null {
  const el = document.querySelector<HTMLElement>(selector);
  return el ? Math.round(el.getBoundingClientRect()[prop]) : null;
}

/** Renders measured facts into <pre id="diag"> so headless Chrome
 * `--dump-dom` produces automated viewport evidence. Geometry numbers are
 * the acceptance surface for the density contract — assert-geometry.mjs
 * checks them against the K5.1 thresholds. */
export function runDiag(el: HTMLPreElement, dataset: Dataset): void {
  const perColumn: Record<string, number> = {};
  document.querySelectorAll<HTMLElement>(".qp-col").forEach((col) => {
    const state = col.dataset.state ?? "?";
    perColumn[state] = col.querySelectorAll(".qp-card").length;
  });

  const body = document.querySelector<HTMLElement>(".qp-body");
  const content = document.querySelector<HTMLElement>(".qp-content");
  const firstRow = document.querySelector<HTMLElement>(".qp-row");
  const firstCell = document.querySelector<HTMLElement>(".qp-row .qp-cell");
  const lastCell = document.querySelector<HTMLElement>(".qp-row .qp-cell:last-child");
  let firstViewportRows = 0;
  document.querySelectorAll<HTMLElement>(".qp-row").forEach((row) => {
    const r = row.getBoundingClientRect();
    if (r.top < window.innerHeight && r.bottom > 0) {
      firstViewportRows += 1;
    }
  });
  const bodyRect = body?.getBoundingClientRect();
  const contentRect = content?.getBoundingClientRect();
  const firstCellRect = firstCell?.getBoundingClientRect();
  const lastCellRect = lastCell?.getBoundingClientRect();

  const facts = {
    at: new Date().toISOString(),
    dataset: { key: dataset.key, size: dataset.tasks.length, completeness: dataset.completeness },
    viewport: {
      innerWidth: window.innerWidth,
      innerHeight: window.innerHeight,
      devicePixelRatio: window.devicePixelRatio,
      scrollWidth: document.documentElement.scrollWidth,
      clientWidth: document.documentElement.clientWidth,
      bodyHorizontalScroll: document.documentElement.scrollWidth - document.documentElement.clientWidth,
    },
    geometry: {
      railWidth: px("#qp-rail", "width"),
      chromeHeight: (px(".qp-head", "height") ?? 0) + (px(".qp-toolbar", "height") ?? 0),
      contentMarginLeft: bodyRect && firstCellRect ? Math.round(firstCellRect.left - bodyRect.left) : null,
      contentMarginRight: bodyRect && lastCellRect ? Math.round(bodyRect.right - lastCellRect.right) : null,
      mainAvailableWidth: contentRect ? Math.round(contentRect.width) : null,
      firstViewportRows,
      rowHeight: firstRow ? Math.round(firstRow.getBoundingClientRect().height) : null,
      rowFontSizePx: firstRow ? parseFloat(getComputedStyle(firstRow).fontSize) : null,
      colHeaderHeight: px(".qp-colhead", "height"),
    },
    rendered: {
      listRows: document.querySelectorAll(".qp-row").length,
      groupHeaders: document.querySelectorAll(".qp-group").length,
      staleBadges: document.querySelectorAll(".qp-stale").length,
      populatedColumns: document.querySelectorAll(".qp-col").length,
      perColumn,
      cards: document.querySelectorAll(".qp-card").length,
    },
    controls: {
      rail: bounds("#qp-rail"),
      railToggle: bounds("#qp-rail-toggle"),
      search: bounds("#qp-search"),
      layoutToggle: bounds("#qp-layout-toggle"),
      groupToggle: bounds("#qp-group-toggle"),
      drawer: bounds(".qp-drawer"),
      drawerClose: bounds(".qp-drawer-close"),
      measurePanel: bounds("#qp-measure"),
    },
  };
  el.textContent = JSON.stringify(facts, null, 2);
}
