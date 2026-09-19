// Single shared adapter. List and board both consume applyView() output, so
// switching layouts can never change the visible unique task-ID set.

import { KNOWN_STATES, type Dataset, type Enrichment, type FilterState, type ProtoState, type ProtoTask, type ProtoView } from "./types";

export const EMPTY_FILTERS: FilterState = { query: "", lane: "", kind: "", hiddenStates: [], minPriority: null };

export const VIEW_STATES: Record<ProtoView, string[]> = {
  operator: [...KNOWN_STATES],
  active: ["claimed", "in_progress", "verifying", "join", "needs_decision"],
  backlog: ["backlog", "hold"],
  all: [...KNOWN_STATES],
};

export function viewPredicate(view: ProtoView): (task: ProtoTask) => boolean {
  if (view === "operator") {
    // Operator view: anything waiting on an operator decision plus backlog.
    return (task) =>
      task.state === "backlog" || task.state === "needs_decision" || (task.kind === "decide" && task.state !== "merged" && task.state !== "dropped");
  }
  const allowed = new Set(VIEW_STATES[view]);
  return (task) => allowed.has(task.state);
}

export function clampPreview(title: string, clamp: number): string {
  return title.length > clamp ? `${title.slice(0, clamp)}…` : title;
}

/** Search matches the FULL original title (never the clamped preview) plus
 * the numeric task ID in both `123` and `#123` forms, case-insensitive. */
export function matchesQuery(task: ProtoTask, rawQuery: string): boolean {
  const query = rawQuery.trim();
  if (query === "") {
    return true;
  }
  const hashId = /^#(\d+)$/.exec(query);
  if (hashId) {
    return task.id === Number(hashId[1]);
  }
  const lower = query.toLowerCase();
  if (/^\d+$/.test(lower) && task.id === Number(lower)) {
    return true;
  }
  return task.title.toLowerCase().includes(lower);
}

export function matchesFilters(task: ProtoTask, filters: FilterState): boolean {
  if (filters.lane !== "" && task.lane !== filters.lane) {
    return false;
  }
  if (filters.kind !== "" && task.kind !== filters.kind) {
    return false;
  }
  if (filters.hiddenStates.includes(task.state)) {
    return false;
  }
  if (filters.minPriority !== null && task.priority < filters.minPriority) {
    return false;
  }
  return matchesQuery(task, filters.query);
}

export function taskOrder(a: ProtoTask, b: ProtoTask): number {
  if (a.priority !== b.priority) {
    return b.priority - a.priority;
  }
  if (a.created_at !== b.created_at) {
    return a.created_at < b.created_at ? -1 : 1;
  }
  return a.id - b.id;
}

/** The one shared path. Both renderers display exactly this set. */
export function applyView(tasks: ProtoTask[], state: Pick<ProtoState, "view" | "filters">): ProtoTask[] {
  const inView = viewPredicate(state.view);
  return tasks.filter((task) => inView(task) && matchesFilters(task, state.filters)).sort(taskOrder);
}

// ---- grouping -----------------------------------------------------------

export type GroupSignals = {
  count: number;
  decision: number;
  hold: number;
  unknown: number;
  urgent: number;
};

export type BundleGroup = { key: string; name: string; tasks: ProtoTask[]; signals: GroupSignals };
export type AreaGroup = { key: string; name: string; bundles: BundleGroup[]; signals: GroupSignals };

export function taskSignals(task: ProtoTask): { decision: boolean; hold: boolean; unknown: boolean; urgent: boolean } {
  const unknown =
    task.state_entered_at === null ||
    task.claimant === null ||
    task.coverage.status === "not_collected" ||
    (task.due_at === null && task.blocker === null);
  return {
    decision: task.state === "needs_decision" || task.kind === "decide" || task.decision !== undefined,
    hold: task.state === "hold",
    unknown,
    urgent: task.priority >= 90,
  };
}

function emptySignals(): GroupSignals {
  return { count: 0, decision: 0, hold: 0, unknown: 0, urgent: 0 };
}

function addSignals(signals: GroupSignals, task: ProtoTask): void {
  const s = taskSignals(task);
  signals.count += 1;
  if (s.decision) signals.decision += 1;
  if (s.hold) signals.hold += 1;
  if (s.unknown) signals.unknown += 1;
  if (s.urgent) signals.urgent += 1;
}

