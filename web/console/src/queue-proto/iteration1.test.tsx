import { existsSync, readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { beforeEach, describe, expect, it } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { QueueProtoApp } from "./QueueProtoApp";
import { EXPERIMENT_SCRIPTS } from "./MeasurePanel";
import { buildDatasets, buildStale63, GENERATED_AT, STALE63_BUCKETS } from "./fixtures";
import { ageDays, applyView, EMPTY_FILTERS, isStale, STALE_MIN_AGE_DAYS } from "./adapter";
import type { ProtoTask } from "./types";

const datasets = buildDatasets();
const stale = datasets.stale63;

function mkTask(over: Partial<ProtoTask>): ProtoTask {
  return {
    id: 1,
    title: "synthetic probe",
    kind: "fix",
    state: "backlog",
    lane: "synth-lane-ops",
    claimant: null,
    priority: 10,
    created_at: GENERATED_AT,
    state_entered_at: GENERATED_AT,
    due_at: null,
    blocker: null,
    created_by: "synth-op",
    refs: {},
    events: [],
    dwell: [],
    coverage: { status: "not_collected", participants: null },
    ...over,
  };
}

describe("view rail — semantic navigation, not tabs", () => {
  beforeEach(() => {
    localStorage.clear();
    window.history.replaceState(null, "", "/queue-proto.html");
  });

  it("renders a navigation landmark with links carrying hrefs and aria-current", () => {
    render(<QueueProtoApp datasets={datasets} initialSet="sample200" />);
    const rail = document.getElementById("qp-rail")!;
    expect(within(rail as HTMLElement).getByRole("navigation", { name: "queue views" })).toBeTruthy();
    expect(rail.querySelector('[role="tab"], [role="tablist"]')).toBeNull();
    const all = screen.getByRole("link", { name: "All" });
    expect(all.getAttribute("href")).toContain("view=all");
    // default view = backlog → its link is current
    expect(screen.getByRole("link", { name: "Backlog" }).getAttribute("aria-current")).toBe("page");
    expect(all.getAttribute("aria-current")).toBeNull();
  });

  it("per-view counts equal the filtered row set the view renders", () => {
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="sample200" />);
    for (const [view, label] of [
      ["operator", "Operator"],
      ["backlog", "Backlog"],
      ["all", "All"],
    ] as const) {
      const link = screen.getByRole("link", { name: label });
      const shown = Number(link.querySelector(".qp-nav-count")!.textContent);
      expect(shown).toBe(applyView(datasets.sample200.tasks, { view, filters: EMPTY_FILTERS }).length);
    }
    // counts follow filters, not the full fixture
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    fireEvent.change(screen.getByLabelText("search"), { target: { value: "#1150" } });
    expect(Number(screen.getByRole("link", { name: "All" }).querySelector(".qp-nav-count")!.textContent)).toBe(1);
    expect(container.querySelectorAll(".qp-row").length).toBe(1);
  });

  it("partial fixtures show N+ language; complete fixtures show the exact count", () => {
    const { unmount } = render(<QueueProtoApp datasets={datasets} initialSet="sample200" />);
    const rail = document.getElementById("qp-rail")!;
    expect(rail.textContent).toContain("200+");
    expect(rail.textContent).toContain("partial");
    unmount();
    render(<QueueProtoApp datasets={datasets} initialSet="stale63" />);
    expect(document.getElementById("qp-rail")!.textContent).toContain("63 fixture tasks");
    expect(document.getElementById("qp-rail")!.textContent).not.toContain("63+");
  });

  it("sidebar collapses via the header toggle and reopens", () => {
    render(<QueueProtoApp datasets={datasets} initialSet="sample200" />);
    const root = document.querySelector(".qp-root")!;
    const toggle = screen.getByRole("button", { name: /views/ });
    expect(root.classList.contains("rail-collapsed")).toBe(false);
    fireEvent.click(toggle);
    expect(root.classList.contains("rail-collapsed")).toBe(true);
    fireEvent.click(toggle);
    expect(root.classList.contains("rail-collapsed")).toBe(false);
  });
});

