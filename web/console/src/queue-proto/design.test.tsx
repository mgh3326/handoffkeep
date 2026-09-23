// #535 console design system slice 1 (hk:doc 2408 §2): tokens, the state-
// grouped list, 40px default density, full titles, visible data gaps. The
// mutants in 2408 §4 each turn one of these assertions red.

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { gzipSync } from "node:zlib";
import { renderToStaticMarkup } from "react-dom/server";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import type { BoardTask } from "../board/types";
import { applyTheme, loadTheme } from "./AppShell";
import { boardTaskToProto } from "./boardtask";
import { QueueProtoApp } from "./QueueProtoApp";
import { STATE_SHAPES, StateIcon } from "./StateIcon";
import { STATE_GROUP_ORDER, STATE_LABEL } from "./states";
import { DEFAULT_STATE, STORAGE_KEY } from "./storage";
import { KNOWN_STATES, type Dataset } from "./types";

const here = dirname(fileURLToPath(import.meta.url));
const consoleRoot = resolve(here, "..", "..");
const built = resolve(consoleRoot, "..", "..", "internal", "ui", "static", "console");
const read = (...p: string[]) => readFileSync(join(...p), "utf8");

const TOKENS = read(consoleRoot, "src", "design", "tokens.css");
const PROTO_CSS = read(here, "proto.css");

function mkBoardTask(over: Partial<BoardTask>): BoardTask {
  return {
    id: 7001,
    lane: "live-lane",
    title: "live task alpha",
    kind: "fix",
    state: "claimed",
    priority: 40,
    created_by: "live-op",
    created_at: "2026-09-19T09:00:00+09:00",
    updated_at: "2026-09-20T09:00:00+09:00",
    refs: {},
    ...over,
  };
}

function liveDataset(tasks: BoardTask[], completeness: Dataset["completeness"] = "complete"): Dataset {
  return {
    key: "live",
    label: "live queue backlog",
    source: "live",
    generatedAt: "2026-09-21T14:20:00+09:00",
    completeness,
    completenessNote: "live",
    // server-provided enumeration: the canonical nine plus any server-only state
    states: [...new Set([...KNOWN_STATES, ...tasks.map((t) => t.state)])],
    tasks: tasks.map(boardTaskToProto),
    enrichment: {},
  };
}

function renderLive(tasks: BoardTask[], opts: { completeness?: Dataset["completeness"]; refreshFailed?: boolean } = {}) {
  return render(
    <QueueProtoApp datasets={{ live: liveDataset(tasks, opts.completeness) }} initialSet="live" refreshFailed={opts.refreshFailed} />,
  );
}

beforeEach(() => {
  localStorage.clear();
  window.history.replaceState(null, "", "/ui/queue?view=all");
});

// ---- (a) tokens are the only color source ------------------------------

/** Declarations of CSS text, comments removed. */
function declarations(css: string): string[] {
  return css
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .split(/[;{}]/)
    .map((d) => d.trim())
    .filter((d) => /^[-a-z]+\s*:/.test(d));
}

const COLOR_LITERAL = /#[0-9a-f]{3,8}\b|\b(?:rgba?|hsla?|hwb|lab|lch|oklab|oklch|color)\(|\b(?:white|black|red|green|blue|gray|grey|orange|yellow|purple|canvas|canvastext|buttonface)\b/i;

/** Migrated product sources: every file the list/shell renders from. */
const MIGRATED_TSX = ["AppShell.tsx", "ListView.tsx", "QueueProtoApp.tsx", "StateIcon.tsx", "TaskRow.tsx", "Toolbar.tsx", "ViewRail.tsx", "live.tsx", "main.tsx", "states.ts"];

function colorLiterals(css: string): string[] {
  return declarations(css).filter((d) => !d.startsWith("--") && COLOR_LITERAL.test(d.slice(d.indexOf(":") + 1)));
}

