import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { BoardApp, operatorVisible, taskOrder } from "./BoardApp";
import type { BoardTask } from "./types";

const STATES = ["backlog", "claimed", "in_progress", "verifying", "join", "hold", "needs_decision", "merged", "dropped"];

function task(partial: Partial<BoardTask> & { id: number }): BoardTask {
  return {
    lane: "lane-a",
    title: `task-${partial.id}`,
    kind: "implement",
    state: "backlog",
    priority: 0,
    created_by: "test",
    created_at: `2026-09-01T00:00:0${partial.id}Z`,
    updated_at: "2026-09-01T00:00:00Z",
    refs: {},
    ...partial,
  };
}

function boardPayload(tasks: BoardTask[]) {
  return { generated_at: "2026-09-19T00:00:00Z", states: STATES, tasks, truncated: false };
}

const EMPTY_POLICY = { status: "not_configured", pointer_key: "policy/active", pointer_doc_url: "/ui/doc/policy/active", items: [] };

const urlOf = (input: RequestInfo | URL) => (typeof input === "string" ? input : input instanceof URL ? input.href : input.url);

function stubFetch(map: Record<string, unknown>) {
  const routes = Object.entries(map).sort((a, b) => b[0].length - a[0].length);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = urlOf(input);
      for (const [prefix, body] of routes) {
        if (url.startsWith(prefix)) {
          if (body instanceof Error) {
            return Promise.reject(body);
          }
          return new Response(JSON.stringify(body), { status: 200 });
        }
      }
      return new Response("not found", { status: 404 });
    }),
  );
}

