import { ageDays, isStale } from "./adapter";
import { StateIcon } from "./StateIcon";
import { stateLabel } from "./states";
import type { ProtoTask } from "./types";

/** Whole days since a timestamp, as the board card shows it. */
export function AgeCell({ days }: { days: number | null }) {
  if (days === null) {
    return <span className="qp-unknown">unknown</span>;
  }
  return <span>{days}d</span>;
}

/** Relative age in Korean ("2일", "5시간") measured against the dataset
 * snapshot time, or null when either timestamp is absent or unparseable —
 * the caller renders that as a missing value, never as 0. */
export function ageLabel(nowIso: string, iso: string | null): string | null {
  if (iso === null) {
    return null;
  }
  const ms = Date.parse(nowIso) - Date.parse(iso);
  if (!Number.isFinite(ms)) {
    return null;
  }
  const days = ageDays(nowIso, iso)!;
  if (days >= 1) {
    return `${days}일`;
  }
  return `${Math.max(1, Math.floor(ms / 3_600_000))}시간`;
}

/** A value the list API does not carry, or carries as null: shown as an
 * explicit word so it can never read as 0 or as "nothing". */
function Missing({ children }: { children: string }) {
  return <span className="qp-unknown">{children}</span>;
}

type RowFieldsProps = {
  task: ProtoTask;
  now: string;
  /** Flat (ungrouped) lists have no group header naming the state, so the
   * row shows the state label next to the shape. */
  showStateLabel?: boolean;
};

/**
 * One list row: state shape · #id · p-value · title · lane/claimant and ages.
 * The title is the full original string — it is clipped only by CSS
 * line-clamp (1 line at 40px, 2 lines at 56px), never cut in JS, so the DOM,
 * copy/paste and assistive tech keep every character.
 */
export function RowFields({ task, now, showStateLabel = false }: RowFieldsProps) {
  const created = ageLabel(now, task.created_at);
  const entered = ageLabel(now, task.state_entered_at);
  const stale = isStale(task, now);
  return (
    <>
      <span className="qp-cell qp-state-icon" title={stateLabel(task.state)}>
        <StateIcon state={task.state} srLabel={!showStateLabel} />
      </span>
      <span className="qp-cell qp-id">#{task.id}</span>
      <span className={`qp-cell qp-pri${task.priority >= 90 ? " urgent" : ""}`}>p{task.priority}</span>
      {showStateLabel ? <span className="qp-cell qp-state">{stateLabel(task.state)}</span> : null}
      <span className="qp-cell qp-title">{task.title}</span>
      <span className="qp-cell qp-meta">
        <span className="qp-meta-who">
          <span className="qp-lane">{task.lane}</span>
          <span className="qp-meta-sep" aria-hidden="true">
            /
          </span>
          {task.claimant === null ? (
            <Missing>인수자 미상</Missing>
          ) : (
            <span className="qp-claimant">{task.claimant}</span>
          )}
        </span>
        <span className="qp-meta-age">
          <span className="qp-age" title="생성(created_at) 기준">
            {created === null ? <Missing>생성 미상</Missing> : `생성 ${created} 전`}
          </span>
          {stale ? (
            <span className="qp-stale" title="생성 후 7일 이상 — 전제가 여전히 유효하다는 뜻이 아닙니다">
              7일+
            </span>
          ) : null}
          <span className="qp-age" title="현재 상태 진입(state_entered_at) 기준">
            {entered === null ? <Missing>진입 미수집</Missing> : `진입 ${entered} 전`}
          </span>
        </span>
      </span>
    </>
  );
}

/** Board card fields — deliberately fewer lines than the dense list row. The
 * card lives inside a fixed-height virtual row (76/96px) with
 * overflow:hidden, so every extra line is clipped content. The column head
 * already names the state and the second age column adds nothing a card
 * needs, so both stay out; what remains is identity: pri · id · age · stale ·
 * title (CSS-clamped, full text in the DOM) · lane/claimant. */
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
      <span className="qp-cell qp-title">{task.title}</span>
      <span className="qp-cell qp-lane">
        {task.lane}/{task.claimant === null ? <span className="qp-unknown">unknown</span> : task.claimant}
      </span>
    </>
  );
}
