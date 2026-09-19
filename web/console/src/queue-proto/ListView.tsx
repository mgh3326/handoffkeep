import { useMemo } from "react";
import { groupByArea, type AreaGroup, type GroupSignals } from "./adapter";
import { VirtualList } from "./VirtualList";
import { RowFields } from "./TaskRow";
import type { Dataset, ProtoState, ProtoTask } from "./types";

type FlatRow =
  | { kind: "group"; key: string; name: string; depth: 1 | 2; signals: GroupSignals }
  | { kind: "task"; task: ProtoTask };

export function flattenGrouped(groups: AreaGroup[], collapsed: string[]): FlatRow[] {
  const rows: FlatRow[] = [];
  const isCollapsed = (key: string) => collapsed.includes(key);
  for (const group of groups) {
    rows.push({ kind: "group", key: group.key, name: group.name, depth: 1, signals: group.signals });
    if (isCollapsed(group.key)) {
      continue;
    }
    for (const bundle of group.bundles) {
      rows.push({ kind: "group", key: bundle.key, name: bundle.name, depth: 2, signals: bundle.signals });
      if (isCollapsed(bundle.key)) {
        continue;
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

export function ListView({ dataset, visible, grouping, collapsedGroups, density, selectedId, onOpen, onToggleGroup }: ListViewProps) {
  const rows = useMemo<FlatRow[]>(() => {
    if (grouping !== "area") {
      return flattenUngrouped(visible);
    }
    return flattenGrouped(groupByArea(visible, dataset.enrichment), collapsedGroups);
  }, [visible, grouping, collapsedGroups, dataset.enrichment]);

  const rowHeight = density === "compact" ? 30 : 44;

  return (
    <VirtualList
      className="qp-list"
      items={rows}
      rowHeight={rowHeight}
      getKey={(row) => (row.kind === "task" ? `t${row.task.id}` : row.key)}
      renderRow={(row) => {
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
        return (
          <button
            type="button"
            className={`qp-row${task.id === selectedId ? " selected" : ""}`}
            data-task-id={task.id}
            onClick={(event) => onOpen(task.id, event.currentTarget)}
            onKeyDown={(event) => {
              if (event.key === "Enter") {
                onOpen(task.id, event.currentTarget);
              }
            }}
          >
            <RowFields task={task} now={dataset.generatedAt} />
          </button>
        );
      }}
    />
  );
}
