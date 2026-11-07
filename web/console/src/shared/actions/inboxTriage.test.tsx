import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { TriageUi, useInboxTriage, type TriageRow } from "./InboxTriage";

function Fixture({ rows }: { rows: TriageRow[] }) {
  const triage = useInboxTriage(rows, () => {});
  return (
    <div>
      {rows.map(row => (
        <a key={row.key} href={`#${row.key}`} data-inbox-row data-inbox-key={row.key} data-selected={triage.selected.has(row.key) ? "true" : undefined}>
          {row.key}
        </a>
      ))}
      <input aria-label="메모" />
      <TriageUi triage={triage} />
    </div>
  );
}

const row = (key: string, over: Partial<TriageRow> = {}): TriageRow => ({ key, read: false, done: false, woken: false, ...over });
const taskRow = (key: string): TriageRow => ({ ...row(key), task: { id: Number(key.split(":")[1]), state: "developing", priority: 2 } });

function writeResult(items: string[], action = "done") {
  return new Response(JSON.stringify({
    action,
    items: items.map(item_key => ({ item_key, read: action === "read", done: action === "done", snoozed_until: action === "snooze" ? "2026-11-08T00:00:00Z" : null })),
    failed: [],
    skipped: 0,
  }));
}

let fetcher: ReturnType<typeof vi.fn>;

beforeEach(() => {
  const meta = document.createElement("meta");
  meta.name = "hk-csrf";
  meta.content = "token";
  document.head.append(meta);
  fetcher = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const body = typeof init?.body === "string" ? JSON.parse(init.body) : { items: [] };
    const path = String(input);
    if (path === "/ui/api/inbox/undo") return new Response(JSON.stringify({ items: [], conflicted: [] }));
    return writeResult(body.items ?? [], body.action);
  });
  vi.stubGlobal("fetch", fetcher);
});

afterEach(() => {
  vi.unstubAllGlobals();
  document.querySelector('meta[name="hk-csrf"]')?.remove();
});

function focusRow(key: string) {
  const link = screen.getByText(key, { selector: "[data-inbox-row]" });
  link.focus();
  return link;
}

function lastBody(call = 0) {
  const entry = fetcher.mock.calls[call];
  return JSON.parse(String((entry[1] as RequestInit | undefined)?.body));
}

it("moves focus with j and k", () => {
  render(<Fixture rows={[row("task:1"), row("task:2"), row("task:3")]} />);
  const first = focusRow("task:1");
  fireEvent.keyDown(first, { key: "j" });
  expect(document.activeElement?.getAttribute("data-inbox-key")).toBe("task:2");
  fireEvent.keyDown(document.activeElement!, { key: "j" });
  expect(document.activeElement?.getAttribute("data-inbox-key")).toBe("task:3");
  fireEvent.keyDown(document.activeElement!, { key: "j" });
  expect(document.activeElement?.getAttribute("data-inbox-key")).toBe("task:3");
  fireEvent.keyDown(document.activeElement!, { key: "k" });
  expect(document.activeElement?.getAttribute("data-inbox-key")).toBe("task:2");
});

it("e completes the focused row and u toggles read", async () => {
  render(<Fixture rows={[row("task:1"), row("task:2", { read: true })]} />);
  fireEvent.keyDown(focusRow("task:1"), { key: "e" });
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1));
  expect(lastBody()).toMatchObject({ action: "done", items: ["task:1"] });
  expect(await screen.findByText(/완료 1건/)).toBeTruthy();

  fireEvent.keyDown(focusRow("task:2"), { key: "u" });
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2));
  expect(lastBody(1)).toMatchObject({ action: "unread", items: ["task:2"] });
});

it("h opens the snooze picker and a preset sends its time", async () => {
  render(<Fixture rows={[row("task:1")]} />);
  fireEvent.keyDown(focusRow("task:1"), { key: "h" });
  const dialog = screen.getByRole("dialog", { name: "다시 알림 시각" });
  const before = Date.now();
  fireEvent.click(within(dialog).getByRole("button", { name: /1시간/ }));
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1));
  const body = lastBody();
  expect(body.action).toBe("snooze");
  expect(body.items).toEqual(["task:1"]);
  const until = Date.parse(body.until);
  expect(until).toBeGreaterThan(before + 59 * 60 * 1000);
  expect(until).toBeLessThanOrEqual(Date.now() + 61 * 60 * 1000);
});

