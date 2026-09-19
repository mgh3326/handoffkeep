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

/** Renders measured facts into <pre id="diag"> so headless Chrome
 * `--dump-dom` produces automated viewport evidence. */
export function runDiag(el: HTMLPreElement, dataset: Dataset): void {
  const perColumn: Record<string, number> = {};
  document.querySelectorAll<HTMLElement>(".qp-col").forEach((col) => {
    const state = col.dataset.state ?? "?";
    perColumn[state] = col.querySelectorAll(".qp-card").length;
  });
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
    rendered: {
      listRows: document.querySelectorAll(".qp-row").length,
      groupHeaders: document.querySelectorAll(".qp-group").length,
      populatedColumns: document.querySelectorAll(".qp-col").length,
      perColumn,
      cards: document.querySelectorAll(".qp-card").length,
    },
    controls: {
      search: bounds("#qp-search"),
      layoutToggle: bounds("#qp-layout-toggle"),
      groupToggle: bounds("#qp-group-toggle"),
      viewBar: bounds("#qp-viewbar"),
      drawer: bounds(".qp-drawer"),
      drawerClose: bounds(".qp-drawer-close"),
      measurePanel: bounds("#qp-measure"),
    },
  };
  el.textContent = JSON.stringify(facts, null, 2);
}
