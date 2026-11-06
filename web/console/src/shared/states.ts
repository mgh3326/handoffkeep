// Task state vocabulary for display: Korean names, list group order and the
// token suffix of each state color. Display only — the canonical enum and its
// meaning come from the queue API.

/** Korean display names for the canonical states.
 * Legacy names (pre-hierarchy) are kept so event history still renders
 * correctly — the server writes old names into event from/to on rows that
 * predate the migration. */
export const STATE_LABEL: Record<string, string> = {
  // ---- current states (twelve, post-hierarchy) ----
  triage: "분류 대기",
  backlog: "대기",
  unstarted: "착수 전",
  researching: "조사 중",
  developing: "개발 중",
  reviewing: "검토 중",
  merged: "머지됨",
  deployed: "배포됨",
  completed: "완료됨",
  dropped: "종료됨",
  duplicate: "중복",
  icebox: "보류(보관)",
  // ---- virtual group label (not a state; driven by awaiting_decision flag) ----
  awaiting_decision: "결정 필요",
  // ---- legacy names kept for event history rendering ----
  reopened: "재개됨",
  claimed: "인수됨",
  in_progress: "진행 중",
  verifying: "검증 중",
  join: "JOIN",
  hold: "보류",
  needs_decision: "결정 필요",
};

/** Group order in the list: what needs the operator first, terminal last.
 * Lists only current states; awaiting_decision is a virtual group prepended
 * by groupByState when any task carries the flag. */
export const STATE_GROUP_ORDER = [
  "triage",
  "backlog",
  "unstarted",
  "researching",
  "developing",
  "reviewing",
  "merged",
  "deployed",
  "completed",
  "dropped",
  "duplicate",
  "icebox",
] as const;

export function isKnownState(state: string): boolean {
  return Object.prototype.hasOwnProperty.call(STATE_LABEL, state);
}

/** Label for a state; an unsupported enum keeps its raw value visible next
 * to the generic name so the original is never lost. */
export function stateLabel(state: string): string {
  return isKnownState(state) ? STATE_LABEL[state] : `알 수 없는 상태 (${state})`;
}

/** Token suffix for a state's color: enum underscores become hyphens. */
export function stateToken(state: string): string {
  return isKnownState(state) ? state.replace(/_/g, "-") : "unknown";
}
