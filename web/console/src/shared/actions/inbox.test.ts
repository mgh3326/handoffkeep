import { expect, it } from "vitest";
import { actionState } from "./InboxTriage";
import { inboxActions, snoozePresets, type InboxTarget } from "./inbox";

const target = (over: Partial<InboxTarget> = {}): InboxTarget => ({ key: "task:1", read: false, done: false, woken: false, ...over });
const find = (id: string) => {
  const action = inboxActions.find(candidate => candidate.id === id);
  if (!action) throw new Error(`missing action ${id}`);
  return action;
};

it("keeps one registry entry per inbox action with Korean refusals", () => {
  const done = find("done");
  expect(done.applicable(target())).toBe(true);
  expect(done.disabledReason(target())).toBeNull();
  expect(done.disabledReason(target({ done: true }))).toContain("이미 완료");

  const read = find("read");
  expect(read.applicable(target())).toBe(true);
  expect(read.disabledReason(target({ read: true }))).toContain("이미 읽");
  // A woken item is acknowledged by reading it again.
  expect(read.applicable(target({ read: true, woken: true }))).toBe(true);
  expect(read.disabledReason(target({ read: true, woken: true }))).toBeNull();

  const unread = find("unread");
  expect(unread.applicable(target())).toBe(false);
  expect(unread.applicable(target({ read: true }))).toBe(true);

  const snooze = find("snooze");
  expect(snooze.applicable(target())).toBe(true);
  expect(snooze.disabledReason(target())).toBeNull();
});

it("applies priority and hold only to task rows, with a reason", () => {
  const priority = find("priority");
  expect(priority.disabledReason(target())).toContain("작업 항목");
  const task = { id: 7, state: "developing", priority: 2 };
  expect(priority.disabledReason(target({ task }))).toBeNull();
  expect(priority.disabledReason(target({ task: { ...task, state: "completed" } }))).toContain("종결");
  const hold = find("hold");
  expect(hold.disabledReason(target({ task }))).toContain("보류");
});

it("reads an action over a selection as shown, enabled or refused", () => {
  const done = find("done");
  expect(actionState(done, [target({ key: "task:1" })])).toEqual({ shown: true, disabled: false, reason: null });
  // A row that is already done takes no part in the selection's done action.
  expect(actionState(done, [target({ key: "task:1", done: true })])).toEqual({ shown: false, disabled: true, reason: null });
  expect(actionState(done, [target({ key: "task:1", done: true }), target({ key: "task:2" })])).toEqual({ shown: true, disabled: false, reason: null });
  expect(actionState(find("read"), [target({ read: true })])).toEqual({ shown: false, disabled: true, reason: null });
});

it("offers KST snooze presets that are all in the future", () => {
  const now = new Date("2026-11-07T00:00:00Z"); // 09:00 KST, a Saturday
  const presets = snoozePresets(now);
  expect(presets.map(preset => preset.id)).toEqual(["hour", "evening", "morning", "monday"]);
  expect(presets[0].at.toISOString()).toBe("2026-11-07T01:00:00.000Z");
  expect(presets[1].at.toISOString()).toBe("2026-11-07T09:00:00.000Z");
  expect(presets[2].at.toISOString()).toBe("2026-11-08T00:00:00.000Z");
  for (const preset of presets) expect(preset.at.getTime()).toBeGreaterThan(now.getTime());
  const monday = presets[3].at;
  expect(monday.toISOString()).toBe("2026-11-09T00:00:00.000Z");
});

it("moves the evening preset to tomorrow once today's 18:00 KST has passed", () => {
  const now = new Date("2026-11-07T10:00:00Z"); // 19:00 KST
  const presets = snoozePresets(now);
  expect(presets[1].label).toBe("내일 저녁 18:00");
  expect(presets[1].at.toISOString()).toBe("2026-11-08T09:00:00.000Z");
});
