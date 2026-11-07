import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { actions } from "./index";
import { ActionNotices, TaskActions } from "./TaskActions";
import { eventText } from "../timeline";

beforeEach(() => {
  const meta = document.createElement("meta");
  meta.name = "hk-csrf";
  meta.content = "token";
  document.head.append(meta);
});
afterEach(() => { vi.restoreAllMocks(); document.querySelector('meta[name="hk-csrf"]')?.remove(); });

const byID = Object.fromEntries(actions.map(action => [action.id, action]));
const base = { id: 7, state: "developing", priority: 2, kind: "implement" };

function openMenu(id = 7) {
  fireEvent.click(screen.getByRole("button", { name: `작업 #${id} 메뉴` }));
}

function mockWrites(response: Response, lanes: string[] = []) {
  return vi.spyOn(window, "fetch").mockImplementation(async (input) => {
    if (String(input) === "/ui/api/board/lanes") return new Response(JSON.stringify({ lanes }), { status: 200 });
    return response.clone();
  });
}

function writeBody(fetcher: ReturnType<typeof mockWrites>, index: number): Record<string, unknown> {
  return JSON.parse(String(fetcher.mock.calls[index][1]?.body));
}

it("explains every lifecycle action's store guard", () => {
  const developing = { ...base };
  expect(byID["close-merged"].applicable(developing)).toBe(true);
  expect(byID["close-merged"].disabledReason(developing)).toBeNull();
  expect(byID["close-completed"].disabledReason(developing)).toContain("decide·ops·verify");
  expect(byID["drop"].disabledReason(developing)).toBeNull();
  expect(byID["hold"].disabledReason(developing)).toContain("대기 또는 착수 전");
  expect(byID["reopen"].disabledReason(developing)).toContain("종결되었거나 보류 중");
  expect(byID["relane"].disabledReason(developing)).toBeNull();

  const merged = { ...base, state: "merged" };
  expect(byID["close-merged"].disabledReason(merged)).toBe("종결된 작업은 변경할 수 없습니다");
  expect(byID["reopen"].disabledReason(merged)).toBeNull();
  expect(byID["relane"].disabledReason(merged)).toContain("레인을 옮길 수 없습니다");

  const flagged = { ...base, awaiting_decision: true };
  expect(byID["close-merged"].disabledReason(flagged)).toBe("결정 대기 중인 작업은 보류·드롭·중복만 가능");
  expect(byID["close-completed"].disabledReason(flagged)).toBe("결정 대기 중인 작업은 보류·드롭·중복만 가능");
  expect(byID["drop"].disabledReason(flagged)).toBeNull();

  const ops = { ...base, state: "reviewing", kind: "ops" };
  expect(byID["close-completed"].disabledReason(ops)).toBeNull();

  const icebox = { ...base, state: "icebox" };
  expect(byID["hold"].disabledReason(icebox)).toBeNull();
  expect(byID["drop"].disabledReason(icebox)).toContain("보류를 해제");
  expect(byID["reopen"].disabledReason(icebox)).toBeNull();
  expect(byID["hold"].disabledReason({ ...base, state: "backlog" })).toBeNull();
});

it("lists every action in the row menu and disables the guarded ones with their reason", () => {
  render(<div data-action-task="7"><TaskActions compact target={{ ...base, state: "merged" }} onPriority={() => {}} /></div>);
  openMenu();
  const menu = screen.getByRole("menu", { name: "작업 메뉴" });
  for (const label of ["우선순위 변경", "머지됨으로 닫기", "완료로 닫기", "드롭 / 중복", "보류 / 보류 해제", "다시 열기", "레인 넘기기"]) {
    expect(menu.textContent).toContain(label);
  }
  expect(screen.getByRole("menuitem", { name: /머지됨으로 닫기/ }).hasAttribute("disabled")).toBe(true);
  expect(screen.getByRole("menuitem", { name: /다시 열기/ }).hasAttribute("disabled")).toBe(false);
  expect(screen.getByRole("menuitem", { name: /보류 \/ 보류 해제/ }).getAttribute("title")).toBe("종결된 작업은 변경할 수 없습니다");
});