function mergeSignals(into: GroupSignals, from: GroupSignals): void {
  into.count += from.count;
  into.decision += from.decision;
  into.hold += from.hold;
  into.unknown += from.unknown;
  into.urgent += from.urgent;
}

/**
 * Two-level draft grouping: area → bundle. `standalone` and `unclassified`
 * are always-present top-level groups. Never inferred from titles — reads
 * only the synthetic enrichment fixture.
 */
export function groupByArea(tasks: ProtoTask[], enrichment: Record<number, Enrichment>): AreaGroup[] {
  const areas = new Map<string, Map<string, ProtoTask[]>>();
  const standalone: ProtoTask[] = [];
  const unclassified: ProtoTask[] = [];
  for (const task of tasks) {
    const enr = enrichment[task.id];
    if (!enr) {
      unclassified.push(task);
      continue;
    }
    if (enr.standalone) {
      standalone.push(task);
      continue;
    }
    const area = enr.area ?? "unclassified";
    const bundle = enr.bundle ?? "(no bundle)";
    if (!areas.has(area)) {
      areas.set(area, new Map());
    }
    const bundles = areas.get(area)!;
    if (!bundles.has(bundle)) {
      bundles.set(bundle, []);
    }
    bundles.get(bundle)!.push(task);
  }

  const groups: AreaGroup[] = [];
  const areaNames = [...areas.keys()].sort();
  for (const name of areaNames) {
    const bundles: BundleGroup[] = [];
    const signals = emptySignals();
    for (const [bundleName, bundleTasks] of [...areas.get(name)!.entries()].sort()) {
      const bSignals = emptySignals();
      for (const task of bundleTasks) {
        addSignals(bSignals, task);
      }
      bundles.push({ key: `${name}/${bundleName}`, name: bundleName, tasks: bundleTasks, signals: bSignals });
      mergeSignals(signals, bSignals);
    }
    groups.push({ key: `area:${name}`, name, bundles, signals });
  }

  const special = (key: string, name: string, members: ProtoTask[]): AreaGroup => {
    const signals = emptySignals();
    for (const task of members) {
      addSignals(signals, task);
    }
    return { key, name, bundles: [{ key: `${key}/all`, name, tasks: members, signals }], signals };
  };
  groups.push(special("area:standalone", "standalone", standalone));
  groups.push(special("area:unclassified", "unclassified", unclassified));
  return groups;
}

/** Unique task IDs inside a collapsed group tree — used by tests to prove
 * collapsed counts equal the union of expanded member IDs. */
export function groupTaskIds(group: AreaGroup): Set<number> {
  const ids = new Set<number>();
  for (const bundle of group.bundles) {
    for (const task of bundle.tasks) {
      ids.add(task.id);
    }
  }
  return ids;
}

// ---- board columns ------------------------------------------------------

/** Bounded columns: only populated states relevant to the current view —
 * never all nine by default. */
export function boardColumns(visible: ProtoTask[], view: ProtoView, hiddenColumns: string[]): { state: string; tasks: ProtoTask[] }[] {
  const candidates = view === "all" ? [...KNOWN_STATES] : VIEW_STATES[view];
  const byState = new Map<string, ProtoTask[]>();
  for (const task of visible) {
    if (!byState.has(task.state)) {
      byState.set(task.state, []);
    }
    byState.get(task.state)!.push(task);
  }
  return candidates
    .filter((state) => (byState.get(state)?.length ?? 0) > 0 && !hiddenColumns.includes(state))
    .map((state) => ({ state, tasks: byState.get(state)! }));
}

// ---- status line --------------------------------------------------------

export function statusLine(dataset: Dataset): string {
  return `scope: ${dataset.label} · source: local synthetic fixture · generated: ${dataset.generatedAt} · completeness: ${dataset.completeness} — ${dataset.completenessNote}`;
}

export function ageDays(nowIso: string, iso: string | null): number | null {
  if (iso === null) {
    return null;
  }
  const ms = Date.parse(nowIso) - Date.parse(iso);
  return Math.max(0, Math.floor(ms / 86_400_000));
}
