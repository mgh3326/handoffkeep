import type { ReactNode } from "react";
import { liveSectionLabel, type LiveResponse } from "../live";

/**
 * Live status strip under the queue header (#598). It always names what the
 * hub sections are doing — counts when healthy, the failure kind when not —
 * so a hub outage can never pass for an idle fleet, and a cached comparison
 * is never presented as current. Mismatches (a live-state task with no active
 * job, a hub job attached to no task) are listed explicitly.
 */
export function LiveStrip({ live }: { live: LiveResponse }) {
  const parts: ReactNode[] = [];
  const { jobs, nodes, mismatch } = live;
  if (jobs.fetched_at !== "") {
    parts.push(
      <span key="counts" className="qp-livestat">
        live · 잡 {jobs.items.length} · 머신 {nodes.items.length}
      </span>,
    );
  }
  if (jobs.status !== "ok") {
    parts.push(
      <span key="jobs" className="qp-status-warn">
        ⚠ 허브 잡: {liveSectionLabel(jobs.status)}
        {jobs.fetched_at === "" ? " · 받은 자료 없음" : ` · ${jobs.fetched_at} 자료 표시 중`}
      </span>,
    );
  }
  if (nodes.status !== "ok") {
    parts.push(
      <span key="nodes" className="qp-status-warn">
        ⚠ 허브 머신: {liveSectionLabel(nodes.status)}
        {nodes.fetched_at === "" ? " · 받은 자료 없음" : ` · ${nodes.fetched_at} 자료 표시 중`}
      </span>,
    );
  }
  if (mismatch.basis === "unavailable") {
    parts.push(
      <span key="mm" className="qp-status-warn">
        ⚠ 잡-태스크 대조 불가 · 허브 잡 자료 없음
      </span>,
    );
  } else if (mismatch.tasks_without_job.length > 0 || mismatch.jobs_without_task.length > 0) {
    const shown = mismatch.jobs_without_task.slice(0, 5).join(", ");
    parts.push(
      <span key="mm" className="qp-status-warn">
        ⚠ 어긋남
        {mismatch.tasks_without_job.length > 0
          ? ` · 잡 없는 태스크 ${mismatch.tasks_without_job.map((id) => `#${id}`).join(", ")}`
          : ""}
        {mismatch.jobs_without_task.length > 0
          ? ` · 태스크 없는 활성 잡 ${mismatch.jobs_without_task.length}건 (${shown}${mismatch.jobs_without_task.length > 5 ? "…" : ""})`
          : ""}
        {mismatch.basis === "cached" ? " · 마지막 성공 잡 목록 기준" : ""}
      </span>,
    );
  }
  if (live.tasks_truncated) {
    parts.push(
      <span key="trunc" className="qp-status-warn">
        ⚠ 태스크 목록 일부만 조회됨 — 잡 연결이 누락될 수 있습니다
      </span>,
    );
  }
  if (parts.length === 0) {
    return null;
  }
  return (
    <div className="qp-livestrip" role="status" data-testid="live-strip">
      {parts}
    </div>
  );
}