describe("dense grouped list — adversarial: rail without list must fail", () => {
  beforeEach(() => {
    localStorage.clear();
    window.history.replaceState(null, "", "/queue-proto.html");
  });

  it("sidebar and the dense grouped list are both present with real rows", () => {
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="stale63" />);
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    expect(document.getElementById("qp-rail")).toBeTruthy();
    expect(container.querySelectorAll(".qp-group").length).toBeGreaterThanOrEqual(2);
    expect(container.querySelectorAll(".qp-row").length).toBe(63);
    expect(container.querySelector(".qp-colhead")!.textContent).toContain("created age");
    expect(container.querySelector(".qp-colhead")!.textContent).toContain("state age");
  });

  it("group header counts come from the full filtered fixture, not rendered rows", () => {
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="stale63" />);
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    const headers = [...container.querySelectorAll<HTMLElement>(".qp-group")];
    let headerTotal = 0;
    for (const h of headers) {
      const badge = h.querySelector<HTMLElement>(".qp-badge")!;
      headerTotal += Number(badge.textContent);
    }
    expect(headerTotal).toBe(63);
    // collapse every header: zero task rows rendered, counts unchanged
    for (const h of headers) {
      fireEvent.click(h);
    }
    expect(container.querySelectorAll(".qp-row").length).toBe(0);
    let collapsedTotal = 0;
    for (const h of container.querySelectorAll<HTMLElement>(".qp-group")) {
      collapsedTotal += Number(h.querySelector<HTMLElement>(".qp-badge")!.textContent);
    }
    expect(collapsedTotal).toBe(63);
    expect(container.textContent).toContain("63 unique tasks");
  });
});

