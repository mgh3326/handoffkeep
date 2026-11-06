// A static chip: an icon (when the variant has one) and a text label. Meaning
// is never carried by color alone — every variant prints its words — and the
// chip is a plain <span>, never a control. The color group comes from
// statusGroup (shared/states.ts) so one function decides it everywhere.

import { Icon } from "../Icon";
import { StateIcon } from "../StateIcon";
import { statusGroup, stateLabel, type ChipGroup } from "../states";

/** A lane longer than this is cut; the full name stays in the accessible name. */
export const LANE_CHIP_MAX = 16;

export type ChipProps =
  | { kind: "status"; state: string; awaiting_decision?: boolean; stalled?: boolean; label?: string }
  | { kind: "priority"; value: number; urgent?: boolean }
  | { kind: "lane"; lane: string }
  /** A counter (the open-decision count in the navigation). `label` is the
   * spoken form; the digits alone are hidden from assistive technology. */
  | { kind: "count"; group: ChipGroup; count: number; label: string };

function StatusChip(p: Extract<ChipProps, { kind: "status" }>) {
  const group = statusGroup(p);
  const icon = group === "failed" ? <Icon name="warning" /> : <StateIcon state={group === "decision" ? "awaiting_decision" : p.state} />;
  return (
    <span className={`hk-chip hk-chip--${group}`} data-chip="status" data-group={group}>
      {icon}
      {p.label ?? stateLabel(p.state)}
    </span>
  );
}

export function Chip(props: ChipProps) {
  switch (props.kind) {
    case "status":
      return <StatusChip {...props} />;
    case "priority":
      return (
        <span className={`hk-chip hk-chip--priority${props.urgent ? " hk-chip--priority-urgent" : ""}`} data-chip="priority">
          {props.urgent ? <Icon name="warning" /> : null}P{props.value}
        </span>
      );
    case "lane": {
      const chars = Array.from(props.lane);
      const cut = chars.length > LANE_CHIP_MAX;
      return (
        <span className="hk-chip hk-chip--lane" data-chip="lane">
          {cut ? (
            <>
              <span aria-hidden="true">{chars.slice(0, LANE_CHIP_MAX).join("")}…</span>
              <span className="hk-sr-only">{props.lane}</span>
            </>
          ) : (
            props.lane
          )}
        </span>
      );
    }
    case "count":
      return (
        <span className={`hk-chip hk-chip--${props.group}`} data-chip="count">
          <span aria-hidden="true">{props.count}</span>
          <span className="hk-sr-only">{props.label}</span>
        </span>
      );
  }
}
