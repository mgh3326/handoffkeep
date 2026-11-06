// A decision's own chips: record state, due time, source. They reuse the D1
// chip classes (.hk-chip--{group}), so the colors come from the chip tokens;
// every chip prints its words and, for the urgent ones, an icon, so meaning
// never rides on color alone.

import { Icon } from "../shared/Icon";
import type { ChipGroup } from "../shared/states";
import type { RecordState } from "./types";

export type StateChip = { group: ChipGroup; label: string; icon?: "warning" | "check" };

const KNOWN: Record<Exclude<RecordState, "unknown">, StateChip> = {
  open: { group: "decision", label: "열림 · 응답 대기" },
  overdue: { group: "failed", label: "기한 경과 · 기본값 미적용", icon: "warning" },
  uncleaned: { group: "input", label: "미정리 요청" },
  answered: { group: "done", label: "답변됨", icon: "check" },
  default_applied: { group: "neutral", label: "기본값 적용됨" },
  withdrawn: { group: "neutral", label: "철회됨" },
  superseded: { group: "neutral", label: "대체됨" },
  unrecorded: { group: "neutral", label: "미기록" },
};

/** The chip of a record state; a state this console does not know keeps its
 * raw value visible. */
export function stateChip(raw: string): StateChip {
  if (Object.prototype.hasOwnProperty.call(KNOWN, raw)) return KNOWN[raw as keyof typeof KNOWN];
  return { group: "failed", label: `지원되지 않는 상태 ${raw}`, icon: "warning" };
}

/** The StateIcon shape that stands for a record state in a list row. */
export function stateShape(raw: string): string {
  switch (raw) {
    case "open":
    case "overdue":
      return "awaiting_decision";
    case "uncleaned":
      return "triage";
    case "answered":
    case "default_applied":
      return "completed";
    case "withdrawn":
    case "superseded":
      return "dropped";
    default:
      return "unknown";
  }
}

const SOURCES: Record<string, string> = {
  task_request: "요청",
  task_flag: "태스크 표시",
  job_escalate: "작업 에스컬레이션",
  lane_event: "알림",
  chat: "채팅",
  linear: "Linear",
};

export function sourceLabel(source: string): string {
  return Object.prototype.hasOwnProperty.call(SOURCES, source) ? SOURCES[source] : source;
}

export function DecisionChip({ group, label, icon }: StateChip) {
  return (
    <span className={`hk-chip hk-chip--${group}`} data-chip="decision" data-group={group}>
      {icon ? <Icon name={icon} /> : null}
      {label}
    </span>
  );
}
