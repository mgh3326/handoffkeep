// #529 — 서비스별 배포 상태 블록. Since #620 it renders on the Deploys page
// (deploys/DeploysApp), not on the queue — the queue keeps only a one-line
// summary (DeploySummary). Read-only: one GET to /ui/api/deploy-pending on
// mount and on the poll cadence; no writes, no notifications, no GitHub
// calls. The merged list heading stays "마지막 배포 이후 머지됨" — the
// surface must never claim "미배포 확정" (design doc §2 advisory), and a
// service without records renders "기록 없음", never "최신".
// Styles are inline except dp-warn/muted, which deploys.css defines — the
// queue CSS budget never carried this panel's classes.

import type { CSSProperties } from "react";
import {
  deployElapsed,
  deployPRLabel,
  deployStamp,
  fetchDeployPending,
  shortDeployedRef,
  type DeployPendingResponse,
  type DeployRecordView,
  type DeployServiceView,
} from "./deploy";
import { useDeployPending } from "./deployPoll";

const RESULT_LABEL: Record<string, string> = {
  success: "success",
  failed: "failed",
  rolled_back: "rolled_back",
};

const panelStyle: CSSProperties = {
  margin: "0 var(--hk-space-4) var(--hk-space-3)",
  padding: "var(--hk-space-3) var(--hk-space-4)",
  border: "1px solid var(--hk-border-subtle)",
  borderRadius: "var(--hk-radius-md)",
  background: "var(--hk-surface-1)",
};
const headStyle: CSSProperties = { display: "flex", alignItems: "baseline", gap: "var(--hk-space-3)" };
const titleStyle: CSSProperties = {
  margin: 0,
  fontSize: "var(--hk-font-section)",
  lineHeight: "var(--hk-leading-section)",
};
const svcStyle: CSSProperties = {
  marginTop: "var(--hk-space-2)",
  paddingTop: "var(--hk-space-2)",
  borderTop: "1px solid var(--hk-border-subtle)",
};
const svcHeadStyle: CSSProperties = {
  display: "flex",
  alignItems: "baseline",
  gap: "var(--hk-space-2)",
  fontSize: "var(--hk-font-row)",
};
const metaStyle: CSSProperties = {
  fontSize: "var(--hk-font-meta)",
  lineHeight: "var(--hk-leading-meta)",
  color: "var(--hk-text-muted)",
};
const recStyle: CSSProperties = {
  display: "flex",
  flexWrap: "wrap",
  alignItems: "baseline",
  gap: "var(--hk-space-1) var(--hk-space-3)",
  padding: "var(--hk-space-1) 0 0",
  fontSize: "var(--hk-font-meta)",
  lineHeight: "var(--hk-leading-meta)",
  color: "var(--hk-text-secondary)",
};
const noteStyle: CSSProperties = { ...metaStyle, margin: "var(--hk-space-2) 0 0" };
const monoStyle: CSSProperties = { fontFamily: "var(--hk-font-mono)" };
const mergedListStyle: CSSProperties = { margin: "var(--hk-space-1) 0 0", padding: 0, listStyle: "none" };
const mergedItemStyle: CSSProperties = {
  display: "flex",
  alignItems: "baseline",
  gap: "var(--hk-space-2)",
  minWidth: 0,
  fontSize: "var(--hk-font-meta)",
  lineHeight: "var(--hk-leading-meta)",
};
const mergedTitleStyle: CSSProperties = {
  minWidth: 0,
  overflow: "hidden",
  textOverflow: "ellipsis",
  whiteSpace: "nowrap",
  color: "var(--hk-text-secondary)",
};
const dangerStyle: CSSProperties = { color: "var(--hk-danger)" };
const invalidListStyle: CSSProperties = {
  margin: "var(--hk-space-1) 0 0",
  padding: 0,
  listStyle: "none",
};
const invalidItemStyle: CSSProperties = {
  display: "flex",
  flexWrap: "wrap",
  alignItems: "baseline",
  gap: "var(--hk-space-2)",
  fontSize: "var(--hk-font-meta)",
  lineHeight: "var(--hk-leading-meta)",
};

