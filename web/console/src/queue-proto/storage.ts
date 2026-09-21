// Versioned local presentation state. The ONLY thing persisted is view /
// filter / grouping / columns / density — never task bodies or credentials.

import type { ProtoState } from "./types";
import { EMPTY_FILTERS } from "./adapter";

export const STORAGE_KEY = "queue-proto:presentation:v1";
export const SCHEMA_VERSION = 1;
export const MEASURE_KEY = "queue-proto:measurements:v1";

export type SavedView = Pick<ProtoState, "view" | "layout" | "grouping" | "density" | "filters" | "hiddenColumns">;

// Product default: state groups, 40px single-line rows (operator 09-21,
// hk:doc 2408 §2 AC3). The view stays "backlog" — design does not change
// which tasks the queue opens on.
export const DEFAULT_STATE: ProtoState = {
  view: "backlog",
  layout: "list",
  grouping: "state",
  density: "compact",
  filters: EMPTY_FILTERS,
  hiddenColumns: [],
  collapsedGroups: [],
  sidebarCollapsed: false,
};

/** The synthetic local preview keeps its prior area→bundle draft default so
 * the fixture harness measures what it always measured; only the live
 * product switched to state groups. */
export const PREVIEW_DEFAULT_STATE: ProtoState = { ...DEFAULT_STATE, grouping: "area" };

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
    grouping: "state",
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

/** Grouping/density outside the known values fall back to the defaults
 * rather than reaching the renderer as an unhandled string. */
function withKnownPresentation<T extends Partial<SavedView>>(saved: T): T {
  const out = { ...saved };
  if (out.grouping !== undefined && !["state", "none", "area"].includes(out.grouping)) {
    out.grouping = DEFAULT_STATE.grouping;
  }
  if (out.density !== undefined && !["compact", "comfortable"].includes(out.density)) {
    out.density = DEFAULT_STATE.density;
  }
  return out;
}

export function loadPresentation(storage: Pick<Storage, "getItem"> = localStorage, defaults: ProtoState = DEFAULT_STATE): LoadResult {
  let raw: string | null = null;
  try {
    raw = storage.getItem(STORAGE_KEY);
  } catch {
    raw = null;
  }
  if (raw === null) {
    return { state: defaults, views: NAMED_VIEWS, versionMismatch: false };
  }
  try {
    const parsed = JSON.parse(raw) as PersistedPayload;
    if (parsed.schemaVersion !== SCHEMA_VERSION) {
      // Explicit fallback: defaults + a visible signal, never a silent empty view.
      return { state: defaults, views: NAMED_VIEWS, versionMismatch: true };
    }
    const state: ProtoState = sane(parsed.current)
      ? { ...defaults, ...withKnownPresentation(parsed.current), collapsedGroups: [] }
      : defaults;
    const storedViews: Record<string, SavedView> = {};
    if (typeof parsed.views === "object" && parsed.views !== null) {
      for (const [name, view] of Object.entries(parsed.views)) {
        storedViews[name] = withKnownPresentation(view);
      }
    }
    const views = { ...NAMED_VIEWS, ...storedViews };
    return { state, views, versionMismatch: false };
  } catch {
    return { state: defaults, views: NAMED_VIEWS, versionMismatch: true };
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
