import { fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { CommandBar } from "./CommandBar";
import type { ActionTarget } from "../actions";

const open = (id: number, title: string): ActionTarget => ({ id, state: "developing", priority: 2, lane: "console-demo", title });
const closed = (id: number, title: string): ActionTarget => ({ id, state: "completed", priority: 0, lane: "console-demo", title });
const lanes = ["console-demo", "fleet-status"];

let fetcher: ReturnType<typeof vi.fn>;

beforeEach(() => {
  const meta = document.createElement("meta");
  meta.name = "hk-csrf";
  meta.content = "token";
  document.head.append(meta);
  fetcher = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const path = String(input);
    const method = init?.method ?? "GET";
    if (method === "POST" && path.endsWith("/priority")) {
      if (path.includes("/102/")) return new Response(JSON.stringify({ error: "task_conflict" }), { status: 409 });
      return new Response(JSON.stringify({ previous: 2, priority: 1 }));
    }
    if (method === "POST" && path.endsWith("/relane")) {
      return new Response(JSON.stringify({ previous: "console-demo", lane: "fleet-status", changed: true, relay: "sent" }));
    }
    if (method === "POST" && path.endsWith("/comments")) {
      return new Response(JSON.stringify({ id: 1, task_id: 101, body: "확인했습니다", author: "operator:test", created_at: "2026-11-07T00:00:00Z" }), { status: 201 });
    }
    return new Response("{}");
  });
  vi.stubGlobal("fetch", fetcher);
});

afterEach(() => {
  vi.unstubAllGlobals();
  document.querySelector('meta[name="hk-csrf"]')?.remove();
});

function body(call: number): Record<string, unknown> {
  const raw = (fetcher.mock.calls[call][1] as RequestInit | undefined)?.body;
  return typeof raw === "string" && raw.startsWith("{") ? JSON.parse(raw) : {};
}

function keyedCalls(): string[] {
  return fetcher.mock.calls
    .filter(call => /\/(priority|relane)$/.test(String(call[0])))
    .map(call => String(body(fetcher.mock.calls.indexOf(call)).event_key ?? ""));
}

it("previews old to new per task and shows the store's refusals before sending", () => {
  render(<CommandBar targets={[open(101, "열린 작업"), closed(102, "닫힌 작업")]} lanes={lanes} onClose={() => {}} onApplied={() => {}} />);
  fireEvent.change(screen.getByLabelText("명령 입력"), { target: { value: "상태 머지됨 근거 https://example.test/pr/1 우선순위 1" } });

  const first = screen.getByRole("region", { name: "#101 열린 작업" });
  expect(within(first).getByText("개발 중 → 머지됨")).toBeTruthy();
  expect(within(first).getByText("P2 → P1")).toBeTruthy();

  const second = screen.getByRole("region", { name: "#102 닫힌 작업" });
  expect(within(second).getAllByText("종결된 작업은 변경할 수 없습니다").length).toBe(2);
  expect(screen.getByText("실행 2건 · 거부 2건")).toBeTruthy();
  expect((screen.getByRole("button", { name: "적용 (Enter)" }) as HTMLButtonElement).disabled).toBe(false);
  expect(fetcher).not.toHaveBeenCalled();
});

