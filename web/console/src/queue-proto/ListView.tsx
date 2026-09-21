import { useCallback, useMemo } from "react";
import { groupByArea, groupByState, type AreaGroup, type GroupSignals, type StateGroup } from "./adapter";
import { StateIcon } from "./StateIcon";
import { stateLabel } from "./states";
import { VirtualList } from "./VirtualList";
import { RowFields } from "./TaskRow";
import type { Dataset, Density, ProtoState, ProtoTask } from "./types";

type FlatRow =
  | { kind: "group"; key: string; name: string; depth: 1 | 2; signals: GroupSignals }
  | { kind: "state"; key: string; state: string; count: number }
  | { kind: "task"; task: ProtoTask };

/** Row heights in rem, mirroring --hk-row-compact / --hk-row-context and the
 * group header. The virtual list positions rows in px, so the rem values are
 * scaled by the live root font size — a user who enlarges the base font gets
 * taller rows instead of clipped ones. */
export const ROW_REM: Record<Density, number> = { compact: 2.5, comfortable: 3.5 };
export const GROUP_REM = 2.25;

function rootFontPx(): number {
  if (typeof window === "undefined") {
    return 16;
  }
  const px = Number.parseFloat(window.getComputedStyle(document.documentElement).fontSize);
  return Number.isFinite(px) && px > 0 ? px : 16;
}

export function flattenByState(groups: StateGroup[], collapsed: string[]): FlatRow[] {
  const rows: FlatRow[] = [];
  for (const group of groups) {
    rows.push({ kind: "state", key: group.key, state: group.state, count: group.tasks.length });
    if (collapsed.includes(group.key)) {
      continue;
    }
    for (const task of group.tasks) {
      rows.push({ kind: "task", task });
    }
  }
  return rows;
}

export function flattenGrouped(groups: AreaGroup[], collapsed: string[]): FlatRow[] {
  const rows: FlatRow[] = [];
  const isCollapsed = (key: string) => collapsed.includes(key);
  for (const group of groups) {
    // Empty groups (e.g. standalone/unclassified with no members) are not
    // rendered as headers — their zero counts would only be noise.
    if (group.signals.count === 0) {
      continue;
    }
    rows.push({ kind: "group", key: group.key, name: group.name, depth: 1, signals: group.signals });
    if (isCollapsed(group.key)) {
      continue;
    }
    for (const bundle of group.bundles) {
      // A single-bundle area lists its tasks directly under the area header —
      // a depth-2 header that restates the only bundle adds no information.
      if (group.bundles.length > 1) {
        rows.push({ kind: "group", key: bundle.key, name: bundle.name, depth: 2, signals: bundle.signals });
        if (isCollapsed(bundle.key)) {
          continue;
        }
      }
      for (const task of bundle.tasks) {
        rows.push({ kind: "task", task });
      }
    }
  }
  return rows;
}

export function flattenUngrouped(tasks: ProtoTask[]): FlatRow[] {
  return tasks.map((task) => ({ kind: "task", task }));
}

export function SignalBadges({ signals }: { signals: GroupSignals }) {
  return (
    <span className="qp-signals">
      <span className="qp-badge">{signals.count}</span>
      {signals.decision > 0 ? <span className="qp-badge qp-sig-decision">decision {signals.decision}</span> : null}
      {signals.hold > 0 ? <span className="qp-badge qp-sig-hold">hold {signals.hold}</span> : null}
      {signals.unknown > 0 ? <span className="qp-badge qp-sig-unknown">unknown {signals.unknown}</span> : null}
      {signals.urgent > 0 ? <span className="qp-badge qp-sig-urgent">urgent {signals.urgent}</span> : null}
      {signals.stale > 0 ? <span className="qp-badge qp-sig-stale">stale {signals.stale}</span> : null}
    </span>
  );
}

type ListViewProps = {
  dataset: Dataset;
  visible: ProtoTask[];
  grouping: ProtoState["grouping"];
  collapsedGroups: string[];
  density: ProtoState["density"];
  selectedId: number | null;
  onOpen: (id: number, el: HTMLElement) => void;
  onToggleGroup: (key: string) => void;
};

