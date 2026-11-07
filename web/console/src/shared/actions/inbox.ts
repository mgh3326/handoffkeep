// The inbox half of the console action registry (hk 3343). C1's registry
// (./index) describes actions on a task; this one describes triage of an
// attention item — the row Home and Work show. Both share the contract: one
// entry per action, with applicable/disabledReason in Korean, so a row, the
// bulk bar and Cmd-K render from the same list.

import type { ActionTarget } from "./index";

/** One attention item as a triage target. task is present when the row is a
 * task, so the task registry can act on it too. */
export type InboxTarget = {
  key: string;
  read: boolean;
  done: boolean;
  woken: boolean;
  task?: ActionTarget;
};

export type InboxActionId = "done" | "read" | "unread" | "snooze" | "priority" | "hold";

export type InboxAction = {
  id: InboxActionId;
  label: string;
  keywords: string[];
  shortcut?: string;
  /** What the operator is told to expect; the toast offers undo. */
  confirm: "none" | "undo";
  applicable: (target: InboxTarget) => boolean;
  disabledReason: (target: InboxTarget) => string | null;
};

const settled = new Set(["merged", "deployed", "completed", "dropped", "duplicate", "canceled"]);

export const inboxActions: InboxAction[] = [
  {
    id: "done",
    label: "완료",
    keywords: ["done", "완료", "e"],
    shortcut: "e",
    confirm: "undo",
    applicable: target => !target.done,
    disabledReason: target => (target.done ? "이미 완료한 항목입니다" : null),
  },
  {
    id: "read",
    label: "읽음",
    keywords: ["read", "읽음", "u"],
    shortcut: "u",
    confirm: "undo",
    applicable: target => !target.read,
    disabledReason: target => (target.read ? "이미 읽은 항목입니다" : null),
  },
  {
    id: "unread",
    label: "안 읽음",
    keywords: ["unread", "안 읽음", "u"],
    shortcut: "u",
    confirm: "undo",
    applicable: target => target.read,
    disabledReason: target => (target.read ? null : "아직 읽지 않은 항목입니다"),
  },
  {
    id: "snooze",
    label: "다시 알림",
    keywords: ["snooze", "다시 알림", "h"],
    shortcut: "h",
    confirm: "undo",
    applicable: () => true,
    disabledReason: () => null,
  },
  {
    id: "priority",
    label: "우선순위",
    keywords: ["priority", "우선순위", "p"],
    shortcut: "p",
    confirm: "undo",
    applicable: target => target.task !== undefined,
    disabledReason: target => {
      if (!target.task) return "작업 항목에만 적용됩니다";
      return settled.has(target.task.state) ? "종결된 작업은 변경할 수 없습니다" : null;
    },
  },
  {
    id: "hold",
    label: "보류",
    keywords: ["hold", "보류"],
    confirm: "undo",
    applicable: target => target.task !== undefined,
    // Task park belongs to the lifecycle work (hk 3340). The entry stays
    // visible so the bulk bar speaks the same vocabulary, and refuses with a
    // reason instead of disappearing.
    disabledReason: () => "보류는 작업 수명주기 콘솔 작업에서 제공합니다",
  },
];

/** The action's Korean label. */
export function inboxActionLabel(id: InboxActionId): string {
  return inboxActions.find(action => action.id === id)?.label ?? id;
}

const KST_OFFSET_MS = 9 * 60 * 60 * 1000;

export type SnoozePreset = { id: string; label: string; at: Date };

/** The snooze presets the picker offers: 1시간, the next 저녁 18:00 and 내일
 * 아침 09:00 in KST, and 다음 주 월요일 09:00 KST. Computed from now so a
 * preset never points at a time that already passed. */
export function snoozePresets(now: Date): SnoozePreset[] {
  const kst = new Date(now.getTime() + KST_OFFSET_MS);
  const year = kst.getUTCFullYear();
  const month = kst.getUTCMonth();
  const day = kst.getUTCDate();
  const kstAt = (dayOffset: number, hour: number) =>
    new Date(Date.UTC(year, month, day + dayOffset, hour) - KST_OFFSET_MS);
  const eveningToday = kstAt(0, 18);
  const evening = eveningToday.getTime() > now.getTime() ? eveningToday : kstAt(1, 18);
  const eveningLabel = evening.getTime() === eveningToday.getTime() ? "오늘 저녁 18:00" : "내일 저녁 18:00";
  const weekday = kst.getUTCDay();
  const toMonday = weekday === 1 ? 7 : (8 - weekday) % 7;
  return [
    { id: "hour", label: "1시간", at: new Date(now.getTime() + 60 * 60 * 1000) },
    { id: "evening", label: eveningLabel, at: evening },
    { id: "morning", label: "내일 아침 09:00", at: kstAt(1, 9) },
    { id: "monday", label: "다음 주 월요일 09:00", at: kstAt(toMonday, 9) },
  ];
}
