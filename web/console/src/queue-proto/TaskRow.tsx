import { ageDays, decisionMark, isStale } from "./adapter";
import { LIVE_TASK_STATES, liveAgeLabel, liveSectionLabel, taskLiveJobs, type LiveJob, type LiveResponse } from "../live";
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

/** One hub job as a row chip: role · machine · pane · elapsed · last-event
 * age. Every field renders or says 미상/미기록 — never blank, never 0. */
function JobChip({ job, now }: { job: LiveJob; now: string }) {
  const elapsed = liveAgeLabel(now, job.started_at);
  const eventAge = liveAgeLabel(now, job.last_event_at);
  const title = [`job ${job.job_id}`];
  if (job.owner_lane) {
    title.push(`owner ${job.owner_lane}`);
  }
  if (job.last_event_kind) {
    title.push(`last ${job.last_event_kind}`);
  }
  return (
    <span className="qp-livechip" title={title.join(" · ")}>
      <span className="qp-livechip-role">{job.role && job.role !== "" ? job.role : "role 미상"}</span>
      <span className="qp-livechip-machine">{job.machine === "" ? "머신 미상" : job.machine}</span>
      {job.pane ? <span className="qp-livechip-pane">{job.pane}</span> : null}
      <span className="qp-livechip-age">{elapsed === null ? "경과 미상" : `${elapsed}째`}</span>
      <span className="qp-livechip-evt">{eventAge === null ? "이벤트 미상" : `evt ${eventAge} 전`}</span>
    </span>
  );
}

/** Live chips for claimed · in_progress · verifying rows (#598): the task's
 * own refs.job_id job plus one-hop owner_lane children (tester/worker).
 * Failure modes stay explicit — a hub outage is "잡 조회 불가", a task with
 * no recorded job is "잡 ID 미기록", a recorded job missing from the hub is
 * "활성 잡 없음". Synthetic datasets (live === undefined) show nothing. */
export function LiveChips({ task, now, live }: { task: ProtoTask; now: string; live: LiveResponse | undefined }) {
  if (live === undefined || !LIVE_TASK_STATES.has(task.state)) {
    return null;
  }
  if (live.jobs.fetched_at === "") {
    return <span className="qp-livechip qp-livechip-warn">잡 조회 불가 · {liveSectionLabel(live.jobs.status)}</span>;
  }
  const { recorded, primary, children } = taskLiveJobs(live, task.id);
  if (!recorded) {
    return <span className="qp-livechip qp-livechip-warn">잡 ID 미기록</span>;
  }
  if (primary === null) {
    return <span className="qp-livechip qp-livechip-warn">활성 잡 없음</span>;
  }
  return (
    <span className="qp-livechips">
      <JobChip job={primary} now={now} />
      {children.map((job) => (
        <JobChip key={job.job_id} job={job} now={now} />
      ))}
    </span>
  );
}

/** One-line live marker for the fixed-height board card: the clipped card
 * box (76/96px) cannot spare a chip row without pushing the lane/claimant
 * line out, so cards carry a dot + job count instead of per-job chips.
 * Failure states keep their explicit words — never blank, never idle-looking.
 * Full role·machine·pane·elapsed chips stay on list rows. */
function LiveChipSummary({ task, now, live }: { task: ProtoTask; now: string; live: LiveResponse | undefined }) {
  if (live === undefined || !LIVE_TASK_STATES.has(task.state)) {
    return null;
  }
  if (live.jobs.fetched_at === "") {
    return (
      <span className="qp-livechip qp-livechip-warn" title={`잡 조회 불가 · ${liveSectionLabel(live.jobs.status)}`}>
        잡 조회 불가
      </span>
    );
  }
  const { recorded, primary, children } = taskLiveJobs(live, task.id);
  if (!recorded) {
    return <span className="qp-livechip qp-livechip-warn">잡 ID 미기록</span>;
  }
  if (primary === null) {
    return <span className="qp-livechip qp-livechip-warn">활성 잡 없음</span>;
  }
  const elapsed = liveAgeLabel(now, primary.started_at);
  const title = [
    `job ${primary.job_id}`,
    primary.role === "" ? "role 미상" : primary.role,
    primary.machine === "" ? "머신 미상" : primary.machine,
    elapsed === null ? "경과 미상" : `${elapsed}째`,
    ...children.map((job) => `+ job ${job.job_id}`),
  ];
  return (
    <span className="qp-livechip qp-livesum" title={title.join(" · ")}>
      <span className="qp-livechip-role">●</span>
      {` live ${1 + children.length}`}
    </span>
  );
}

/** #618 row badge: only the fact that a recorded decision request is open
 * (the drawer carries the question and choices; answering is #580). Text,
 * not color alone, says which. */
function DecisionBadge({ task }: { task: ProtoTask }) {
  const mark = decisionMark(task);
  return mark === null ? null : (
    <span className="qp-livechip qp-livechip-warn" data-decision={mark} title={task.refs.decision_request?.id}>
      {mark === "pending" ? "내 결정 대기" : "미정리 요청"}
    </span>
  );
}

/** The hub job standing in for an unrecorded claimant: the task's primary
 * job owner_lane, exact-matched — never a guessed string. */
function liveClaimant(task: ProtoTask, live: LiveResponse | undefined): string | null {
  if (live === undefined || live.jobs.fetched_at === "" || task.claimant !== null) {
    return null;
  }
  return taskLiveJobs(live, task.id).primary?.owner_lane ?? null;
}

type RowFieldsProps = {
  task: ProtoTask;
  now: string;
  /** Flat (ungrouped) lists have no group header naming the state, so the
   * row shows the state label next to the shape. */
  showStateLabel?: boolean;
  /** /ui/api/live aggregation; absent for synthetic datasets. */
  live?: LiveResponse;
};

/**
 * One list row: state shape · #id · p-value · title · lane/claimant and ages.
 * The title is the full original string — it is clipped only by CSS
 * line-clamp (1 line at 40px, 2 lines at 56px), never cut in JS, so the DOM,
 * copy/paste and assistive tech keep every character.
 */
export function RowFields({ task, now, showStateLabel = false, live }: RowFieldsProps) {
  const created = ageLabel(now, task.created_at);
  const entered = ageLabel(now, task.state_entered_at);
  const stale = isStale(task, now);
  const claimantFill = liveClaimant(task, live);
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
            claimantFill === null ? (
              <Missing>인수자 미상</Missing>
            ) : (
              <span className="qp-claimant qp-claimant-live" title="허브 잡의 owner_lane — 태스크에 기록된 인수자 없음">
                {claimantFill}
              </span>
            )
          ) : (
            <span className="qp-claimant">{task.claimant}</span>
          )}
        </span>
        <DecisionBadge task={task} />
        <LiveChips task={task} now={now} live={live} />
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
 * live summary (inline on the meta line — a chip row of its own would clip
 * the claimant) · title (CSS-clamped, full text in the DOM) · lane/claimant. */
export function CardFields({ task, now, live }: { task: ProtoTask; now: string; live?: LiveResponse }) {
  const stale = isStale(task, now);
  const claimantFill = liveClaimant(task, live);
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
      <DecisionBadge task={task} />
      <LiveChipSummary task={task} now={now} live={live} />
      <span className="qp-cell qp-title">{task.title}</span>
      <span className="qp-cell qp-lane">
        {task.lane}/
        {task.claimant === null ? (
          claimantFill === null ? (
            <span className="qp-unknown">unknown</span>
          ) : (
            claimantFill
          )
        ) : (
          task.claimant
        )}
      </span>
    </>
  );
}