/** href a row would load on its own: the queue with that task open. The
 * click handler intercepts plain clicks for the in-page panel; modified and
 * middle clicks keep the native new-tab behaviour. */
function taskHref(id: number): string {
  const params = typeof window !== "undefined" ? new URLSearchParams(window.location.search) : new URLSearchParams();
  params.set("task", String(id));
  return `/ui/queue?${params.toString()}`;
}

export function ListView({ dataset, visible, grouping, collapsedGroups, density, selectedId, onOpen, onToggleGroup }: ListViewProps) {
  const rows = useMemo<FlatRow[]>(() => {
    if (grouping === "state") {
      return flattenByState(groupByState(visible), collapsedGroups);
    }
    if (grouping === "area") {
      return flattenGrouped(groupByArea(visible, dataset.enrichment, dataset.generatedAt), collapsedGroups);
    }
    return flattenUngrouped(visible);
  }, [visible, grouping, collapsedGroups, dataset.enrichment, dataset.generatedAt]);

  const px = rootFontPx();
  const rowPx = ROW_REM[density] * px;
  const groupPx = GROUP_REM * px;
  const heightOf = useCallback((row: FlatRow) => (row.kind === "task" ? rowPx : groupPx), [rowPx, groupPx]);
  // A partial dataset makes every group count a lower bound.
  const partial = dataset.completeness !== "complete";

  return (
    <div className="qp-listwrap" data-density={density} data-grouping={grouping}>
      <VirtualList
        className="qp-list"
        items={rows}
        rowHeight={heightOf}
        getKey={(row) => (row.kind === "task" ? `t${row.task.id}` : row.key)}
        renderRow={(row) => {
          if (row.kind === "state") {
            const collapsed = collapsedGroups.includes(row.key);
            return (
              <h2 className="qp-group-h">
                <button
                  type="button"
                  className={`qp-group qp-group-state${collapsed ? " collapsed" : ""}`}
                  data-group={row.key}
                  aria-expanded={!collapsed}
                  onClick={() => onToggleGroup(row.key)}
                >
                  <svg className="hk-icon qp-chev" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" aria-hidden="true">
                    <path d="M6 9l6 6 6-6" />
                  </svg>
                  <StateIcon state={row.state} />
                  <span className="qp-group-name">{stateLabel(row.state)}</span>
                  <span className="qp-badge qp-group-count">{row.count}</span>
                  {partial ? <span className="qp-group-partial">확인된 건수 · 일부만 조회됨</span> : null}
                </button>
              </h2>
            );
          }
          if (row.kind === "group") {
            const collapsed = collapsedGroups.includes(row.key);
            return (
              <button
                type="button"
                className={`qp-group qp-group-d${row.depth}${collapsed ? " collapsed" : ""}`}
                data-group={row.key}
                aria-expanded={!collapsed}
                onClick={() => onToggleGroup(row.key)}
              >
                <span className="qp-group-name">
                  {collapsed ? "▸" : "▾"} {row.name}
                </span>
                <SignalBadges signals={row.signals} />
              </button>
            );
          }
          const task = row.task;
          const selected = task.id === selectedId;
          return (
            <a
              href={taskHref(task.id)}
              className={`qp-row${selected ? " selected" : ""}`}
              data-task-id={task.id}
              aria-current={selected ? "true" : undefined}
              onClick={(event) => {
                if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey || event.button !== 0) {
                  return;
                }
                event.preventDefault();
                onOpen(task.id, event.currentTarget);
              }}
              onKeyDown={(event) => {
                if (event.key === "Enter" && !event.metaKey && !event.ctrlKey && !event.shiftKey && !event.altKey) {
                  // The link's own Enter activation would fire a click too;
                  // open once, from here. Modified Enter keeps the native
                  // open-in-new-tab/window.
                  event.preventDefault();
                  onOpen(task.id, event.currentTarget);
                }
              }}
            >
              <RowFields task={task} now={dataset.generatedAt} showStateLabel={grouping === "none"} />
            </a>
          );
        }}
      />
    </div>
  );
}