it("lets a woken item be read again, and u sends read for it", async () => {
  render(<Fixture rows={[row("task:1", { read: true, woken: true })]} />);
  fireEvent.keyDown(focusRow("task:1"), { key: "u" });
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1));
  expect(lastBody()).toMatchObject({ action: "read", items: ["task:1"] });
});

it("? opens the shortcut help", () => {
  render(<Fixture rows={[row("task:1")]} />);
  fireEvent.keyDown(focusRow("task:1"), { key: "?" });
  const dialog = screen.getByRole("dialog", { name: "단축키 도움말" });
  expect(within(dialog).getByText("완료")).toBeTruthy();
  expect(within(dialog).getByText("실행 취소")).toBeTruthy();
  expect(within(dialog).getByText("범위 선택")).toBeTruthy();
});

it("x selects a row, Shift+x selects the range, and the bar lists the actions", () => {
  render(<Fixture rows={[taskRow("task:1"), taskRow("task:2"), taskRow("task:3")]} />);
  fireEvent.keyDown(focusRow("task:1"), { key: "x" });
  let bar = screen.getByRole("region", { name: "선택 항목 작업" });
  expect(within(bar).getByText("1개 선택")).toBeTruthy();
  expect(within(bar).getByRole("button", { name: "완료" })).toBeTruthy();
  expect(within(bar).getByRole("button", { name: "우선순위" })).toBeTruthy();
  expect(within(bar).getByRole("button", { name: "보류" }).hasAttribute("disabled")).toBe(true);

  fireEvent.keyDown(focusRow("task:3"), { key: "X", shiftKey: true });
  bar = screen.getByRole("region", { name: "선택 항목 작업" });
  expect(within(bar).getByText("3개 선택")).toBeTruthy();
});

it("caps a bulk write at 50 and says how many were left", async () => {
  const rows = Array.from({ length: 51 }, (_, index) => row(`task:${index + 1}`));
  render(<Fixture rows={rows} />);
  fireEvent.keyDown(focusRow("task:1"), { key: "x" });
  fireEvent.keyDown(focusRow("task:51"), { key: "X", shiftKey: true });
  const bar = screen.getByRole("region", { name: "선택 항목 작업" });
  expect(within(bar).getByText("51개 선택")).toBeTruthy();
  fireEvent.click(within(bar).getByRole("button", { name: "완료" }));
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1));
  const body = lastBody();
  expect(body.items).toHaveLength(50);
  expect(body.items[0]).toBe("task:1");
  expect(body.items[49]).toBe("task:50");
  expect(await screen.findByText(/51건 중 앞 50건만 처리했습니다/)).toBeTruthy();
});

it("undo restores the whole batch with the write's idempotency key", async () => {
  render(<Fixture rows={[row("task:1"), row("task:2")]} />);
  fireEvent.keyDown(focusRow("task:1"), { key: "x" });
  fireEvent.keyDown(focusRow("task:2"), { key: "x" });
  const bar = screen.getByRole("region", { name: "선택 항목 작업" });
  fireEvent.click(within(bar).getByRole("button", { name: "완료" }));
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1));
  const write = lastBody();
  expect(write.items).toEqual(["task:1", "task:2"]);
  const undo = await screen.findByRole("button", { name: "실행 취소 (z)" });
  fireEvent.click(undo);
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2));
  expect(String(fetcher.mock.calls[1][0])).toBe("/ui/api/inbox/undo");
  expect(lastBody(1)).toEqual({ event_key: write.event_key });
  expect(await screen.findByText("완료 실행 취소")).toBeTruthy();
});

