import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { actions } from "./index";
import { ActionNotices, ConfirmDialog, TaskActions } from "./TaskActions";
import { useState } from "react";

afterEach(() => { vi.restoreAllMocks(); document.querySelector('meta[name="hk-csrf"]')?.remove(); });
it("makes the priority action applicable and explains settled tasks", () => {
  const target = { id: 1, priority: 2, state: "developing" };
  expect(actions[0].applicable(target)).toBe(true);
  expect(actions[0].disabledReason(target)).toBeNull();
  expect(actions[0].disabledReason({ ...target, state: "completed" })).toBeTruthy();
});
it("lists priority for a focused task and undo restores its previous value", async () => {
  const meta = document.createElement("meta"); meta.name = "hk-csrf"; meta.content = "token"; document.head.append(meta);
  const fetcher = vi.spyOn(window, "fetch").mockResolvedValueOnce(new Response(JSON.stringify({ previous: 2, priority: 1 }), { status: 200 })).mockResolvedValueOnce(new Response(JSON.stringify({ previous: 1, priority: 2 }), { status: 200 }));
  function Fixture() { const [priority, setPriority] = useState(2); return <><ActionNotices /><div data-action-task="1" className="action-row"><a href="#">작업 P{priority}</a>{priority === 2 && <TaskActions target={{ id: 1, state: "developing", priority }} onPriority={setPriority} />}</div></>; }
  render(<Fixture />); const row = screen.getByRole("link"); row.focus();
  fireEvent.keyDown(row, { key: "k", metaKey: true }); expect(screen.getByRole("dialog", { name: "명령 팔레트" }).textContent).toContain("우선순위 변경");
  fireEvent.click(screen.getByRole("dialog").querySelector("button")!); fireEvent.click(screen.getByRole("menuitemradio", { name: "P1 1" }));
  await screen.findByText("우선순위를 변경했습니다", { exact: false }); expect(screen.getByRole("link").textContent).toContain("P1");
  fireEvent.keyDown(window, { key: "z" }); await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2));
  expect(JSON.parse(String(fetcher.mock.calls[1][1]?.body)).priority).toBe(2);
  await waitFor(() => expect(screen.getByRole("link").textContent).toContain("P2"));
});
it("traps focus and closes the confirmation on Escape", () => {
  const close = vi.fn(); render(<ConfirmDialog title="종결" effect="작업 종결" close={close} confirm={() => {}} />);
  const cancel = screen.getByRole("button", { name: "취소" }); const confirm = screen.getByRole("button", { name: "작업 종결" });
  confirm.focus(); fireEvent.keyDown(confirm, { key: "Tab" }); expect(document.activeElement).toBe(cancel);
  fireEvent.keyDown(cancel, { key: "Escape" }); expect(close).toHaveBeenCalledOnce();
});

it("shows the current priority and accepts a number key", async () => {
  const meta = document.createElement("meta"); meta.name = "hk-csrf"; meta.content = "token"; document.head.append(meta);
  const fetcher = vi.spyOn(window, "fetch").mockResolvedValue(new Response(JSON.stringify({ previous: 2, priority: 4 }), { status: 200 }));
  render(<div data-action-task="7"><TaskActions compact target={{ id: 7, state: "developing", priority: 2 }} onPriority={() => {}} /></div>);
  fireEvent.click(screen.getByRole("button", { name: "작업 #7 우선순위 변경" }));
  const menu = screen.getByRole("menu", { name: "우선순위 선택" });
  expect(screen.getByRole("menuitemradio", { name: /P2 · 현재/ }).getAttribute("aria-checked")).toBe("true");
  expect(screen.getByRole("menuitemradio", { name: /없음/ })).toBeTruthy();
  fireEvent.keyDown(menu, { key: "4" });
  await waitFor(() => expect(fetcher).toHaveBeenCalledOnce());
  expect(JSON.parse(String(fetcher.mock.calls[0][1]?.body)).priority).toBe(4);
});

it("opens only the action belonging to the focused row or detail for the same task", () => {
  const target = { id: 101, state: "developing", priority: 2 };
  render(<><div data-action-task="101"><a href="#row">행</a><TaskActions compact target={target} onPriority={() => {}} /></div><header data-action-task="101"><a href="#detail">상세</a><TaskActions target={target} onPriority={() => {}} /></header></>);
  const row = screen.getByRole("link", { name: "행" }); row.focus();
  fireEvent.keyDown(row, { key: "k", metaKey: true });
  expect(screen.getAllByRole("dialog", { name: "명령 팔레트" })).toHaveLength(1);
  fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
  row.focus(); fireEvent.keyDown(row, { key: "p" });
  expect(screen.getAllByRole("menu", { name: "우선순위 선택" })).toHaveLength(1);
});
