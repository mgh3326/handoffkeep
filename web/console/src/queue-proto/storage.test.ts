import { describe, expect, it } from "vitest";
import { DEFAULT_STATE, loadPresentation, NAMED_VIEWS, savePresentation, SCHEMA_VERSION, STORAGE_KEY } from "./storage";

function memStorage(seed?: string): Storage {
  let data: Record<string, string> = seed ? { [STORAGE_KEY]: seed } : {};
  return {
    getItem: (k: string) => data[k] ?? null,
    setItem: (k: string, v: string) => {
      data[k] = v;
    },
    removeItem: (k: string) => {
      delete data[k];
    },
    clear: () => {
      data = {};
    },
    key: () => null,
    get length() {
      return Object.keys(data).length;
    },
  };
}

describe("versioned saved presentation state", () => {
  it("provides at least 3 named local views", () => {
    expect(Object.keys(NAMED_VIEWS).length).toBeGreaterThanOrEqual(3);
  });

  it.each(Object.keys(NAMED_VIEWS))("named view %s restores reload-equivalent state", (name) => {
    const storage = memStorage();
    const saved = NAMED_VIEWS[name];
    savePresentation({ ...DEFAULT_STATE, ...saved, collapsedGroups: [] }, NAMED_VIEWS, storage);
    const loaded = loadPresentation(storage);
    expect(loaded.versionMismatch).toBe(false);
    expect(loaded.state.view).toBe(saved.view);
    expect(loaded.state.layout).toBe(saved.layout);
    expect(loaded.state.grouping).toBe(saved.grouping);
    expect(loaded.state.density).toBe(saved.density);
    expect(loaded.views[name]).toEqual(saved);
  });

  it("round-trips filters/grouping/columns/density", () => {
    const storage = memStorage();
    const state = {
      ...DEFAULT_STATE,
      view: "all" as const,
      grouping: "area" as const,
      density: "comfortable" as const,
      hiddenColumns: ["merged", "dropped"],
      filters: { query: "큐", lane: "synth-lane-ops", kind: "fix", hiddenStates: ["dropped"], minPriority: 50 },
    };
    savePresentation(state, NAMED_VIEWS, storage);
    const loaded = loadPresentation(storage);
    expect(loaded.state).toMatchObject({
      view: "all",
      grouping: "area",
      density: "comfortable",
      hiddenColumns: ["merged", "dropped"],
    });
    expect(loaded.state.filters.minPriority).toBe(50);
  });

  it("version mismatch falls back explicitly: defaults + mismatch signal", () => {
    const storage = memStorage(JSON.stringify({ schemaVersion: SCHEMA_VERSION + 1, current: { view: "all" }, views: {} }));
    const loaded = loadPresentation(storage);
    expect(loaded.versionMismatch).toBe(true);
    expect(loaded.state).toEqual(DEFAULT_STATE);
    // never a silent empty result — views still populated
    expect(Object.keys(loaded.views).length).toBeGreaterThanOrEqual(3);
  });

  it("corrupt payload also falls back explicitly", () => {
    const storage = memStorage("{not json");
    const loaded = loadPresentation(storage);
    expect(loaded.versionMismatch).toBe(true);
    expect(loaded.state).toEqual(DEFAULT_STATE);
  });

  it("empty storage loads defaults without a mismatch flag", () => {
    const loaded = loadPresentation(memStorage());
    expect(loaded.versionMismatch).toBe(false);
    expect(loaded.state).toEqual(DEFAULT_STATE);
  });
});
