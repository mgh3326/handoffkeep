import type { SubmitResult } from "./types";
const labels: Record<SubmitResult | "unconfirmed", string> = {
  answered: "답변됨",
  conflict: "다른 답이 먼저 기록됨",
  stale_revision: "이전 revision · 다시 확인 필요",
  pending_confirmation: "접수 · 확인 대기",
  expired: "기본값 적용 후 도착",
  superseded: "대체된 요청",
  unknown: "지원되지 않는 결과",
  unconfirmed: "결과 미확인",
};
export function resultLabel(
  result: SubmitResult | "unconfirmed",
  raw?: string,
) {
  return labels[result] + (result === "unknown" && raw ? ` ${raw}` : "");
}
const groups: Record<SubmitResult | "unconfirmed", string> = {
  answered: "done",
  pending_confirmation: "input",
  conflict: "failed",
  stale_revision: "failed",
  expired: "neutral",
  superseded: "neutral",
  unknown: "failed",
  unconfirmed: "input",
};
export function ResultBadge({
  result,
  raw,
}: {
  result: SubmitResult | "unconfirmed";
  raw?: string;
}) {
  return (
    <span
      className={`hk-chip hk-chip--${groups[result]} decision-result decision-result-${result}`}
    >
      {resultLabel(result, raw)}
    </span>
  );
}