describe("(a) no color literal outside tokens.css", () => {
  it("proto.css declares no color value of its own", () => {
    expect(declarations(PROTO_CSS).length).toBeGreaterThan(200); // the scan saw the real file
    expect(colorLiterals(PROTO_CSS)).toEqual([]);
  });

  it("the lint catches a literal (non-vacuous)", () => {
    expect(colorLiterals(".x { color: #fff; }")).toEqual(["color: #fff"]);
    expect(colorLiterals(".x { background: rgb(0 0 0 / 10%); }")).toHaveLength(1);
    expect(colorLiterals(".x { background: Canvas; }")).toHaveLength(1);
    expect(colorLiterals(".x { color: var(--hk-accent); border-color: transparent; }")).toEqual([]);
  });

  it("migrated TSX carries no inline color literal", () => {
    for (const file of MIGRATED_TSX) {
      const text = read(here, file);
      const hits = [...text.matchAll(/(?:style=\{\{[^}]*\}\}|(?:color|background|fill|stroke|border)[A-Za-z]*\s*[:=]\s*["'][^"']*["'])/g)]
        .map((m) => m[0])
        .filter((s) => COLOR_LITERAL.test(s));
      expect(hits, file).toEqual([]);
    }
  });

  it("every --hk-* token proto.css uses is defined in tokens.css", () => {
    const defined = new Set([...TOKENS.matchAll(/(--hk-[a-z0-9-]+)\s*:/g)].map((m) => m[1]));
    const used = new Set([...PROTO_CSS.matchAll(/var\((--hk-[a-z0-9-]+)/g)].map((m) => m[1]));
    expect(used.size).toBeGreaterThan(30);
    expect([...used].filter((t) => !defined.has(t))).toEqual([]);
  });

  it("the board page links the one token stylesheet before the app styles", () => {
    const tpl = read(consoleRoot, "..", "..", "internal", "ui", "templates", "board.html");
    const tokens = tpl.indexOf("/ui/static/console/tokens.css");
    const board = tpl.indexOf("/ui/static/console/board.css");
    expect(tokens).toBeGreaterThan(0);
    expect(tokens).toBeLessThan(board);
    expect(existsSync(join(built, "tokens.css"))).toBe(true);
    // the app bundle does not carry its own copy of the token values
    expect(read(built, "board.css")).not.toContain("--hk-surface-0:");
  });
});

// ---- contrast of the token pairs (2402 Q3) -----------------------------

function themeValues(theme: "dark" | "light"): Record<string, string> {
  const block = (sel: RegExp) => TOKENS.match(sel)?.[1] ?? "";
  const dark = block(/:root\s*\{([^}]*--hk-surface-0[^}]*)\}/);
  const light = block(/:root\[data-theme="light"\]\s*\{([^}]*)\}/);
  const out: Record<string, string> = {};
  for (const text of theme === "dark" ? [dark] : [dark, light]) {
    for (const m of text.matchAll(/(--hk-[a-z0-9-]+)\s*:\s*(#[0-9a-f]{6})/gi)) {
      out[m[1]] = m[2];
    }
  }
  return out;
}

function luminance(hex: string): number {
  const c = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255).map((v) => (v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4));
  return 0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2];
}

function contrast(a: string, b: string): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