/** Reason classes the server emits for a malformed deploy record; anything
 * newer than this list renders verbatim. */
const INVALID_REASON_LABEL: Record<string, string> = {
  key: "키 시각 형식이 계약(YYYYMMDDTHHMMSSZ)이 아님",
  deployed_at: "deployed_at이 RFC3339가 아님",
  schema: "deploy-record/v0 본문이 아님(schema·service·result)",
};
const invalidReasonLabel = (reason: string) => INVALID_REASON_LABEL[reason] ?? reason;

function RecordLine({ label, rec, now }: { label: string; rec: DeployRecordView; now: string }) {
  const ref = shortDeployedRef(rec.deployed_ref);
  const stamp = deployStamp(rec.deployed_at);
  const elapsed = deployElapsed(now, rec.deployed_at);
  const badResult = rec.result === "failed" || rec.result === "rolled_back";
  return (
    <div style={recStyle} data-result={rec.result}>
      <span className="muted">{label}</span>
      <span style={badResult ? dangerStyle : undefined}>{RESULT_LABEL[rec.result] ?? rec.result}</span>
      <span style={monoStyle}>{ref ?? "ref 없음"}</span>
      <span>
        {stamp ?? "시각 미기록"}
        {elapsed !== null ? ` · ${elapsed}` : ""}
      </span>
      {rec.failed_step !== null ? <span className="muted">step {rec.failed_step}</span> : null}
      {rec.serving_maybe_changed ? (
        <span className="dp-warn" role="note">
          배포 명령은 완료 — 서빙 판이 바뀌었을 수 있음
        </span>
      ) : null}
    </div>
  );
}

