// Versioned local presentation state. The ONLY thing persisted is view /
// filter / grouping / columns / density — never task bodies or credentials.

import type { ProtoState } from "./types";
import { EMPTY_FILTERS } from "./adapter";

export const STORAGE_KEY = "queue-proto:presentation:v1";
export const SCHEMA_VERSION = 1;
export const MEASURE_KEY = "queue-proto:measurements:v1";

export type SavedView = Pick<ProtoState, "view" | "layout" | "grouping" | "density" | "filters" | "hiddenColumns">;

export const DEFAULT_STATE: ProtoState = {
  view: "backlog",
  layout: "list",
  grouping: "none",
  density: "compact",
  filters: EMPTY_FILTERS,
  hiddenColumns: [],
  collapsedGroups: [],
};

// ≥3 named local views shipped with the prototype.
export const NAMED_VIEWS: Record<string, SavedView> = {
  "ops-triage": {
    view: "operator",
    layout: "list",
    grouping: "none",
    density: "compact",
    filters: EMPTY_FILTERS,
    hiddenColumns: [],
  },
  "active-flow": {
    view: "active",
    layout: "board",
    grouping: "none",
    density: "compact",
    filters: EMPTY_FILTERS,
    hiddenColumns: [],
  },
  "backlog-scan": {
    view: "backlog",
    layout: "list",
    grouping: "area",
    density: "compact",
    filters: EMPTY_FILTERS,
    hiddenColumns: [],
  },
};

export type PersistedPayload = {
  schemaVersion: number;
  current: SavedView;
  views: Record<string, SavedView>;
};

export type LoadResult = {
  state: ProtoState;
  views: Record<string, SavedView>;
  /** true when stored data had a different schemaVersion — caller must show
   * an explicit reset notice, never a silent empty result. */
  versionMismatch: boolean;
};

function toView(state: ProtoState): SavedView {
  return {
    view: state.view,
    layout: state.layout,
    grouping: state.grouping,
    density: state.density,
    filters: state.filters,
    hiddenColumns: state.hiddenColumns,
  };
}

function sane(saved: unknown): saved is SavedView {
  if (typeof saved !== "object" || saved === null) {
    return false;
  }
  const s = saved as SavedView;
  return ["operator", "active", "backlog", "all"].includes(s.view) && ["list", "board"].includes(s.layout) && typeof s.filters === "object";
}

export function loadPresentation(storage: Pick<Storage, "getItem"> = localStorage): LoadResult {
  let raw: string | null = null;
  try {
    raw = storage.getItem(STORAGE_KEY);
  } catch {
    raw = null;
  }
  if (raw === null) {
    return { state: DEFAULT_STATE, views: NAMED_VIEWS, versionMismatch: false };
  }
  try {
    const parsed = JSON.parse(raw) as PersistedPayload;
    if (parsed.schemaVersion !== SCHEMA_VERSION) {
      // Explicit fallback: defaults + a visible signal, never a silent empty view.
      return { state: DEFAULT_STATE, views: NAMED_VIEWS, versionMismatch: true };
    }
    const state: ProtoState = sane(parsed.current) ? { ...DEFAULT_STATE, ...parsed.current, collapsedGroups: [] } : DEFAULT_STATE;
    const views = typeof parsed.views === "object" && parsed.views !== null ? { ...NAMED_VIEWS, ...parsed.views } : NAMED_VIEWS;
    return { state, views, versionMismatch: false };
  } catch {
    return { state: DEFAULT_STATE, views: NAMED_VIEWS, versionMismatch: true };
  }
}

export function savePresentation(state: ProtoState, views: Record<string, SavedView>, storage: Pick<Storage, "setItem"> = localStorage): void {
  try {
    const payload: PersistedPayload = { schemaVersion: SCHEMA_VERSION, current: toView(state), views };
    storage.setItem(STORAGE_KEY, JSON.stringify(payload));
  } catch {
    // localStorage may be unavailable — presentation state simply won't persist.
  }
}
