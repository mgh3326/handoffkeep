import { describe, expect, it } from "vitest";
import { applyView, boardColumns, EMPTY_FILTERS, groupByArea, groupTaskIds, matchesQuery, statusLine } from "./adapter";
import { buildDatasets, buildPerf5000, buildSample200, PREVIEW_CLAMP } from "./fixtures";
import { flattenGrouped } from "./ListView";
import type { FilterState, ProtoView } from "./types";

const sample = buildSample200();
const datasets = buildDatasets();
const edge = datasets.edge;

function filters(part: Partial<FilterState>): FilterState {
  return { ...EMPTY_FILTERS, ...part };
}

function boardIds(visibleIds: ReturnType<typeof applyView>, view: ProtoView): Set<number> {
  const cols = boardColumns(visibleIds, view, []);
  return new Set(cols.flatMap((c) => c.tasks.map((t) => t.id)));
}

describe("shared adapter — identical task sets for both renderers", () => {
  it("default state (backlog view): list IDs === board column IDs", () => {
    const visible = applyView(sample.tasks, { view: "backlog", filters: EMPTY_FILTERS });
    expect(visible.length).toBeGreaterThan(0);
    expect(boardIds(visible, "backlog")).toEqual(new Set(visible.map((t) => t.id)));
  });

  it.each([
    ["korean substring", "큐"],
    ["mixed-case english", "DrAwEr"],
    ["numeric id", "1150"],
    ["#id form", "#1150"],
    ["text after clamp boundary", "CLAMPED-TAIL-MARKER-1150"],
  ])("view=all, query %s → identical unique ID set per layout", (_name, query) => {
    const visible = applyView(sample.tasks, { view: "all", filters: filters({ query }) });
    const listIds = new Set(visible.map((t) => t.id));
    expect(boardIds(visible, "all")).toEqual(listIds);
    expect(listIds.size).toBe(visible.length); // unique
  });

  it("search text after the clamp boundary finds the task but stays out of the preview", () => {
    const visible = applyView(sample.tasks, { view: "all", filters: filters({ query: "CLAMPED-TAIL-MARKER-1150" }) });
    expect(visible.map((t) => t.id)).toEqual([1150]);
    const preview = visible[0].title.slice(0, PREVIEW_CLAMP);
    expect(preview).not.toContain("CLAMPED-TAIL-MARKER");
    expect(visible[0].title).toContain("CLAMPED-TAIL-MARKER-1150");
  });
});

describe("matchesQuery", () => {
  const t = { id: 42, title: "Fix 큐 drawer 검색 MixedCase" } as never;
  it("matches #id, numeric id, korean, mixed-case", () => {
    expect(matchesQuery(t, "#42")).toBe(true);
    expect(matchesQuery(t, "42")).toBe(true);
    expect(matchesQuery(t, "검색")).toBe(true);
    expect(matchesQuery(t, "mixedcase")).toBe(true);
    expect(matchesQuery(t, "#43")).toBe(false);
    expect(matchesQuery(t, "")).toBe(true);
  });
});

describe("board columns are bounded", () => {
  it("never renders all nine state columns by default", () => {
    const visible = applyView(sample.tasks, { view: "backlog", filters: EMPTY_FILTERS });
    const cols = boardColumns(visible, "backlog", []);
    expect(cols.length).toBeLessThanOrEqual(2); // backlog + hold only
    for (const col of cols) {
      expect(col.tasks.length).toBeGreaterThan(0);
    }
  });

  it("all view still shows only populated columns and hides via hiddenColumns", () => {
    const visible = applyView(sample.tasks, { view: "all", filters: EMPTY_FILTERS });
    const cols = boardColumns(visible, "all", []);
    expect(cols.length).toBeLessThan(9);
    const hidden = boardColumns(visible, "all", [cols[0].state]);
    expect(hidden.find((c) => c.state === cols[0].state)).toBeUndefined();
  });
});

describe("grouping", () => {
  const visible = applyView(edge.tasks, { view: "all", filters: EMPTY_FILTERS });
  const groups = groupByArea(visible, edge.enrichment, edge.generatedAt);

  it("collapsed unique counts equal the union of expanded member IDs", () => {
    for (const g of groups) {
      const ids = groupTaskIds(g);
      expect(g.signals.count).toBe(ids.size);
      for (const b of g.bundles) {
        expect(b.signals.count).toBe(b.tasks.length);
      }
    }
    // union over all groups == full visible set
    const allIds = new Set<number>();
    for (const g of groups) {
      for (const id of groupTaskIds(g)) {
        allIds.add(id);
      }
    }
    expect(allIds).toEqual(new Set(visible.map((t) => t.id)));
  });

  it("always shows standalone and unclassified groups", () => {
    const names = groups.map((g) => g.name);
    expect(names).toContain("standalone");
    expect(names).toContain("unclassified");
    const unclassified = groups.find((g) => g.name === "unclassified")!;
    expect(unclassified.signals.count).toBeGreaterThan(0);
  });

  it("urgent task inside a collapsed group remains signaled", () => {
    // task 5004 (p97) lives in synth-bundle-edge
    const consoleGroup = groups.find((g) => g.name === "synth-area-console")!;
    const edgeBundle = consoleGroup.bundles.find((b) => b.name === "synth-bundle-edge")!;
    expect(edgeBundle.signals.urgent).toBe(1);
    expect(edgeBundle.signals.count).toBe(2); // 5003 + 5004
    // collapsing the bundle removes rows but keeps the signal
    const rows = flattenGrouped(groups, [edgeBundle.key]);
    expect(rows.find((r) => r.kind === "task" && r.task.id === 5004)).toBeUndefined();
    const header = rows.find((r) => r.kind === "group" && r.key === edgeBundle.key)!;
    expect(header.kind === "group" && header.signals.urgent).toBe(1);
  });

  it("duplicate-candidate pair and implement/verify pair stay distinct tasks", () => {
    const ids = new Set(visible.map((t) => t.id));
    for (const id of [5007, 5008, 5009, 5010]) {
      expect(ids).toContain(id);
    }
    expect(edge.enrichment[5007].relations[0].type).toBe("duplicate-candidate");
    expect(edge.enrichment[5009].relations[0].type).toBe("verifies");
    expect(edge.enrichment[5010].relations[0].type).toBe("implements");
  });
});

describe("count conservation on the 5000-task set", () => {
  it("no silent cap: all 5000 IDs flow through the adapter", () => {
    const perf = buildPerf5000();
    const visible = applyView(perf.tasks, { view: "all", filters: EMPTY_FILTERS });
    expect(visible).toHaveLength(5000);
    expect(new Set(visible.map((t) => t.id)).size).toBe(5000);
  });

  it("grouped counts conserve the unique task union", () => {
    const perf = buildPerf5000();
    const visible = applyView(perf.tasks, { view: "all", filters: EMPTY_FILTERS });
    const groups = groupByArea(visible, perf.enrichment, perf.generatedAt);
    const allIds = new Set<number>();
    let headerTotal = 0;
    for (const g of groups) {
      for (const id of groupTaskIds(g)) {
        allIds.add(id);
      }
      headerTotal += g.signals.count;
    }
    expect(allIds.size).toBe(5000);
    expect(headerTotal).toBe(5000);
  });
});

describe("status line", () => {
  it("declares completeness wording truthfully", () => {
    expect(statusLine(sample)).toContain("completeness: partial");
    expect(statusLine(datasets.perf5000)).toContain("completeness: complete");
    expect(statusLine(sample)).toContain("synthetic");
  });
});
