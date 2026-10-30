import {
  jobForPane,
  liveSectionLabel,
  loadLabel,
  memoryLabel,
  pingAgeLabel,
  taskIdForJob,
  type LiveJob,
  type LiveNode,
  type LiveResponse,
  type LiveSession,
} from "./live";
import { ReapPanel } from "./ReapPanel";
import { getJSON } from "./shared/http";
import { usePoll } from "./shared/usePoll";

const LIVE_API = "/ui/api/live";
const POLL_MS = 10_000;

/** Runtime shape check for /ui/api/live. A 200 with a malformed body is a
 * failure, never published as a snapshot — the loader throws before usePoll
 * can replace the last good data. Lives here, not in live.ts: that module is
 * shared with the queue bundle, while this check is fleet-only. Checks the
 * fields the console reads: section envelopes, links, the mismatch lists and
 * each node's identity. */
function isLiveResponse(value: unknown): value is LiveResponse {
  if (typeof value !== "object" || value === null) {
    return false;
  }
  const x = value as Record<string, unknown>;
  const section = (v: unknown): boolean => {
    if (typeof v !== "object" || v === null) {
      return false;
    }
    const s = v as Record<string, unknown>;
    return typeof s.status === "string" && typeof s.fetched_at === "string" && Array.isArray(s.items);
  };
  const node = (v: unknown): boolean => {
    if (typeof v !== "object" || v === null) {
      return false;
    }
    const n = v as Record<string, unknown>;
    return typeof n.machine_id === "string" && typeof n.display_state === "string";
  };
  const mismatch = x.mismatch;
  return (
    typeof x.generated_at === "string" &&
    section(x.jobs) &&
    section(x.nodes) &&
    (x.nodes as { items: unknown[] }).items.every(node) &&
    Array.isArray(x.links) &&
    typeof mismatch === "object" &&
    mismatch !== null &&
    Array.isArray((mismatch as Record<string, unknown>).tasks_without_job) &&
    Array.isArray((mismatch as Record<string, unknown>).jobs_without_task)
  );
}

const loadLive = async () => {
  const body = await getJSON<LiveResponse>(LIVE_API);
  if (!isLiveResponse(body)) {
    throw new Error("malformed live response");
  }
  return body;
};

/** Session-list message for a node whose snapshot is not a plain session
 * list. Every non-ok state names itself — a stale or failed collection is
 * never rendered as "한가함". */
function snapshotNote(node: LiveNode): string {
  const snap = node.session_snapshot;
  switch (node.display_state) {
    case "empty":
      return "세션 없음";
    case "unavailable":
      return "수집 실패";
    case "unknown":
      return `알 수 없는 수집 상태: ${snap?.snapshot_status ?? "미상"}`;
    case "missing":
    default:
      return "스냅샷 없음";
  }
}

/** session → job (exact pane_id on the same machine) → task (exact job_id /
 * one-hop owner_lane). Returns the label for the link cell; never fabricates
 * a connection. */
function sessionLink(live: LiveResponse, machineId: string, session: LiveSession): { href: string | null; label: string } {
  const job = jobForPane(live, machineId, session.pane_id);
  if (job === null) {
    return { href: null, label: "연결 없음" };
  }
  const taskId = taskIdForJob(live, job.job_id);
  if (taskId === null) {
    return { href: null, label: `잡 ${job.job_id}` };
  }
  return { href: `/ui/queue?task=${taskId}`, label: `#${taskId}` };
}

function MachineJobs({ jobs, live }: { jobs: LiveJob[]; live: LiveResponse }) {
  if (jobs.length === 0) {
    // "활성 잡 없음" is only true when the jobs section actually delivered.
    // A failed/absent fetch must name itself, never read as an idle machine.
    if (live.jobs.fetched_at === "") {
      return <span className="muted">잡 조회 불가 · {liveSectionLabel(live.jobs.status)}</span>;
    }
    return <span className="muted">활성 잡 없음</span>;
  }
  return (
    <ul className="fleet-jobs">
      {jobs.map((job) => {
        const taskId = taskIdForJob(live, job.job_id);
        return (
          <li key={job.job_id}>
            <span className="fleet-job">
              {job.role ?? "role 미상"} · {job.job_id}
              {job.pane ? ` · ${job.pane}` : ""}
            </span>
            {taskId === null ? (
              <span className="muted">{live.tasks_truncated ? " 태스크 대조 불가(목록 잘림)" : " 태스크 미연결"}</span>
            ) : (
              <a href={`/ui/queue?task=${taskId}`}> #{taskId}</a>
            )}
          </li>
        );
      })}
    </ul>
  );
}

