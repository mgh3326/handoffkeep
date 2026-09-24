// The drawer's decision card (#618) — its own lazy chunk, loaded when a
// detail first opens, so the queue's initial bundle carries none of it.
// Read only: no buttons, no radio inputs, nothing pre-selected — answering
// is #580's permission surface. Every display state comes from the detail
// BFF (decision_requests[].state); this file never derives one from time.
//
// Imports are type-only or react: a value import of a module that lives in
// the entry would make this chunk import board.js (loaded twice under ?v=).

import type { DecisionOption, DecisionRequestView, LegacyDecisionView } from "../board/types";
import type { DetailFetchState } from "./DetailDrawer";
import type { ProtoTask } from "./types";

const RESOLUTION_LABEL: Record<string, string> = {
  answered: "답변",
  default_applied: "기본값 적용",
  withdrawn: "철회",
};

function Options({ options, allowFree, chosen }: { options: DecisionOption[]; allowFree: boolean; chosen?: string }) {
  if (options.length === 0) {
    return <p className="qp-unknown">선택지 미기록</p>;
  }
  return (
    <ul className="qp-drawer-refs" data-decision-options>
      {options.map((option) => (
        <li key={option.key}>
          <strong>{option.key}</strong> {option.label}
          {option.recommended ? <span className="qp-stale"> 권고</span> : null}
          {chosen === option.key ? <strong> ← 선택됨</strong> : null}
        </li>
      ))}
      {allowFree ? <li className="muted">직접 답변 허용</li> : null}
    </ul>
  );
}

function Request({ request }: { request: DecisionRequestView }) {
  const recommended = request.options.find((option) => option.recommended);
  const resolution = request.resolution;
  return (
    <div className="qp-decision-req" data-decision-request={request.id} data-state={request.state}>
      <p>
        <strong data-decision-state>{request.state_label}</strong> · <code>{request.id}</code> r{request.revision}
        {request.supersedes ? <span className="muted"> · {request.supersedes} 대체</span> : null}
        {request.superseded_by ? <span className="muted"> · {request.superseded_by} 로 대체됨</span> : null}
      </p>
      <p>{request.question}</p>
      <Options options={request.options} allowFree={request.allow_free} chosen={resolution?.option} />
      <p>
        권고:{" "}
        {recommended === undefined ? (
          <span className="muted">없음</span>
        ) : (
          <>
            {recommended.key}
            {request.reason ? ` — ${request.reason}` : <span className="muted"> (이유 미기록)</span>}
          </>
        )}
      </p>
      <p>
        무응답 시: {request.default_action}
        {request.default_option ? ` (선택지 ${request.default_option})` : ""}
      </p>
      <p>
        발동 조건: {request.default_trigger ? request.default_trigger : <span className="muted">발동 시점 미기록</span>} · 응답 기한:{" "}
        {request.due_at ? <time dateTime={request.due_at}>{request.due_at}</time> : <span className="muted">기한 미지정</span>}
      </p>
      {request.state === "overdue" ? (
        <p className="qp-stale-note" role="note">
          기한이 지났지만 기본값 적용 기록이 없습니다 — 적용되지 않았습니다.
        </p>
      ) : null}
      {request.state === "uncleaned" ? (
        <p className="qp-stale-note" role="note">
          종료된 태스크에 열린 요청이 남아 있습니다 — 답이 아니라 정리가 필요합니다(tasks decision-resolve).
        </p>
      ) : null}
      {resolution ? (
        <p data-decision-resolution>
          결과: {RESOLUTION_LABEL[resolution.kind] ?? resolution.kind}
          {resolution.option ? ` ${resolution.option}` : ""}
          {resolution.text ? ` — ${resolution.text}` : ""}
          {resolution.receipt ? ` · 영수증 ${resolution.receipt}` : ""} ·{" "}
          {resolution.responder ? `${resolution.responder} (기록 ${resolution.by})` : resolution.by} ·{" "}
          <time dateTime={resolution.at}>{resolution.at}</time>
        </p>
      ) : null}
      <p className="muted">
        요청 {request.requested_by} · <time dateTime={request.requested_at}>{request.requested_at}</time>
        {request.doc ? (
          <>
            {" · "}
            <a href={`/ui/doc/${request.doc}`}>요청 문서</a>
          </>
        ) : null}
      </p>
    </div>
  );
}

function Legacy({ legacy }: { legacy: LegacyDecisionView }) {
  return (
    <div className="qp-decision-req" data-decision-request="" data-state={legacy.state}>
      <p>
        <strong data-decision-state>{legacy.state_label}</strong> — 상태는 결정 필요이지만 결정 요청이 hk 에 기록되지 않았습니다. 권고·무응답
        동작·기한은 알 수 없습니다.
      </p>
      {legacy.question !== "" ? <p>{legacy.question}</p> : <p className="qp-unknown">질문 미기록</p>}
      {legacy.options.length > 0 ? <Options options={legacy.options} allowFree={legacy.allow_free} /> : null}
    </div>
  );
}

export default function DecisionCard({ task, detail }: { task: ProtoTask; detail?: DetailFetchState }) {
  if (detail === undefined) {
    // No detail fetch behind this screen (synthetic preview, tests): fixture
    // rows carry their own question; otherwise the data is "not connected",
    // never "no request" — and needs_decision still names itself.
    return task.decision ? (
      <>
        <p>{task.decision.question}</p>
        <p className="muted">evidence: {task.decision.evidence}</p>
      </>
    ) : (
      <p className="muted">
        {task.state === "needs_decision" ? "상태는 결정 필요 — " : ""}결정 요청 정보 미연결(상세 미조회)
      </p>
    );
  }
  if (detail.status === "loading") {
    return <p className="muted">결정 요청 확인 중…</p>;
  }
  if (detail.status !== "loaded") {
    return (
      <p className="qp-unknown" role="note" data-decision-error>
        결정 요청 조회 실패 — "요청 없음"이 아닙니다.
      </p>
    );
  }
  const requests = detail.data.decision_requests;
  if (requests === undefined) {
    return <p className="qp-unknown">결정 요청 미연결 — 이 서버는 결정 요청을 제공하지 않습니다.</p>;
  }
  const current = requests.find((request) => request.current);
  const earlier = requests.filter((request) => request !== current);
  const legacy = detail.data.decision_legacy ?? null;
  return (
    <>
      {current !== undefined ? <Request request={current} /> : null}
      {legacy !== null ? <Legacy legacy={legacy} /> : null}
      {current === undefined && legacy === null ? <p className="muted">기록된 결정 요청 없음</p> : null}
      {earlier.length > 0 ? (
        <details className="qp-decision-history">
          <summary>결정 이력 {earlier.length}건</summary>
          {earlier.map((request) => (
            <Request key={request.id} request={request} />
          ))}
        </details>
      ) : null}
    </>
  );
}