function stubBoard(tasks: BoardTask[], extra: Record<string, unknown> = {}) {
  stubFetch({ "/ui/api/board/tasks": boardPayload(tasks), "/ui/api/policy/active": EMPTY_POLICY, ...extra });
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe("BoardApp", () => {
  it("renders canonical state columns in order and queue-ordered cards", async () => {
    stubBoard([
      task({ id: 1, title: "low prio", priority: 1 }),
      task({ id: 2, title: "high prio", priority: 9 }),
      task({ id: 3, title: "same prio older", priority: 9, created_at: "2026-08-01T00:00:00Z" }),
      task({ id: 4, title: "progress", state: "in_progress" }),
      task({ id: 5, title: "merged done", state: "merged" }),
    ]);
    render(<BoardApp />);
    await screen.findByText(/high prio/);

    const columns = document.querySelectorAll(".board-column");
    expect([...columns].map((column) => column.getAttribute("data-state"))).toEqual(STATES);

    const backlog = within(columns[0] as HTMLElement);
    const titles = backlog.getAllByRole("button").map((card) => card.textContent ?? "");
    expect(titles[0]).toContain("same prio older");
    expect(titles[1]).toContain("high prio");
    expect(titles[2]).toContain("low prio");

    expect(within(columns[2] as HTMLElement).getByRole("button", { name: /#4 progress/ })).toBeTruthy();
    expect(within(columns[7] as HTMLElement).getByRole("button", { name: /#5 merged done/ })).toBeTruthy();
  });

  it("orders tasks deterministically by priority then queue order", () => {
    const a = task({ id: 1, priority: 1, created_at: "2026-09-01T00:00:00Z" });
    const b = task({ id: 2, priority: 5, created_at: "2026-09-02T00:00:00Z" });
    const c = task({ id: 3, priority: 5, created_at: "2026-08-01T00:00:00Z" });
    expect([a, b, c].sort(taskOrder).map((x) => x.id)).toEqual([3, 2, 1]);
  });

  it("applies the operator-only preset like the retired queue view", () => {
    expect(operatorVisible(task({ id: 1, kind: "decide", state: "in_progress" }))).toBe(true);
    expect(operatorVisible(task({ id: 2, kind: "implement", state: "needs_decision" }))).toBe(true);
    expect(operatorVisible(task({ id: 3, kind: "implement", state: "in_progress" }))).toBe(false);
    expect(operatorVisible(task({ id: 4, kind: "decide", state: "merged" }))).toBe(false);
    expect(operatorVisible(task({ id: 5, kind: "implement", state: "backlog" }))).toBe(true);
  });

  it("filters by lane, state visibility, and search without losing data", async () => {
    stubBoard([
      task({ id: 1, title: "alpha", lane: "lane-a" }),
      task({ id: 2, title: "beta", lane: "lane-b" }),
      task({ id: 3, title: "gamma", lane: "lane-b", state: "merged" }),
    ]);
    render(<BoardApp />);
    await screen.findByText(/alpha/);

    fireEvent.change(screen.getByLabelText("Lane"), { target: { value: "lane-b" } });
    expect(screen.queryByText(/alpha/)).toBeNull();
    expect(screen.getByText(/beta/)).toBeTruthy();

    fireEvent.click(screen.getByLabelText("merged"));
    expect(screen.queryByText(/gamma/)).toBeNull();
    fireEvent.click(screen.getByLabelText("merged"));
    expect(screen.getByText(/gamma/)).toBeTruthy();

    fireEvent.change(screen.getByLabelText("검색"), { target: { value: "beta" } });
    expect(screen.queryByText(/gamma/)).toBeNull();
    expect(screen.getByText(/beta/)).toBeTruthy();
    fireEvent.click(screen.getByText("필터 초기화"));
    expect(screen.getByText(/alpha/)).toBeTruthy();
    expect(screen.getByText(/gamma/)).toBeTruthy();
  });

  it("operator preset collapses backlog and hides non-decide work", async () => {
    stubBoard([
      task({ id: 1, title: "queued", state: "backlog" }),
      task({ id: 2, title: "working", state: "in_progress" }),
      task({ id: 3, title: "needs answer", kind: "decide", state: "in_progress" }),
      task({ id: 4, title: "blocked", state: "needs_decision" }),
    ]);
    render(<BoardApp />);
    await screen.findByText(/working/);

    fireEvent.click(screen.getByLabelText("운영자 필요만"));
    expect(screen.queryByText(/working/)).toBeNull();
    expect(screen.queryByText(/queued/)).toBeNull();
    expect(screen.getByText(/needs answer/)).toBeTruthy();
    expect(screen.getByText(/blocked/)).toBeTruthy();
    expect(screen.getByText("접힘 (1)")).toBeTruthy();
  });

  it("loads task detail on card selection and keeps all participant segments", async () => {
    stubBoard([task({ id: 7, title: "selectable" })], {
      "/ui/api/board/tasks/7": {
        task: task({ id: 7, title: "selectable" }),
        events: [{ id: 1, from: "backlog", to: "claimed", by: "worker-a", note: "claimed it", at: "2026-09-01T00:00:00Z" }],
        dwell: [{ state: "backlog", seconds: 5, open: true }],
        linear: null,
        participants: { task_ref: "hk:task/7", coverage: "not_collected", segments: [] },
      },
    });
    render(<BoardApp />);
    const card = await screen.findByText(/selectable/);
    fireEvent.click(card);
    await screen.findByText("수집된 bench rep이 없습니다.");
    expect(screen.getByText("backlog → claimed")).toBeTruthy();
    expect(screen.getByText("hk:task/7")).toBeTruthy();
    fireEvent.click(screen.getByText("닫기"));
    expect(screen.queryByText("전이 기록")).toBeNull();
  });

  it("shows loading, empty, and error states explicitly", async () => {
    stubBoard([]);
    const view = render(<BoardApp />);
    expect(screen.getByText("불러오는 중")).toBeTruthy();
    await screen.findByText("표시할 태스크가 없습니다.");
    view.unmount();

    stubFetch({ "/ui/api/board/tasks": new Error("down"), "/ui/api/policy/active": EMPTY_POLICY });
    render(<BoardApp />);
    await screen.findByText("보드를 불러오지 못했습니다.");
  });

  it("keeps the last good board when a refresh fails", async () => {
    vi.useFakeTimers();
    let fail = false;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = urlOf(input);
        if (url.startsWith("/ui/api/board/tasks")) {
          if (fail) {
            return Promise.reject(new Error("down"));
          }
          return new Response(JSON.stringify(boardPayload([task({ id: 1, title: "survivor" })])), { status: 200 });
        }
        return new Response(JSON.stringify(EMPTY_POLICY), { status: 200 });
      }),
    );
    render(<BoardApp />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(screen.getByText(/survivor/)).toBeTruthy();
    fail = true;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(15_000);
    });
    expect(screen.getByText(/survivor/)).toBeTruthy();
    expect(screen.getByText(/갱신 실패/)).toBeTruthy();
  });

  it("renders hostile titles as text, never markup", async () => {
    stubBoard([task({ id: 1, title: '<img src=x onerror="alert(1)"><script>alert(2)</script>' })]);
    render(<BoardApp />);
    await screen.findByText(/onerror/);
    expect(document.querySelector(".board img")).toBeNull();
    expect(document.querySelector(".board script")).toBeNull();
  });
});
