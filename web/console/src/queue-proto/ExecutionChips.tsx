import type { BoardTask, BoardWorkSession } from "../shared/boardTypes";
import { taskLiveJobs, type LiveJob, type LiveResponse } from "../live";

const roles: Record<string, string> = {
  lead: "Builder",
  worker: "Worker",
  review: "Reviewer",
  consult: "Consultant",
};
function Role({ role }: { role: string | undefined }) {
  return (
    <span
      className="hk-chip"
      aria-label={
        role ? `Role ${role}: ${roles[role] ?? role}` : "Role not collected"
      }
    >
      {role ? (roles[role] ?? role) : "Role not collected"}
    </span>
  );
}
function primarySession(task: BoardTask) {
  const board = task.work_board;
  if (!board) return undefined;
  const sorted = [...board.sessions].sort(
    (a, b) => b.at.localeCompare(a.at) || a.job_id.localeCompare(b.job_id),
  );
  return (
    board.sessions.find((s) => s.job_id === board.primary_job_id) ??
    sorted.find((s) => s.role === "lead") ??
    sorted.find((s) => s.role === "worker") ??
    sorted.find((s) => s.active) ??
    sorted[0]
  );
}
function Session({
  recorded,
  observed,
  observation,
}: {
  recorded?: BoardWorkSession;
  observed?: LiveJob;
  observation: string;
}) {
  const host = recorded?.host || observed?.machine;
  return (
    <>
      <span className="hk-chip">{host || "Host not collected"}</span>
      <Role role={recorded?.role || observed?.role} />
      <span className="hk-chip">
        {recorded?.model || "Model not collected"}
      </span>
      {recorded?.family ? (
        <span className="hk-chip">{recorded.family}</span>
      ) : null}
      {recorded ? (
        <span className="hk-chip">
          Recorded placement · <time>{recorded.at}</time>
        </span>
      ) : null}
      {observed ? (
        <span className="hk-chip">
          {observation}
          {observed.machine && observed.machine !== host
            ? ` on ${observed.machine}`
            : ""}
        </span>
      ) : null}
    </>
  );
}
export function ExecutionChips({
  task,
  live,
}: {
  task: BoardTask;
  live: LiveResponse;
}) {
  const linked = taskLiveJobs(live, task.id);
  const observation =
    live.jobs.status === "ok"
      ? "Observed job"
      : `Last observed job · ${live.jobs.status}`;
  const recorded = primarySession(task);
  const observed = recorded
    ? live.jobs.items.find((j) => j.job_id === recorded.job_id)
    : (linked.primary ?? undefined);
  const primaryID = recorded?.job_id ?? observed?.job_id;
  const sessions = new Map<
    string,
    { recorded?: BoardWorkSession; observed?: LiveJob }
  >();
  for (const session of task.work_board?.sessions ?? []) {
    if (session.active && session.job_id !== primaryID)
      sessions.set(session.job_id, {
        recorded: session,
        observed: live.jobs.items.find((j) => j.job_id === session.job_id),
      });
  }
  for (const job of [linked.primary, ...linked.children]) {
    if (job && job.job_id !== primaryID && !sessions.has(job.job_id))
      sessions.set(job.job_id, { observed: job });
  }
  const pr = task.work_board?.pr || task.refs.pr;
  return (
    <>
      <Session
        recorded={recorded}
        observed={observed}
        observation={observation}
      />
      {sessions.size ? (
        <details className="hk-chip">
          <summary>+{sessions.size} sessions</summary>
          <ul>
            {[...sessions].map(([id, session]) => (
              <li key={id}>
                <Session {...session} observation={observation} />
              </li>
            ))}
          </ul>
        </details>
      ) : null}
      {pr ? (
        /^https:\/\/github\.com\/[^/]+\/[^/]+\/pull\/\d+$/.test(pr) ? (
          <a className="hk-chip" href={pr}>
            PR {pr.split("/").pop()}
          </a>
        ) : (
          <span className="hk-chip">PR {pr}</span>
        )
      ) : null}
    </>
  );
}