it("prefills merged evidence from the PR and posts one call with the path", async () => {
  const applied = vi.fn();
  const fetcher = mockWrites(new Response(JSON.stringify({ state: "merged", path: ["developing", "reviewing", "merged"] }), { status: 200 }));
  const target = { ...base, refs: { pr: "https://github.com/example/repo/pull/42" } };
  render(<div data-action-task="7"><TaskActions compact target={target} onPriority={() => {}} onApplied={applied} /></div>);
  openMenu();
  fireEvent.click(screen.getByRole("menuitem", { name: "머지됨으로 닫기" }));
  const dialog = screen.getByRole("dialog", { name: "머지됨으로 닫기" });
  expect(dialog.textContent).toContain("기록 경로: 개발 중 → 검토 중 → 머지됨");
  expect(dialog.textContent).toContain("검토 단계를 거쳐 한 번에 기록됩니다");
  expect((screen.getByLabelText("종료 근거") as HTMLInputElement).value).toBe("https://github.com/example/repo/pull/42");
  fireEvent.change(screen.getByLabelText("메모 (선택)"), { target: { value: "머지 완료" } });
  fireEvent.click(screen.getByRole("button", { name: "머지됨으로 닫기" }));
  await waitFor(() => expect(fetcher).toHaveBeenCalledOnce());
  expect(String(fetcher.mock.calls[0][0])).toBe("/ui/api/tasks/7/close-merged");
  expect(writeBody(fetcher, 0)).toMatchObject({ evidence: "https://github.com/example/repo/pull/42", reason: "머지 완료" });
  await waitFor(() => expect(applied).toHaveBeenCalled());
});

it("closes completed only with a result link", async () => {
  const fetcher = mockWrites(new Response(JSON.stringify({ state: "completed" }), { status: 200 }));
  const target = { ...base, state: "reviewing", kind: "ops" };
  render(<div data-action-task="7"><TaskActions compact target={target} onPriority={() => {}} /></div>);
  openMenu();
  fireEvent.click(screen.getByRole("menuitem", { name: "완료로 닫기" }));
  const confirm = screen.getByRole("button", { name: "완료로 닫기" });
  expect(confirm.hasAttribute("disabled")).toBe(true);
  fireEvent.change(screen.getByLabelText("결과 링크"), { target: { value: "https://example.com/run/1" } });
  expect(confirm.hasAttribute("disabled")).toBe(false);
  fireEvent.click(confirm);
  await waitFor(() => expect(fetcher).toHaveBeenCalledOnce());
  expect(writeBody(fetcher, 0)).toMatchObject({ evidence: "https://example.com/run/1" });
});

it("asks for the surviving task id when the drop type is 대체됨", async () => {
  const fetcher = mockWrites(new Response(JSON.stringify({ state: "duplicate", closure_type: "superseded" }), { status: 200 }));
  render(<div data-action-task="7"><TaskActions compact target={base} onPriority={() => {}} /></div>);
  openMenu();
  fireEvent.click(screen.getByRole("menuitem", { name: "드롭 / 중복" }));
  fireEvent.click(screen.getByLabelText("대체됨 (중복)"));
  const confirm = screen.getByRole("button", { name: "중복으로 닫기" });
  expect(confirm.hasAttribute("disabled")).toBe(true);
  fireEvent.change(screen.getByLabelText("대체 작업 번호"), { target: { value: "3312" } });
  fireEvent.change(screen.getByLabelText("이유"), { target: { value: "같은 작업이 이미 있습니다" } });
  expect(confirm.hasAttribute("disabled")).toBe(false);
  fireEvent.click(confirm);
  await waitFor(() => expect(fetcher).toHaveBeenCalledOnce());
  expect(writeBody(fetcher, 0)).toMatchObject({ closure: "superseded", duplicate_of: 3312, reason: "같은 작업이 이미 있습니다" });
});

it("requires a reason before reopening", async () => {
  const fetcher = mockWrites(new Response(JSON.stringify({ state: "unstarted" }), { status: 200 }));
  render(<div data-action-task="7"><TaskActions compact target={{ ...base, state: "merged" }} onPriority={() => {}} /></div>);
  openMenu();
  fireEvent.click(screen.getByRole("menuitem", { name: "다시 열기" }));
  const confirm = screen.getByRole("button", { name: "다시 열기" });
  expect(confirm.hasAttribute("disabled")).toBe(true);
  fireEvent.change(screen.getByLabelText("이유"), { target: { value: "머지가 되돌려졌습니다" } });
  fireEvent.click(confirm);
  await waitFor(() => expect(fetcher).toHaveBeenCalledOnce());
  expect(writeBody(fetcher, 0)).toMatchObject({ reason: "머지가 되돌려졌습니다" });
});

