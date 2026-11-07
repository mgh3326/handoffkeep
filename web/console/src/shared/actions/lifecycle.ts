import type { Action, ActionTarget } from "./index";

const settled = new Set(["merged", "deployed", "completed", "dropped", "duplicate"]);
const completable = new Set(["decide", "ops", "verify"]);
const mergeable = new Set(["reviewing", "researching", "developing"]);
const holdable = new Set(["backlog", "unstarted"]);

const closedReason = "종결된 작업은 변경할 수 없습니다";
const awaitingReason = "결정 대기 중인 작업은 보류·드롭·중복만 가능";

/** The lifecycle actions of one task. Every entry is applicable so the row
 * menu can show the whole set; disabledReason mirrors the store's guards and
 * is the only thing that blocks a click. */
export const lifecycleActions: Action[] = [
  {
    id: "close-merged",
    label: "머지됨으로 닫기",
    keywords: ["merge", "머지", "닫기", "close"],
    kind: "command",
    confirm: "dialog",
    applicable: () => true,
    disabledReason: (target: ActionTarget) =>
      settled.has(target.state) ? closedReason
        : target.awaiting_decision ? awaitingReason
          : mergeable.has(target.state) ? null
            : "검토 중이거나 개발 중인 작업만 머지됨으로 닫을 수 있습니다",
  },
  {
    id: "close-completed",
    label: "완료로 닫기",
    keywords: ["complete", "완료", "닫기", "close"],
    kind: "command",
    confirm: "dialog",
    applicable: () => true,
    disabledReason: (target: ActionTarget) =>
      settled.has(target.state) ? closedReason
        : target.awaiting_decision ? awaitingReason
          : target.state === "triage" ? "분류 대기 중인 작업은 완료로 닫을 수 없습니다"
            : completable.has(target.kind ?? "") ? null
              : "decide·ops·verify 작업만 완료로 닫을 수 있습니다",
  },
  {
    id: "drop",
    label: "드롭 / 중복",
    keywords: ["drop", "드롭", "중복", "duplicate", "cancel"],
    kind: "command",
    confirm: "dialog",
    applicable: () => true,
    disabledReason: (target: ActionTarget) =>
      settled.has(target.state) ? closedReason
        : target.state === "icebox" ? "보류 중인 작업은 먼저 보류를 해제해야 합니다"
          : null,
  },
  {
    id: "hold",
    label: "보류 / 보류 해제",
    keywords: ["hold", "보류", "icebox", "보관"],
    kind: "command",
    confirm: "undo",
    applicable: () => true,
    disabledReason: (target: ActionTarget) =>
      settled.has(target.state) ? closedReason
        : target.state === "icebox" || holdable.has(target.state) ? null
          : "대기 또는 착수 전 작업만 보류할 수 있습니다",
  },
  {
    id: "reopen",
    label: "다시 열기",
    keywords: ["reopen", "다시", "열기"],
    kind: "command",
    confirm: "dialog",
    applicable: () => true,
    disabledReason: (target: ActionTarget) =>
      settled.has(target.state) || target.state === "icebox" ? null
        : "종결되었거나 보류 중인 작업만 다시 열 수 있습니다",
  },
  {
    id: "relane",
    label: "레인 넘기기",
    keywords: ["relane", "레인", "lane", "옮기기"],
    kind: "command",
    confirm: "undo",
    applicable: () => true,
    disabledReason: (target: ActionTarget) =>
      settled.has(target.state) ? "종결된 작업은 레인을 옮길 수 없습니다" : null,
  },
];
