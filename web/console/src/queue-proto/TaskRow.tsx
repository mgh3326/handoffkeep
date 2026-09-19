import { ageDays, clampPreview } from "./adapter";
import { PREVIEW_CLAMP } from "./fixtures";
import type { ProtoTask } from "./types";

export function AgeCell({ days }: { days: number | null }) {
  if (days === null) {
    return <span className="qp-unknown">unknown</span>;
  }
  return <span>{days}d</span>;
}

export function taskPreview(task: ProtoTask): string {
  return clampPreview(task.title, PREVIEW_CLAMP);
}

export function RowFields({ task, now }: { task: ProtoTask; now: string }) {
  return (
    <>
      <span className="qp-cell qp-id">#{task.id}</span>
      <span className="qp-cell qp-title">{taskPreview(task)}</span>
      <span className="qp-cell qp-state">{task.state}</span>
      <span className="qp-cell qp-lane">
        {task.lane}/{task.claimant ?? "unknown"}
      </span>
      <span className="qp-cell qp-pri">p{task.priority}</span>
      <span className="qp-cell qp-age">
        <AgeCell days={ageDays(now, task.created_at)} />
      </span>
      <span className="qp-cell qp-age">
        <AgeCell days={ageDays(now, task.state_entered_at)} />
      </span>
    </>
  );
}
