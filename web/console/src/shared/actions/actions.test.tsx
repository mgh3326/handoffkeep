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
  fireEvent.click(screen.getByRole("dialog").querySelector("button")!); fireEvent.click(screen.getByRole("button", { name: "P1" }));
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