function MergedList({ svc }: { svc: DeployServiceView }) {
  if (svc.merged_boundary === "unrecorded") {
    return <p style={noteStyle}>배포 시각(deployed_at) 미기록 — 마지막 배포 이후 머지 목록의 기준이 없습니다.</p>;
  }
  if (svc.merged_boundary === "no_current") {
    return (
      <p style={noteStyle}>
        {svc.docs_capped
          ? "조회 범위 안에 성공 배포 기록이 없어 마지막 배포 이후 머지 목록을 계산할 수 없습니다."
          : "성공 배포 기록이 없어 마지막 배포 이후 머지 목록을 계산할 수 없습니다."}
      </p>
    );
  }
  return (
    <div style={{ marginTop: "var(--hk-space-1)" }}>
      <div style={metaStyle}>마지막 배포 이후 머지됨 ({svc.merged_since.length})</div>
      {svc.merged_since.length === 0 ? (
        <p style={noteStyle}>해당 없음</p>
      ) : (
        <ul style={mergedListStyle}>
          {svc.merged_since.map((task) => (
            <li key={task.task_id} style={mergedItemStyle}>
              <a href={`/ui/tasks/${task.task_id}`}>#{task.task_id}</a>
              <span style={mergedTitleStyle}>{task.title}</span>
              <a href={task.pr} target="_blank" rel="noreferrer">
                {deployPRLabel(task.pr)}
              </a>
              <span className="muted">{deployStamp(task.merged_at) ?? task.merged_at}</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function ServiceRow({ svc, now }: { svc: DeployServiceView; now: string }) {
  return (
    <section style={svcStyle} aria-label={`${svc.service} 배포`}>
      <div style={svcHeadStyle}>
        <span style={{ fontWeight: "var(--hk-weight-strong)" }}>{svc.service}</span>
        {svc.target ? <span className="muted">{svc.target}</span> : null}
        <span style={metaStyle}>{svc.repo}</span>
      </div>
      {svc.current === null ? (
        <div style={recStyle} data-result="none">
          {/* Under a capped scan, "no success" only holds inside the window —
              an older success may exist beyond it, so say "unverified", not
              "none". */}
          <span className="muted">
            {svc.record_count === 0
              ? "기록 없음"
              : svc.docs_capped
                ? "조회 범위 안에 성공 배포 기록이 없습니다 — 상한 밖은 미확인"
                : "성공 배포 기록 없음"}
          </span>
        </div>
      ) : (
        /* docs_capped means the scan stopped before the end of the key space —
           a record beyond the cap could carry a later deployed_at, so the
           shown current/latest are in-window results, not fleet truth. */
        <RecordLine label={svc.docs_capped ? "현재 판(조회 범위 내)" : "현재 판"} rec={svc.current} now={now} />
      )}
      {svc.latest !== undefined ? (
        <RecordLine label={svc.docs_capped ? "최근 시도(조회 범위 내)" : "최근 시도"} rec={svc.latest} now={now} />
      ) : null}
      {svc.docs_capped ? (
        <p className="dp-warn" style={{ margin: "var(--hk-space-1) 0 0", fontSize: "var(--hk-font-meta)" }}>
          기록 조회 상한에 닿았습니다 — 더 오래된 배포 기록이 있을 수 있어 현재 판·최근 시도·머지 목록 모두 조회 범위
          안의 결과입니다.
        </p>
      ) : null}
      {svc.invalid_count !== undefined && svc.invalid_count > 0 ? (
        <div style={{ margin: "var(--hk-space-1) 0 0" }}>
          <p className="dp-warn" style={{ margin: 0, fontSize: "var(--hk-font-meta)" }}>
            형식이 맞지 않는 기록 {svc.invalid_count}건은 표시하지 않았습니다.
          </p>
          {/* #620 AC4 — each malformed record names its key and every failed
              check so the operator knows what to rewrite; the count-only
              warning never did. An older server sends no list — the count
              line still stands on its own then. */}
          {svc.invalid !== undefined && svc.invalid.length > 0 ? (
            <ul style={invalidListStyle} data-testid="deploy-invalid-list">
              {svc.invalid.map((rec) => (
                <li key={rec.key} style={invalidItemStyle}>
                  <span style={monoStyle}>{rec.key}</span>
                  <span className="muted">{rec.reasons.map(invalidReasonLabel).join(" · ")}</span>
                </li>
              ))}
            </ul>
          ) : null}
        </div>
      ) : null}
      <MergedList svc={svc} />
    </section>
  );
}

type Props = {
  fetchStatus?: () => Promise<DeployPendingResponse>;
  pollMs?: number;
};

/** Self-fetching block: owns its own poll via useDeployPending so the queue
 * list's dataset and types stay untouched. A failed refresh keeps the last
 * good payload with a warning; a first-load failure is an explicit error,
 * never an empty board. */
export function DeployPanel({ fetchStatus = fetchDeployPending, pollMs = 15_000 }: Props) {
  const { data, failed } = useDeployPending(fetchStatus, pollMs);

  if (data === null) {
    return (
      <section style={panelStyle} aria-label="배포 상태" data-deploy-state={failed ? "error" : "loading"}>
        <p style={noteStyle}>{failed ? "배포 정보를 불러오지 못했습니다 — 다시 시도합니다." : "배포 정보를 불러오는 중…"}</p>
      </section>
    );
  }
  return (
    <section style={panelStyle} aria-label="배포 상태" data-deploy-state={failed ? "stale" : "ready"}>
      <div style={headStyle}>
        <h2 style={titleStyle}>배포</h2>
        {failed ? <span className="dp-warn">갱신 실패 — 이전 자료를 보여 주는 중</span> : null}
        {data.events_capped ? (
          <span className="dp-warn">머지 이벤트 조회 상한에 닿아 목록이 잘렸을 수 있습니다.</span>
        ) : null}
      </div>
      {data.services.map((svc) => (
        <ServiceRow key={svc.service} svc={svc} now={data.generated_at} />
      ))}
      <p style={noteStyle}>
        배포가 필요 없는 머지(문서·CI·스킬)도 목록에 섞여 있을 수 있습니다. PR 귀속은 태스크의 refs.pr 기준이며, refs.pr 은 종결 PR
        이 아니라 출처 PR일 수 있습니다(#494 이전).
      </p>
    </section>
  );
}