describe("staleness — explicit threshold, deterministic buckets", () => {
  const NOW = GENERATED_AT; // 2026-09-19T09:00:00+09:00

  it("isStale: non-terminal + age ≥ 7d, boundary exact, terminal excluded", () => {
    expect(isStale(mkTask({ created_at: "2026-09-12T09:00:00+09:00" }), NOW)).toBe(true); // exactly 7d
    expect(isStale(mkTask({ created_at: "2026-09-12T09:01:00+09:00" }), NOW)).toBe(false); // 6d23h
    expect(isStale(mkTask({ created_at: "2026-09-19T08:00:00+09:00" }), NOW)).toBe(false); // 1h
    expect(isStale(mkTask({ created_at: "2026-08-20T00:00:00+09:00", state: "merged" }), NOW)).toBe(false); // terminal
    expect(isStale(mkTask({ created_at: "2026-08-20T00:00:00+09:00", state: "dropped" }), NOW)).toBe(false);
    expect(STALE_MIN_AGE_DAYS).toBe(7);
  });

  it("stale63 reproduces the 6 / 14 / 17 / 24 age distribution over 63 tasks", () => {
    const built = buildStale63();
    expect(built.tasks).toHaveLength(63);
    const buckets = { under24h: 0, days1to3: 0, days3to7: 0, days7plus: 0, unknownAge: 0 };
    for (const t of built.tasks) {
      const days = ageDays(built.generatedAt, t.created_at);
      if (days === null) buckets.unknownAge += 1;
      else if (days < 1) buckets.under24h += 1;
      else if (days < 3) buckets.days1to3 += 1;
      else if (days < 7) buckets.days3to7 += 1;
      else buckets.days7plus += 1;
    }
    expect(buckets).toEqual(STALE63_BUCKETS);
    expect(buckets).toEqual({ under24h: 6, days1to3: 14, days3to7: 17, days7plus: 24, unknownAge: 2 });
    // exactly the ≥7d bucket is stale
    expect(built.tasks.filter((t) => isStale(t, built.generatedAt))).toHaveLength(24);
    // every task is non-terminal — age never implies an outcome
    expect(built.tasks.every((t) => t.state !== "merged" && t.state !== "dropped")).toBe(true);
    // determinism
    expect(buildStale63().tasks.map((t) => t.created_at)).toEqual(built.tasks.map((t) => t.created_at));
  });

  it("trial anchors: exactly one urgent task, one blocked task, one ownership chain", () => {
    expect(stale.tasks.filter((t) => t.priority >= 90)).toHaveLength(1);
    expect(stale.tasks.find((t) => t.priority >= 90)!.id).toBe(6130);
    const blocked = stale.tasks.filter((t) => t.blocker !== null);
    expect(blocked.map((t) => t.id)).toEqual([6150]);
    expect(blocked[0].state).toBe("hold");
    const handoff = stale.tasks.find((t) => t.id === 6140)!;
    expect(handoff.events.map((e) => e.by)).toEqual(["synth-claim-1", "synth-claim-2", "synth-claim-3"]);
    expect(handoff.claimant).toBe("synth-claim-3");
  });

  it("rows mark stale items textually and never imply executability", () => {
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="stale63" />);
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    const badges = container.querySelectorAll(".qp-stale");
    expect(badges.length).toBe(24);
    for (const b of badges) {
      expect(b.textContent).toContain("stale");
    }
    // no element may claim a stale task is ready/safe to run
    expect(container.textContent).not.toContain("ready to execute");
    expect(container.textContent).not.toContain("ready to run");
    // the urgent anchor is urgent but not stale (5d) — the two signals stay distinct
    const urgentRow = container.querySelector('[data-task-id="6130"]')!;
    expect(urgentRow.textContent).toContain("p95");
    expect(urgentRow.querySelector(".qp-stale")).toBeNull();
  });

  it("drawer shows the stale note and exact timestamp labels", () => {
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="stale63" />);
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    const row = container.querySelector<HTMLElement>('[data-task-id="6150"]')!;
    fireEvent.keyDown(row, { key: "Enter" });
    const drawer = screen.getByRole("dialog");
    expect(drawer.textContent).toContain("stale");
    expect(drawer.textContent).toContain("does not imply the premise is still valid or safe to execute");
    expect(drawer.textContent).toContain("since created_at");
    expect(drawer.textContent).toContain("since state_entered_at");
    expect(drawer.textContent).toContain("synth blocker: awaiting external review verdict");
  });
});

describe("navigation, persistence and keyboard flow", () => {
  beforeEach(() => {
    localStorage.clear();
    window.history.replaceState(null, "", "/queue-proto.html");
  });

  it("view links push history entries; popstate restores the prior view", () => {
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="sample200" />);
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    expect(window.location.search).toContain("view=all");
    expect(container.querySelectorAll(".qp-row").length).toBe(200);
    // simulate Back: the previous entry is the normalized initial URL
    window.history.replaceState(null, "", "/queue-proto.html?view=backlog&layout=list");
    fireEvent(window, new PopStateEvent("popstate"));
    expect(screen.getByRole("link", { name: "Backlog" }).getAttribute("aria-current")).toBe("page");
    // selected state and row-set agree after restore
    const expected = applyView(datasets.sample200.tasks, { view: "backlog", filters: EMPTY_FILTERS }).length;
    expect(container.querySelectorAll(".qp-row").length).toBe(expected);
  });

  it("select view → search → open → inspect → close → return to row, 5/5 with zero focus loss", () => {
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="stale63" />);
    for (let i = 0; i < 5; i++) {
      fireEvent.click(screen.getByRole("link", { name: "All" }));
      fireEvent.change(screen.getByLabelText("search"), { target: { value: "#6130" } });
      const row = container.querySelector<HTMLElement>('[data-task-id="6130"]')!;
      row.focus();
      fireEvent.keyDown(row, { key: "Enter" });
      const drawer = screen.getByRole("dialog");
      expect(drawer.textContent).toContain("p95");
      expect(document.activeElement).toBe(drawer);
      fireEvent.keyDown(drawer, { key: "Escape" });
      expect(screen.queryByRole("dialog")).toBeNull();
      expect(document.activeElement).toBe(row);
      fireEvent.change(screen.getByLabelText("search"), { target: { value: "" } });
    }
  });

  it("list scroll position survives a drawer round-trip", () => {
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="stale63" />);
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    const list = container.querySelector<HTMLElement>(".qp-list")!;
    list.scrollTop = 456;
    const row = container.querySelector<HTMLElement>('[data-task-id="6101"]')!;
    fireEvent.keyDown(row, { key: "Enter" });
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    expect(list.scrollTop).toBe(456);
    expect(document.activeElement).toBe(row);
  });

  it("saved views live in the rail and apply their state", () => {
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="sample200" />);
    const rail = document.getElementById("qp-rail")!;
    fireEvent.click(within(rail as HTMLElement).getByRole("button", { name: "backlog-scan" }));
    // backlog-scan = backlog + list + area grouping
    expect(screen.getByRole("link", { name: "Backlog" }).getAttribute("aria-current")).toBe("page");
    expect(container.querySelectorAll(".qp-group").length).toBeGreaterThan(0);
  });
});