describe("token contrast — text 4.5:1, required non-text 3:1", () => {
  const surfaces = ["--hk-surface-0", "--hk-surface-1", "--hk-surface-2", "--hk-surface-selected"];
  const text = ["--hk-text-primary", "--hk-text-secondary", "--hk-text-muted", "--hk-accent", "--hk-warning", "--hk-danger", "--hk-success"];
  const states = ["backlog", "claimed", "in-progress", "verifying", "join", "hold", "needs-decision", "merged", "dropped"].map((s) => `--hk-state-${s}`);
  for (const theme of ["dark", "light"] as const) {
    it(`${theme}: every text and state color clears 4.5:1 on every surface`, () => {
      const v = themeValues(theme);
      const fails: string[] = [];
      for (const fg of [...text, ...states]) {
        for (const bg of surfaces) {
          expect(v[fg], fg).toMatch(/^#/);
          const r = contrast(v[fg], v[bg]);
          if (r < 4.5) fails.push(`${fg} on ${bg} = ${r.toFixed(2)}`);
        }
      }
      expect(fails).toEqual([]);
    });
    it(`${theme}: control border and focus clear 3:1; action pair clears 4.5:1`, () => {
      const v = themeValues(theme);
      for (const fg of ["--hk-border-control", "--hk-focus"]) {
        for (const bg of surfaces) {
          expect(contrast(v[fg], v[bg]), `${fg} on ${bg}`).toBeGreaterThanOrEqual(3);
        }
      }
      expect(contrast(v["--hk-action-fg"], v["--hk-action-bg"])).toBeGreaterThanOrEqual(4.5);
    });
  }
});

// ---- (b) missing data stays visible ------------------------------------

describe("(b) missing fields render as missing — never 0 or blank", () => {
  it("a live row names the absent claimant and state-entry time", () => {
    const { container } = renderLive([mkBoardTask({})]);
    const row = container.querySelector<HTMLElement>('[data-task-id="7001"]')!;
    const unknowns = [...row.querySelectorAll(".qp-unknown")].map((u) => u.textContent);
    expect(unknowns).toEqual(["인수자 미상", "진입 미수집"]);
    const age = [...row.querySelectorAll<HTMLElement>(".qp-age")].map((a) => a.textContent);
    expect(age[1]).toBe("진입 미수집");
    expect(age[1]).not.toMatch(/\d/);
  });

  it("the header lists the fields the list API never carries", () => {
    renderLive([mkBoardTask({}), mkBoardTask({ id: 7002 })]);
    const gap = screen.getByTestId("data-status").querySelector('[data-status="not-collected"]');
    expect(gap?.textContent).toContain("상태 진입 시각 · 기한 · 막힘");
    expect(screen.getByTestId("data-status").textContent).toContain("14:20 확인 자료");
  });

  it("partial loads and failed refreshes are stated on the default screen", () => {
    renderLive([mkBoardTask({})], { completeness: "partial", refreshFailed: true });
    const status = screen.getByTestId("data-status");
    expect(status.getAttribute("role")).toBe("status");
    expect(status.querySelector('[data-status="partial"]')?.textContent).toContain("일부 자료만 조회됨");
    expect(status.querySelector('[data-status="refresh-failed"]')?.textContent).toContain("갱신 실패");
    expect(document.querySelector(".qp-count")!.textContent).toContain("확인된 1건");
    expect(document.querySelector(".qp-group-partial")).toBeTruthy();
  });

  it("a present claimant renders as itself — the mapping is not unconditionally missing", () => {
    const { container } = renderLive([mkBoardTask({ claimed_by: "worker-7" })]);
    const row = container.querySelector<HTMLElement>('[data-task-id="7001"]')!;
    expect(row.querySelector(".qp-claimant")?.textContent).toBe("worker-7");
    expect([...row.querySelectorAll(".qp-unknown")].map((u) => u.textContent)).toEqual(["진입 미수집"]);
  });

  it("the live screen carries no experiment wording or internal endpoint", () => {
    const { container } = renderLive([mkBoardTask({})]);
    fireEvent.keyDown(container.querySelector('[data-task-id="7001"]')!, { key: "Enter" });
    const text = document.body.textContent ?? "";
    for (const banned of ["/ui/api", "synthetic", "SYNTHETIC", "draft enrichment", "area→bundle", "measure"]) {
      expect(text, banned).not.toContain(banned);
    }
  });
});

// ---- (c) titles are never cut in JS ------------------------------------

describe("(c) the full title is in the DOM; only CSS clamps it", () => {
  const long =
    "[운영자 결정] #133/#134 권고 순서대로 진행 + 체결→operator 세션 맥락 전달로 매수·매도 추가/조정 구조. 🔴 운영자가 '자동 승인까지 해도 된다'고 권한 확대 — 기존 불변식(실주문은 항상 운영자)과 충돌하므로 상한·되먹임 정지조건·모의 선행·전수기록·deviation record 설계 후 운영자 회신, 설계 전 배선 금지. TAIL-MARKER-END";

  it("row title text equals the original string, marker and brackets intact", () => {
    const { container } = renderLive([mkBoardTask({ title: long })]);
    const title = container.querySelector<HTMLElement>('[data-task-id="7001"] .qp-title')!;
    expect(long.length).toBeGreaterThan(96 * 2);
    expect(title.textContent).toBe(long);
    expect(title.textContent).toContain("🔴");
    expect(title.textContent).toMatch(/^\[운영자 결정\]/);
    expect(title.textContent!.endsWith("TAIL-MARKER-END")).toBe(true);
  });

  it("the clamp lives in CSS: 1 line by default, 2 lines at 56px", () => {
    const css = PROTO_CSS.replace(/\s+/g, " ");
    expect(css).toMatch(/\.qp-row \.qp-title \{[^}]*-webkit-line-clamp: 1;[^}]*overflow: hidden;/);
    expect(css).toMatch(/\.qp-listwrap\[data-density="comfortable"\] \.qp-row \.qp-title \{[^}]*-webkit-line-clamp: 2;/);
    expect(css).not.toMatch(/\.qp-title[^{]*\{[^}]*display: none/);
  });
});

// ---- (d) default density ----------------------------------------------

describe("(d) default row density is 40px · 1 line", () => {
  it("the product default and a fresh live render use the compact row", () => {
    expect(DEFAULT_STATE.density).toBe("compact");
    expect(DEFAULT_STATE.grouping).toBe("state");
    const { container } = renderLive([mkBoardTask({})]);
    expect(container.querySelector(".qp-listwrap")!.getAttribute("data-density")).toBe("compact");
    const rowBox = container.querySelector<HTMLElement>('[data-task-id="7001"]')!.parentElement!;
    expect(rowBox.style.height).toBe("40px");
    expect(screen.getByRole("button", { name: "행 40px · 1줄" }).getAttribute("aria-pressed")).toBe("true");
  });

  it("choosing 56px · 2 lines persists in the browser and survives a reload", () => {
    const first = renderLive([mkBoardTask({})]);
    fireEvent.click(screen.getByRole("button", { name: "행 56px · 2줄" }));
    const rowBox = first.container.querySelector<HTMLElement>('[data-task-id="7001"]')!.parentElement!;
    expect(rowBox.style.height).toBe("56px");
    expect(JSON.parse(localStorage.getItem(STORAGE_KEY)!).current.density).toBe("comfortable");
    first.unmount();
    const second = renderLive([mkBoardTask({})]);
    expect(second.container.querySelector(".qp-listwrap")!.getAttribute("data-density")).toBe("comfortable");
  });
});

// ---- structure of the list (AC2) --------------------------------------

describe("state-grouped list structure", () => {
  const tasks = [
    mkBoardTask({ id: 1, state: "backlog", priority: 5 }),
    mkBoardTask({ id: 2, state: "needs_decision", priority: 94 }),
    mkBoardTask({ id: 3, state: "needs_decision", priority: 10 }),
    mkBoardTask({ id: 4, state: "in_progress", priority: 50 }),
    mkBoardTask({ id: 5, state: "parked", priority: 50 }),
  ];

  it("groups by state in operator order with shape, Korean name and count", () => {
    const { container } = renderLive(tasks);
    const headers = [...container.querySelectorAll<HTMLElement>(".qp-group-state")];
    expect(headers.map((h) => h.querySelector(".qp-group-name")!.textContent)).toEqual(["결정 필요", "진행 중", "대기", "알 수 없는 상태 (parked)"]);
    expect(headers.map((h) => h.querySelector(".qp-group-count")!.textContent)).toEqual(["2", "1", "1", "1"]);
    expect(headers.every((h) => h.querySelector("svg[data-state-shape]") !== null)).toBe(true);
    expect(headers.every((h) => h.closest("h2") !== null && h.hasAttribute("aria-expanded"))).toBe(true);
    // rows under a group keep priority order, largest first
    const ids = [...container.querySelectorAll<HTMLElement>(".qp-row")].map((r) => Number(r.dataset.taskId));
    expect(ids).toEqual([2, 3, 4, 1, 5]);
  });

  it("collapsing a group hides its rows but keeps its count", () => {
    const { container } = renderLive(tasks);
    const decision = container.querySelector<HTMLElement>('[data-group="state:needs_decision"]')!;
    fireEvent.click(decision);
    expect(container.querySelector('[data-task-id="2"]')).toBeNull();
    expect(container.querySelector('[data-group="state:needs_decision"]')!.getAttribute("aria-expanded")).toBe("false");
    expect(container.querySelector('[data-group="state:needs_decision"] .qp-group-count')!.textContent).toBe("2");
  });

  it("a row is a real link to the task, and the selected row is marked", () => {
    const { container } = renderLive(tasks);
    const row = container.querySelector<HTMLAnchorElement>('[data-task-id="4"]')!;
    expect(row.tagName).toBe("A");
    expect(row.getAttribute("href")).toContain("task=4");
    fireEvent.click(row);
    const selected = container.querySelector<HTMLElement>('[data-task-id="4"]')!;
    expect(selected.classList.contains("selected")).toBe(true);
    expect(selected.getAttribute("aria-current")).toBe("true");
    // a modified click keeps the native new-tab behaviour
    const other = container.querySelector<HTMLAnchorElement>('[data-task-id="2"]')!;
    expect(fireEvent.click(other, { ctrlKey: true })).toBe(true);
  });

  it("sidebar: brand, global nav with the current page, views and saved views", () => {
    renderLive(tasks);
    const global = screen.getByRole("navigation", { name: "전역 탐색" });
    expect(global.querySelector('[aria-current="page"]')!.textContent).toBe("Queue");
    expect([...global.querySelectorAll("a")].map((a) => a.getAttribute("href"))).toEqual([
      "/ui/queue",
      "/ui/decisions",
      "/ui/fleet",
      "/ui/timeline",
      "/ui/compose",
    ]);
    // home does not exist yet — shown, but not as a link that leads nowhere
    expect(global.textContent).toContain("준비 중");
    expect(screen.getByRole("navigation", { name: "queue views" })).toBeTruthy();
    expect(screen.getByRole("navigation", { name: "saved views" })).toBeTruthy();
    expect(screen.getByRole("navigation", { name: "위치" }).textContent).toContain("Queue");
  });

  it("toolbar: search, list/board, 필터 n counting active filters", () => {
    renderLive(tasks);
    expect(screen.getByLabelText("search")).toBeTruthy();
    expect(screen.getByRole("group", { name: "보기" })).toBeTruthy();
    const summary = document.querySelector(".qp-filter summary")!;
    expect(summary.textContent).toBe("필터 0");
    fireEvent.change(screen.getByLabelText("lane filter"), { target: { value: "live-lane" } });
    expect(document.querySelector(".qp-filter summary")!.textContent).toBe("필터 1");
  });
});

// ---- area draft is out of the product ---------------------------------

describe("area→bundle draft grouping is local-preview only", () => {
  it("the live toolbar does not offer it", () => {
    renderLive([mkBoardTask({})]);
    const options = [...(screen.getByLabelText("grouping") as HTMLSelectElement).options].map((o) => o.value);
    expect(options).toEqual(["state", "none"]);
  });

  it("a stored or linked area grouping becomes state groups on live data", () => {
    localStorage.setItem(
      STORAGE_KEY,
      JSON.stringify({ schemaVersion: 1, current: { ...DEFAULT_STATE, view: "all", grouping: "area" }, views: {} }),
    );
    window.history.replaceState(null, "", "/ui/queue?view=all&group=area");
    const { container } = renderLive([mkBoardTask({})]);
    expect(container.querySelector(".qp-listwrap")!.getAttribute("data-grouping")).toBe("state");
    expect(container.querySelector(".qp-group-state")).toBeTruthy();
    expect(new URLSearchParams(window.location.search).get("group")).toBe("state");
  });
});

// ---- (e) state shapes: shape + label, never color alone -----------------

describe("state shapes", () => {
  it("nine canonical states plus unknown each have a distinct outline", () => {
    const keys = [...STATE_GROUP_ORDER, "unknown"];
    expect(new Set(keys).size).toBe(10);
    const markup = keys.map((k) => renderToStaticMarkup(<svg>{STATE_SHAPES[k]}</svg>));
    expect(new Set(markup).size).toBe(10);
    for (const s of KNOWN_STATES) {
      expect(STATE_LABEL[s], s).toBeTruthy();
    }
  });

  it("an icon always comes with a name: hidden label on rows, unsupported enum kept raw", () => {
    const html = renderToStaticMarkup(<StateIcon state="parked" srLabel />);
    expect(html).toContain('data-state-shape="unknown"');
    expect(html).toContain("알 수 없는 상태 (parked)");
    expect(html).toContain('aria-hidden="true"');
    const row = renderToStaticMarkup(<StateIcon state="needs_decision" srLabel />);
    expect(row).toContain("결정 필요");
    expect(row).toContain("hk-state-needs-decision");
  });
});

// ---- product isolation and bundle budget (AC8) --------------------------

function reachable(entry: string): string[] {
  const seen = new Set<string>();
  const stack = [entry];
  const re = /(?:^|\n)\s*(?:import|export)\s[^'";]*?from\s*["']([^"']+)["']|(?:^|\n)\s*import\s*["']([^"']+)["']/g;
  while (stack.length > 0) {
    const file = stack.pop()!;
    if (seen.has(file) || !existsSync(file)) continue;
    seen.add(file);
    for (const m of readFileSync(file, "utf8").matchAll(re)) {
      const spec = m[1] ?? m[2];
      if (!spec?.startsWith(".")) continue;
      const base = resolve(dirname(file), spec);
      for (const cand of [base, `${base}.ts`, `${base}.tsx`]) {
        if (existsSync(cand) && /\.(ts|tsx|css)$/.test(cand)) stack.push(cand);
      }
    }
  }
  return [...seen];
}

function staticClosure(entry: string): string[] {
  const seen = new Set<string>();
  const stack = [entry];
  while (stack.length > 0) {
    const name = stack.pop()!;
    if (seen.has(name)) continue;
    seen.add(name);
    const text = readFileSync(join(built, name), "utf8");
    stack.push(...[...text.matchAll(/(?:from|import)\s*"\.\/([^"]+)"/g)].map((m) => m[1]));
  }
  return [...seen];
}

describe("product entry and bundle budget", () => {
  it("the queue entry does not reach fixtures, preview or the measurement panel", () => {
    const files = reachable(join(here, "main.tsx"));
    expect(files.length).toBeGreaterThan(10);
    expect(files.filter((f) => /fixtures\.ts$|rng\.ts$|preview\.tsx$|MeasurePanel\.tsx$/.test(f))).toEqual([]);
  });

  // CSS raw budget raised 24→26KiB for #598 live-job chips/strip (base was ~24KiB
  // already — the guard still trips on real regressions, not this feature).
  it("initial queue JS ≤ raw 230KiB / gzip 75KiB; CSS ≤ raw 26KiB / gzip 8KiB", () => {
    const size = (names: string[]) =>
      names.reduce(
        (acc, n) => {
          const b = readFileSync(join(built, n));
          return { raw: acc.raw + b.length, gzip: acc.gzip + gzipSync(b, { level: 9 }).length };
        },
        { raw: 0, gzip: 0 },
      );
    const js = size(staticClosure("board.js"));
    expect(js.raw).toBeGreaterThan(50 * 1024); // counted the real closure
    expect(js.raw).toBeLessThanOrEqual(230 * 1024);
    expect(js.gzip).toBeLessThanOrEqual(75 * 1024);
    const css = size(["tokens.css", "board.css"]);
    expect(css.raw).toBeLessThanOrEqual(26 * 1024);
    expect(css.gzip).toBeLessThanOrEqual(8 * 1024);
  });

  it("no external request or font CDN in the served assets", () => {
    for (const name of ["tokens.css", "board.css", ...staticClosure("board.js")]) {
      const text = read(built, name);
      expect(text, name).not.toMatch(/@import|fonts\.googleapis|fonts\.gstatic|url\(\s*["']?https?:/);
    }
  });
});

// ---- first screen survives broken browser storage (PR #32 gate round) ----

describe("storage failures never stop the queue from mounting", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    delete document.documentElement.dataset.theme;
  });

  it("reading the localStorage property itself throws (storage-blocked browser)", () => {
    const original = Object.getOwnPropertyDescriptor(globalThis, "localStorage")!;
    Object.defineProperty(globalThis, "localStorage", {
      configurable: true,
      get() {
        throw new DOMException("storage disabled", "SecurityError");
      },
    });
    try {
      let theme: string | undefined;
      expect(() => (theme = loadTheme())).not.toThrow();
      expect(theme).toBe("dark");
      expect(() => applyTheme("light")).not.toThrow();
      expect(document.documentElement.dataset.theme).toBe("light");
      let view: ReturnType<typeof renderLive> | undefined;
      expect(() => (view = renderLive([mkBoardTask({})]))).not.toThrow();
      const container = view!.container;
      expect(container.querySelector('[data-task-id="7001"]')).toBeTruthy();
      // choosing a density still works for this page, it just is not saved
      fireEvent.click(screen.getByRole("button", { name: "행 56px · 2줄" }));
      expect(container.querySelector(".qp-listwrap")!.getAttribute("data-density")).toBe("comfortable");
    } finally {
      Object.defineProperty(globalThis, "localStorage", original);
    }
  });

  it("getItem and setItem throw", () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new DOMException("quota", "QuotaExceededError");
    });
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new DOMException("quota", "QuotaExceededError");
    });
    let theme: string | undefined;
    expect(() => (theme = loadTheme())).not.toThrow();
    expect(theme).toBe("dark");
    expect(() => applyTheme("dark")).not.toThrow();
    let view: ReturnType<typeof renderLive> | undefined;
    expect(() => (view = renderLive([mkBoardTask({})]))).not.toThrow();
    const container = view!.container;
    expect(container.querySelector('[data-task-id="7001"]')).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "행 56px · 2줄" }));
    expect(container.querySelector(".qp-listwrap")!.getAttribute("data-density")).toBe("comfortable");
  });
});