it("sends one request per runnable change with a distinct key, summarizes a partial failure and undoes exact inverses", async () => {
  render(<CommandBar targets={[open(101, "작업 하나"), open(102, "작업 둘")]} lanes={lanes} onClose={() => {}} onApplied={() => {}} />);
  fireEvent.change(screen.getByLabelText("명령 입력"), { target: { value: "우선순위 1 레인 fleet-status 코멘트 확인했습니다" } });
  fireEvent.click(screen.getByRole("button", { name: "적용 (Enter)" }));

  expect(await screen.findByText("명령 적용: 5건 적용 · 1건 실패")).toBeTruthy();
  expect(String(fetcher.mock.calls[0][0])).toBe("/ui/api/tasks/101/priority");
  expect(String(fetcher.mock.calls[1][0])).toBe("/ui/api/tasks/101/relane");
  expect(String(fetcher.mock.calls[2][0])).toBe("/ui/tasks/101/comments");
  expect(String(fetcher.mock.calls[3][0])).toBe("/ui/api/tasks/102/priority");
  expect(String(fetcher.mock.calls[4][0])).toBe("/ui/api/tasks/102/relane");
  expect(String(fetcher.mock.calls[5][0])).toBe("/ui/tasks/102/comments");
  const keys = keyedCalls();
  expect(keys).toHaveLength(4);
  expect(new Set(keys).size).toBe(4);
  expect(body(0)).toMatchObject({ priority: 1 });
  expect(body(1)).toMatchObject({ lane: "fleet-status" });
  expect(String((fetcher.mock.calls[2][1] as RequestInit).body)).toContain("body=%ED%99%95%EC%9D%B8%ED%96%88%EC%8A%B5%EB%8B%88%EB%8B%A4");
  expect(screen.getByText("#102 작업 둘 · 우선순위 — 작업 상태가 바뀌어 실행할 수 없습니다. 새로고침한 뒤 다시 시도하세요.")).toBeTruthy();

  fireEvent.click(screen.getByRole("button", { name: "실행 취소" }));
  expect(await screen.findByText("3건을 실행 취소했습니다.")).toBeTruthy();
  const undoPriority = fetcher.mock.calls.findIndex(call => String(call[0]) === "/ui/api/tasks/101/priority" && body(fetcher.mock.calls.indexOf(call)).priority === 2);
  expect(undoPriority).toBeGreaterThan(5);
  const undoRelanes = fetcher.mock.calls.filter(call => String(call[0]).endsWith("/relane") && body(fetcher.mock.calls.indexOf(call)).from_key !== "");
  expect(undoRelanes).toHaveLength(2);
  expect(undoRelanes.map(call => body(fetcher.mock.calls.indexOf(call)).from_key)).toEqual([keys[3], keys[1]]);
});

it("moves through autocomplete with the arrow keys, completes with Tab and closes on Escape", () => {
  const close = vi.fn();
  render(<CommandBar targets={[open(101, "작업")]} lanes={lanes} onClose={close} onApplied={() => {}} />);
  const input = screen.getByLabelText("명령 입력") as HTMLInputElement;
  fireEvent.change(input, { target: { value: "우선" } });
  expect(screen.getByRole("listbox", { name: "명령 자동완성" })).toBeTruthy();
  fireEvent.keyDown(input, { key: "ArrowDown" });
  fireEvent.keyDown(input, { key: "Tab" });
  expect(input.value).toBe("우선순위 ");
  fireEvent.change(input, { target: { value: "우선순위 P2" } });
  expect(screen.getByText("P2 → P2")).toBeTruthy();
  fireEvent.keyDown(input, { key: "Escape" });
  expect(screen.queryByRole("listbox", { name: "명령 자동완성" })).toBeNull();
  fireEvent.keyDown(input, { key: "Escape" });
  expect(close).toHaveBeenCalledOnce();
});

it("reports a refused lane and a missing drop reason without sending", () => {
  render(<CommandBar targets={[open(101, "작업")]} lanes={lanes} onClose={() => {}} onApplied={() => {}} />);
  fireEvent.change(screen.getByLabelText("명령 입력"), { target: { value: "레인 nowhere" } });
  expect(screen.getByText("알 수 없는 레인입니다.")).toBeTruthy();
  expect((screen.getByRole("button", { name: "적용 (Enter)" }) as HTMLButtonElement).disabled).toBe(true);
  fireEvent.change(screen.getByLabelText("명령 입력"), { target: { value: "드롭 취소" } });
  expect(screen.getByText("드롭 이유가 필요합니다.")).toBeTruthy();
  // The reason runs to the end of the line, so the preview shows all of it.
  fireEvent.change(screen.getByLabelText("명령 입력"), { target: { value: "드롭 취소 이유 필요 없음 우선순위 1" } });
  expect(screen.getByText("개발 중 → 종료됨 (취소) · 이유: 필요 없음 우선순위 1")).toBeTruthy();
  expect(fetcher).not.toHaveBeenCalled();
});