describe("trial harness — in-app scripts match the graded TRIAL.md answers", () => {
  it("I1 names the All view and the truthful total of 63 in both places", () => {
    const i1 = EXPERIMENT_SCRIPTS.find((s) => s.id === "I1");
    expect(i1, "I1 script must exist").toBeTruthy();
    expect(i1!.text).toContain("All view");
    expect(i1!.text).not.toMatch(/\bActive\b/);
    const protoDir = dirname(fileURLToPath(import.meta.url));
    const trial = readFileSync(resolve(protoDir, "evidence", "TRIAL.md"), "utf8");
    const i1row = trial.split("\n").find((l) => l.startsWith("| I1 |"));
    expect(i1row, "TRIAL.md must carry a graded I1 row").toBeTruthy();
    expect(i1row).toContain("All");
    expect(i1row).toContain("63 unique tasks");
    expect(i1row).not.toMatch(/\bActive\b/);
  });
});

describe("production isolation — adversarial: proto/measurement leak must fail", () => {
  const srcRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");

  /** Walk the static import graph from the production entries; assert no
   * reachable module lives under src/queue-proto/. */
  function reachableFromProdEntries(): string[] {
    const entries = ["main.tsx", "board.tsx"].map((f) => resolve(srcRoot, f));
    const seen = new Set<string>();
    const stack = [...entries];
    const importRe = /(?:import|export)[^'"]*?from\s+["']([^"']+)["']|import\s*\(\s*["']([^"']+)["']/g;
    while (stack.length > 0) {
      const file = stack.pop()!;
      if (seen.has(file) || !existsSync(file)) {
        continue;
      }
      seen.add(file);
      const text = readFileSync(file, "utf8");
      for (const m of text.matchAll(importRe)) {
        const spec = m[1] ?? m[2];
        if (!spec?.startsWith(".")) {
          continue; // package imports can't reach queue-proto
        }
        const base = resolve(dirname(file), spec);
        for (const cand of [base, `${base}.ts`, `${base}.tsx`, `${base}.css`, `${base}/index.ts`, `${base}/index.tsx`]) {
          if (existsSync(cand) && /\.(ts|tsx)$/.test(cand)) {
            stack.push(cand);
          }
        }
      }
    }
    return [...seen];
  }

  it("no production entry transitively imports queue-proto", () => {
    const reachable = reachableFromProdEntries();
    expect(reachable.length).toBeGreaterThan(5); // graph walk actually ran
    const leaked = reachable.filter((f) => f.includes("queue-proto"));
    expect(leaked).toEqual([]);
  });

  it("vite production config has no queue-proto input", () => {
    const cfg = readFileSync(resolve(srcRoot, "..", "vite.config.ts"), "utf8");
    expect(cfg).not.toContain("queue-proto");
    expect(cfg).toContain('fleet: resolve(root, "src/main.tsx")');
    expect(cfg).toContain('board: resolve(root, "src/board.tsx")');
  });
});
