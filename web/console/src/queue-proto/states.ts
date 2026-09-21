// Task state vocabulary for display: Korean names, list group order and the
// token suffix of each state color. Display only — the canonical enum and its
// meaning come from the queue API.

/** Korean display names for the canonical states (2402 Q3). JOIN stays as
 * written until the operator confirms a translation; it never means "done". */
export const STATE_LABEL: Record<string, string> = {
  backlog: "대기",
  claimed: "인수됨",
  in_progress: "진행 중",
  verifying: "검증 중",
  join: "JOIN",
  hold: "보류",
  needs_decision: "결정 필요",
  merged: "머지됨",
  dropped: "종료됨",
};

/** Group order in the list: what needs the operator first, terminal last. */
export const STATE_GROUP_ORDER = [
  "needs_decision",
  "in_progress",
  "verifying",
  "join",
  "claimed",
  "hold",
  "backlog",
  "merged",
  "dropped",
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