it("z undoes the last write while a row has focus", async () => {
  render(<Fixture rows={[row("task:1")]} />);
  fireEvent.keyDown(focusRow("task:1"), { key: "e" });
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1));
  fireEvent.keyDown(focusRow("task:1"), { key: "z" });
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2));
  expect(String(fetcher.mock.calls[1][0])).toBe("/ui/api/inbox/undo");
});

it("Cmd-K lists the selection's actions and the four destinations", () => {
  render(<Fixture rows={[taskRow("task:1"), taskRow("task:2")]} />);
  fireEvent.keyDown(focusRow("task:1"), { key: "x" });
  fireEvent.keyDown(focusRow("task:2"), { key: "x" });
  fireEvent.keyDown(document.activeElement!, { key: "k", metaKey: true });
  const dialog = screen.getByRole("dialog", { name: "명령 팔레트" });
  expect(within(dialog).getByText("완료")).toBeTruthy();
  expect(within(dialog).getByText("다시 알림")).toBeTruthy();
  expect(within(dialog).getAllByText("우선순위").length).toBeGreaterThan(0);
  expect(within(dialog).getByText("보류")).toBeTruthy();
  for (const label of ["홈 화면으로 이동", "결정 화면으로 이동", "작업 화면으로 이동", "배포 화면으로 이동"]) {
    expect(within(dialog).getByText(label)).toBeTruthy();
  }
});

it("sends only the rows an action applies to", async () => {
  render(<Fixture rows={[row("task:1", { read: true }), row("task:2")]} />);
  fireEvent.keyDown(focusRow("task:1"), { key: "x" });
  fireEvent.keyDown(focusRow("task:2"), { key: "x" });
  const bar = screen.getByRole("region", { name: "선택 항목 작업" });
  fireEvent.click(within(bar).getByRole("button", { name: "안 읽음" }));
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1));
  expect(lastBody()).toMatchObject({ action: "unread", items: ["task:1"] });
});

it("runs a palette action for the rows it opened for, not the palette's focus", async () => {
  render(<Fixture rows={[row("task:1"), row("task:2")]} />);
  fireEvent.keyDown(focusRow("task:1"), { key: "x" });
  fireEvent.keyDown(focusRow("task:2"), { key: "x" });
  fireEvent.keyDown(document.activeElement!, { key: "k", metaKey: true });
  const dialog = screen.getByRole("dialog", { name: "명령 팔레트" });
  within(dialog).getByRole("textbox").focus();
  const done = within(dialog).getByText("완료").closest("button");
  if (!done) throw new Error("missing done entry");
  fireEvent.click(done);
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1));
  expect(lastBody()).toMatchObject({ action: "done", items: ["task:1", "task:2"] });
});

it("closes an open dialog on Escape", () => {
  render(<Fixture rows={[row("task:1")]} />);
  fireEvent.keyDown(focusRow("task:1"), { key: "h" });
  expect(screen.getByRole("dialog", { name: "다시 알림 시각" })).toBeTruthy();
  fireEvent.keyDown(document, { key: "Escape" });
  expect(screen.queryByRole("dialog", { name: "다시 알림 시각" })).toBeNull();
});

it("keeps the shortcuts inactive inside a text input", () => {
  render(<Fixture rows={[row("task:1")]} />);
  const input = screen.getByRole("textbox", { name: "메모" });
  input.focus();
  fireEvent.keyDown(input, { key: "x" });
  expect(screen.queryByRole("region", { name: "선택 항목 작업" })).toBeNull();
});

it("reports a refused write in Korean and offers no undo", async () => {
  fetcher.mockResolvedValueOnce(new Response(JSON.stringify({ error: "invalid_snooze" }), { status: 400 }));
  render(<Fixture rows={[row("task:1")]} />);
  fireEvent.keyDown(focusRow("task:1"), { key: "h" });
  fireEvent.click(within(screen.getByRole("dialog", { name: "다시 알림 시각" })).getByRole("button", { name: /1시간/ }));
  expect(await screen.findByText("다시 알림 시각이 올바르지 않습니다.")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "실행 취소 (z)" })).toBeNull();
});