describe("a corrupted saved view is dropped, never crashes the first render", () => {
  const good = { view: "all", layout: "list", grouping: "state", density: "compact", filters: { query: "", lane: "", kind: "", hiddenStates: [], minPriority: null }, hiddenColumns: [] };

  it("drops malformed views and a malformed current state; valid ones still load", () => {
    localStorage.setItem(
      STORAGE_KEY,
      JSON.stringify({
        schemaVersion: 1,
        current: { view: "all", layout: "list", filters: {} },
        views: {
          "broken-null-filters": { view: "all", layout: "list", filters: null },
          "broken-states": { ...good, filters: { ...good.filters, hiddenStates: "merged" } },
          "broken-columns": { ...good, hiddenColumns: { a: 1 } },
          "ops-triage": { view: "all", layout: "list" },
          "my-view": { ...good, filters: { ...good.filters, lane: "live-lane" } },
        },
      }),
    );
    window.history.replaceState(null, "", "/ui/queue");
    let view: ReturnType<typeof renderLive> | undefined;
    expect(() => (view = renderLive([mkBoardTask({})]))).not.toThrow();
    const container = view!.container;
    const rail = document.getElementById("qp-rail")!;
    const saved = [...rail.querySelectorAll<HTMLElement>(".qp-rail-saved")].map((b) => b.textContent);
    expect(saved).toEqual(["ops-triage", "active-flow", "live-now", "backlog-scan", "my-view"]);
    // malformed current → product default (backlog view), not a crash
    expect(screen.getByRole("link", { name: "Backlog" }).getAttribute("aria-current")).toBe("page");
    // the shipped view keeps its default after the corrupt override is dropped
    fireEvent.click(screen.getByRole("button", { name: "ops-triage" }));
    expect(screen.getByRole("link", { name: "Operator" }).getAttribute("aria-current")).toBe("page");
    expect(() => fireEvent.click(screen.getByRole("button", { name: "my-view" }))).not.toThrow();
    expect(screen.getByRole("link", { name: "All" }).getAttribute("aria-current")).toBe("page");
    expect(container.querySelector('[data-task-id="7001"]')).toBeTruthy();
  });
});