it("holds at once and the undo restores the exact previous state", async () => {
  const fetcher = mockWrites(new Response(JSON.stringify({ previous: "backlog", state: "icebox" }), { status: 200 }));
  render(<><ActionNotices /><div data-action-task="7"><TaskActions compact target={{ ...base, state: "backlog" }} onPriority={() => {}} /></div></>);
  openMenu();
  fireEvent.click(screen.getByRole("menuitem", { name: "보류 / 보류 해제" }));
  await waitFor(() => expect(fetcher).toHaveBeenCalledOnce());
  expect(writeBody(fetcher, 0)).toMatchObject({ held: true });
  await screen.findByText(/보류했습니다/);
  fireEvent.click(screen.getByRole("button", { name: "실행 취소 (z)" }));
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2));
  expect(String(fetcher.mock.calls[1][0])).toBe("/ui/api/tasks/7/hold");
  expect(writeBody(fetcher, 1)).toMatchObject({ held: false, to: "backlog" });
});

it("picks an existing lane for a relane and undoes it", async () => {
  const fetcher = mockWrites(new Response(JSON.stringify({ previous: "lane-a", lane: "lane-b", changed: true, relay: "sent" }), { status: 200 }), ["lane-a", "lane-b"]);
  render(<><ActionNotices /><div data-action-task="7"><TaskActions compact target={{ ...base, state: "backlog", lane: "lane-a" }} onPriority={() => {}} /></div></>);
  openMenu();
  fireEvent.click(screen.getByRole("menuitem", { name: "레인 넘기기" }));
  const select = await screen.findByLabelText("옮길 레인");
  expect((select as HTMLSelectElement).value).toBe("lane-b");
  expect(screen.queryByRole("option", { name: "lane-a" })).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "레인 옮기기" }));
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2)); // lanes + write
  expect(String(fetcher.mock.calls[1][0])).toBe("/ui/api/tasks/7/relane");
  expect(writeBody(fetcher, 1)).toMatchObject({ lane: "lane-b" });
  await screen.findByText(/레인을 옮겼습니다/);
  fireEvent.click(screen.getByRole("button", { name: "실행 취소 (z)" }));
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(3));
  expect(writeBody(fetcher, 2)).toMatchObject({ lane: "lane-a" });
});

it("shows the store refusal as a Korean toast", async () => {
  mockWrites(new Response(JSON.stringify({ error: "task_conflict" }), { status: 409 }));
  render(<><ActionNotices /><div data-action-task="7"><TaskActions compact target={base} onPriority={() => {}} /></div></>);
  openMenu();
  fireEvent.click(screen.getByRole("menuitem", { name: "머지됨으로 닫기" }));
  fireEvent.click(screen.getByRole("button", { name: "머지됨으로 닫기" }));
  const alert = await screen.findByRole("alert");
  expect(alert.textContent).toContain("작업 상태가 바뀌어 실행할 수 없습니다");
});

it("reuses the dialog's event key when a failed submit is retried", async () => {
  const fetcher = vi.spyOn(window, "fetch")
    .mockRejectedValueOnce(new TypeError("network"))
    .mockResolvedValueOnce(new Response(JSON.stringify({ state: "merged", path: ["developing", "reviewing", "merged"] }), { status: 200 }));
  render(<><ActionNotices /><div data-action-task="7"><TaskActions compact target={base} onPriority={() => {}} /></div></>);
  openMenu();
  fireEvent.click(screen.getByRole("menuitem", { name: "머지됨으로 닫기" }));
  fireEvent.click(screen.getByRole("button", { name: "머지됨으로 닫기" }));
  await screen.findByRole("alert");
  fireEvent.click(screen.getByRole("button", { name: "머지됨으로 닫기" }));
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2));
  expect(writeBody(fetcher, 0).event_key).toBe(writeBody(fetcher, 1).event_key);
});