function SessionTable({ node, live }: { node: LiveNode; live: LiveResponse }) {
  const snap = node.session_snapshot;
  if (node.display_state !== "sessions" || snap === null) {
    return <p className="muted">{snapshotNote(node)}</p>;
  }
  return (
    <>
      {snap.stale ? <span className="badge stale">stale · 마지막 수신 {snap.received_at}</span> : null}
      {snap.truncated ? <span className="badge">잘림</span> : null}
      <table className="fleet-table">
        <thead>
          <tr>
            <th>세션</th>
            <th>status</th>
            <th>pane</th>
            <th>workspace</th>
            <th>연결</th>
          </tr>
        </thead>
        <tbody>
          {snap.sessions.map((session, index) => {
            const link = sessionLink(live, node.machine_id, session);
            return (
              <tr key={`${session.pane_id}:${index}`}>
                <td>{session.label}</td>
                <td>{session.status}</td>
                <td>{session.pane_id}</td>
                <td>{session.workspace_id}</td>
                <td>{link.href === null ? <span className="muted">{link.label}</span> : <a href={link.href}>{link.label}</a>}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </>
  );
}

function MachineBlock({ node, live }: { node: LiveNode; live: LiveResponse }) {
  const jobs = live.jobs.items.filter((job) => job.machine === node.machine_id);
  // Hub node state is connected|stale|disconnected — every non-connected
  // state gets a badge so a dead node's last heartbeat never reads as live.
  const notConnected = node.state !== "" && node.state !== "connected";
  const stale = notConnected || (node.session_snapshot?.stale ?? false);
  return (
    <section className="fleet-node">
      <h3 className="fleet-node-head">
        {node.machine_id} <span className="muted">{node.state}</span>
        {stale ? <span className="badge stale">{notConnected ? node.state : "stale"}</span> : null}
        <span className="badge">{node.accepting_effective ? "accepting" : "중지"}</span>
      </h3>
      <p className="fleet-node-meta">
        부하 {loadLabel(node.load)} · 메모리 {memoryLabel(node.memory)} · 활성 잡{" "}
        {node.active_jobs === null ? "미상" : node.active_jobs} · 마지막 핑 {pingAgeLabel(node.last_ping_ms)}
      </p>
      <MachineJobs jobs={jobs} live={live} />
      <SessionTable node={node} live={live} />
    </section>
  );
}

/** Machine/session view (#598): per-machine load, memory (with measurement
 * source), active jobs, accepting state and the session list with task/job
 * backlinks. Hub section failures are named, never rendered as an empty or
 * idle fleet. Read-only — no write controls. */
export function FleetApp() {
  const { data, failed: refreshFailed } = usePoll(loadLive, POLL_MS);

  if (data === null && !refreshFailed) {
    return <p className="muted">불러오는 중</p>;
  }
  if (data === null) {
    return <p className="muted">머신 정보를 아직 받지 못했습니다.</p>;
  }

  const nodes = data.nodes.items;
  return (
    <section>
      <h2>Machines · sessions</h2>
      <div className="fleet-status">
        {refreshFailed ? (
          <span className="badge stale">갱신 실패 · 마지막 성공 {data.generated_at || "없음"}</span>
        ) : null}
        <span>
          잡 <span className="badge">{liveSectionLabel(data.jobs.status)}</span>{" "}
          <time>{data.jobs.fetched_at || "받은 자료 없음"}</time>
        </span>
        <span>
          머신 <span className="badge">{liveSectionLabel(data.nodes.status)}</span>{" "}
          <time>{data.nodes.fetched_at || "받은 자료 없음"}</time>
        </span>
        {data.mismatch.basis !== "unavailable" && data.mismatch.jobs_without_task.length > 0 ? (
          <span className="badge">태스크 미연결 잡 {data.mismatch.jobs_without_task.length}건</span>
        ) : null}
      </div>
      {nodes.length === 0 ? (
        <p className="muted">
          {data.nodes.status === "ok" ? "등록된 머신이 없습니다." : `머신 목록을 받지 못했습니다 — ${liveSectionLabel(data.nodes.status)}`}
        </p>
      ) : (
        nodes.map((node) => <MachineBlock key={node.machine_id} node={node} live={data} />)
      )}
      <ReapPanel />
    </section>
  );
}
