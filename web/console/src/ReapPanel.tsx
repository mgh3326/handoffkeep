import { useEffect, useState } from "react";
import { heldByReason, isReapResponse, reapReasonLabel, reapStatusLabel, type ReapResponse } from "./reap";

const REAP_API = "/ui/api/reap";
const POLL_MS = 10_000;

/** 정리 후보 (#603 stage 1): panes a future cleanup step could close, as
 * judged by each node and joined with task state. Report only — there is no
 * close control, and a failed or unsupported lookup names itself instead of
 * reading as "no candidates". */
export function ReapPanel() {
  const [data, setData] = useState<ReapResponse | null>(null);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let cancelled = false;
    const load = async () => {
      try {
        const response = await fetch(REAP_API);
        const body: unknown = response.ok ? await response.json() : null;
        if (!isReapResponse(body)) {
          throw new Error("reap request failed");
        }
        if (!cancelled) {
          setData(body);
          setFailed(false);
        }
      } catch {
        if (!cancelled) {
          setFailed(true);
        }
      }
    };
    void load();
    const timer = window.setInterval(() => {
      void load();
    }, POLL_MS);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, []);

  return (
    <section className="fleet-reap">
      <h2>정리 후보</h2>
      <p className="muted">보고 전용 — 이 화면은 세션을 닫지 않습니다.</p>
      {failed ? <span className="badge stale">정리 후보 조회 불가{data ? ` · 마지막 성공 ${data.generated_at}` : ""}</span> : null}
      {data === null ? null : <ReapBody data={data} />}
    </section>
  );
}

function ReapBody({ data }: { data: ReapResponse }) {
  if (data.status !== "ok") {
    return <p className="muted">정리 후보를 받지 못했습니다 — {reapStatusLabel(data.status)}</p>;
  }
  if (data.nodes.length === 0) {
    return <p className="muted">보고한 노드 없음 (주기 보고가 꺼져 있으면 비어 있습니다)</p>;
  }
  const held = heldByReason(data.held);
  return (
    <>
      <div className="fleet-status">
        {data.nodes.map((node) => (
          <span key={node.machine_id}>
            {node.machine_id}{" "}
            {node.stale ? <span className="badge stale">{node.state || "stale"}</span> : null}
            {!node.observed || !node.jobs_readable ? <span className="badge stale">관측 불가</span> : null}
            {node.truncated ? <span className="badge">잘림</span> : null}
            <span className="muted">
              후보 {node.summary.candidate} · builder 대기 {node.summary.builder_task_gate} · 보류 {node.summary.held} · 사람 세션 {node.summary.no_job} ·
              보고 <time>{node.received_at}</time>
            </span>
          </span>
        ))}
        {data.tasks_truncated ? <span className="badge stale">태스크 목록 잘림</span> : null}
      </div>
      {data.candidates.length === 0 ? (
        <p className="muted">정리 후보 없음</p>
      ) : (
        <table className="fleet-table">
          <thead>
            <tr>
              <th>머신</th>
              <th>세션</th>
              <th>pane</th>
              <th>status</th>
              <th>근거</th>
              <th>job</th>
            </tr>
          </thead>
          <tbody>
            {data.candidates.map((row) => (
              <tr key={`${row.machine}:${row.pane_id}`}>
                <td>{row.machine}</td>
                <td>{row.agent_name}</td>
                <td>{row.pane_id}</td>
                <td>{row.status}</td>
                <td>
                  {row.basis === "task-terminal" && row.task_id !== undefined ? (
                    <>
                      builder · <a href={`/ui/queue?task=${row.task_id}`}>#{row.task_id}</a> {row.task_state}
                    </>
                  ) : (
                    <>
                      {row.role ?? "role 미상"} · {row.terminal_kind} {row.terminal_at}
                    </>
                  )}
                </td>
                <td>{row.job_id}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {held.length > 0 ? (
        <details>
          <summary>보류 {data.held.length}건 — {held.map(([label, count]) => `${label} ${count}`).join(" · ")}</summary>
          <ul className="fleet-jobs">
            {data.held.map((row) => (
              <li key={`${row.machine}:${row.pane_id}`}>
                {row.machine} · {row.agent_name || "이름 없음"} · {row.pane_id} · {reapReasonLabel(row.reason)}
              </li>
            ))}
          </ul>
        </details>
      ) : null}
    </>
  );
}