it("shows a relay warning next to the relane undo toast", async () => {
  const fetcher = mockWrites(new Response(JSON.stringify({ previous: "lane-a", lane: "lane-b", changed: true, relay: "failed" }), { status: 200 }), ["lane-a", "lane-b"]);
  render(<><ActionNotices /><div data-action-task="7"><TaskActions compact target={{ ...base, state: "backlog", lane: "lane-a" }} onPriority={() => {}} /></div></>);
  openMenu();
  fireEvent.click(screen.getByRole("menuitem", { name: "레인 넘기기" }));
  await screen.findByLabelText("옮길 레인");
  fireEvent.click(screen.getByRole("button", { name: "레인 옮기기" }));
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2));
  const alert = await screen.findByRole("alert");
  expect(alert.textContent).toContain("레인은 옮겼지만 레인 알림을 보내지 못했습니다");
  expect(screen.getByRole("button", { name: "실행 취소 (z)" })).toBeTruthy();
});

it("keeps one undo toast: the newest action owns the z key", async () => {
  const lifecycleUndo = vi.fn().mockResolvedValue(undefined);
  const onPriority = vi.fn();
  vi.spyOn(window, "fetch").mockResolvedValue(new Response(JSON.stringify({ previous: 2, priority: 2 }), { status: 200 }));
  render(<ActionNotices />);
  window.dispatchEvent(new CustomEvent("hk-lifecycle-undo", { detail: { message: "보류했습니다", undo: lifecycleUndo } }));
  await screen.findByText(/보류했습니다/);
  window.dispatchEvent(new CustomEvent("hk-priority-undo", { detail: { id: 1, previous: 2, current: 3, onPriority } }));
  await waitFor(() => expect(screen.queryByText(/보류했습니다/)).toBeNull());
  expect(screen.getAllByRole("button", { name: "실행 취소 (z)" })).toHaveLength(1);
  fireEvent.keyDown(window, { key: "z" });
  expect(onPriority).toHaveBeenCalled();
  expect(lifecycleUndo).not.toHaveBeenCalled();
});

it("mints a new key when a failed relane changes its destination", async () => {
  const fetcher = vi.spyOn(window, "fetch")
    .mockImplementationOnce(async () => new Response(JSON.stringify({ lanes: ["lane-a", "lane-b", "lane-c"] }), { status: 200 }))
    .mockRejectedValueOnce(new TypeError("network"))
    .mockImplementationOnce(async () => new Response(JSON.stringify({ previous: "lane-a", lane: "lane-c", changed: true, relay: "sent" }), { status: 200 }));
  render(<><ActionNotices /><div data-action-task="7"><TaskActions compact target={{ ...base, state: "backlog", lane: "lane-a" }} onPriority={() => {}} /></div></>);
  openMenu();
  fireEvent.click(screen.getByRole("menuitem", { name: "레인 넘기기" }));
  const select = await screen.findByLabelText("옮길 레인");
  fireEvent.click(screen.getByRole("button", { name: "레인 옮기기" }));
  await screen.findByRole("alert");
  fireEvent.change(select, { target: { value: "lane-c" } });
  fireEvent.click(screen.getByRole("button", { name: "레인 옮기기" }));
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(3));
  const first = writeBody(fetcher, 1), second = writeBody(fetcher, 2);
  expect(first.lane).toBe("lane-b");
  expect(second.lane).toBe("lane-c");
  expect(first.event_key).not.toBe(second.event_key);
});

it("writes the timeline row in Korean for transitions, relane and closure", () => {
  expect(eventText({ id: 1, kind: "transition", from: "developing", to: "reviewing", by: "operator", at: "" })).toBe("상태 변경 · 개발 중 → 검토 중");
  expect(eventText({ id: 2, kind: "transition", from: "reviewing", to: "merged", by: "operator", note: "머지 완료", at: "" })).toBe("상태 변경 · 검토 중 → 머지됨 · 머지 완료");
  expect(eventText({ id: 3, kind: "relane", from: "lane-a", to: "lane-b", by: "operator", at: "" })).toBe("레인 변경 · lane-a → lane-b");
  expect(eventText({ id: 4, kind: "transition", from: "backlog", to: "icebox", by: "operator", note: "보류", at: "" })).toBe("상태 변경 · 대기 → 보류(보관) · 보류");
  expect(eventText({ id: 5, from: "developing", to: "developing", by: "operator", field: "priority", from_value: "2", to_value: "3", note: "priority 2 → 3: 이유", at: "" })).toBe("우선순위 P2 → P3 · 이유");
});
