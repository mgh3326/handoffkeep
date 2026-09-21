import { ageDays, clampPreview, isStale } from "./adapter";
import { PREVIEW_CLAMP, type ProtoTask } from "./types";

export function AgeCell({ days }: { days: number | null }) {
  if (days === null) {
    return <span className="qp-unknown">unknown</span>;
  }
  return <span>{days}d</span>;
}

export function taskPreview(task: ProtoTask): string {
  return clampPreview(task.title, PREVIEW_CLAMP);
}

/** Visible column labels for the dense list — each duration column names the
 * exact timestamp it is computed from. */
export const LIST_COLUMNS = [
  { key: "pri", label: "priority", title: "priority (p0–p99)" },
  { key: "id", label: "id", title: "task id" },
  { key: "title", label: "title", title: "task title (searchable in full)" },
  { key: "state", label: "state", title: "canonical task state" },
  { key: "flag", label: "flags", title: "stale: non-terminal and age ≥ 7 days" },
  { key: "lane", label: "lane / claimant", title: "lane and current claimant (unknown when unset)" },
  { key: "age", label: "created age", title: "whole days since created_at" },
  { key: "age2", label: "state age", title: "whole days since state_entered_at" },
] as const;

export function RowFields({ task, now }: { task: ProtoTask; now: string }) {
  const stale = isStale(task, now);
  return (
    <>
      <span className="qp-cell qp-pri">p{task.priority}</span>
      <span className="qp-cell qp-id">#{task.id}</span>
      <span className="qp-cell qp-title">{taskPreview(task)}</span>
      <span className="qp-cell qp-state">{task.state}</span>
      <span className="qp-cell qp-flag">
        {stale ? (
          <span className="qp-stale" title="age ≥ 7 days does not imply the premise is still valid">
            ⚠ stale
          </span>
        ) : null}
      </span>
      <span className="qp-cell qp-lane">
        {task.lane}/{task.claimant === null ? <span className="qp-unknown">unknown</span> : task.claimant}
      </span>
      <span className="qp-cell qp-age" title="days since created_at">
        <AgeCell days={ageDays(now, task.created_at)} />
      </span>
      <span className="qp-cell qp-age" title="days since state_entered_at">
        <AgeCell days={ageDays(now, task.state_entered_at)} />
      </span>
    </>
  );
}

/** Board card fields — deliberately fewer lines than the dense list row. The
 * card lives inside a fixed-height virtual row (76/96px) with
 * overflow:hidden, so every extra line is clipped content. The column head
 * already names the state and the second age column adds nothing a card
 * needs, so both stay out; what remains is identity: pri · id · age · stale ·
 * title (clamped) · lane/claimant. */
export function CardFields({ task, now }: { task: ProtoTask; now: string }) {
  const stale = isStale(task, now);
  return (
    <>
      <span className="qp-cell qp-pri">p{task.priority}</span>
      <span className="qp-cell qp-id">#{task.id}</span>
      <span className="qp-cell qp-age" title="days since created_at">
        <AgeCell days={ageDays(now, task.created_at)} />
      </span>
      {stale ? (
        <span className="qp-cell qp-flag">
          <span className="qp-stale" title="age ≥ 7 days does not imply the premise is still valid">
            ⚠ stale
          </span>
        </span>
      ) : null}
      <span className="qp-cell qp-title">{taskPreview(task)}</span>
      <span className="qp-cell qp-lane">
        {task.lane}/{task.claimant === null ? <span className="qp-unknown">unknown</span> : task.claimant}
      </span>
    </>
  );
}
