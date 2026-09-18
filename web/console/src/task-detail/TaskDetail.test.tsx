import { fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { TaskDetail } from "./TaskDetail";
import type { BoardDetail } from "../board/types";

function detail(partial: Partial<BoardDetail> = {}): BoardDetail {
  return {
    task: {
      id: 42,
      lane: "lane-a",
      title: "detail me",
      kind: "implement",
      state: "in_progress",
      priority: 3,
      created_by: "test",
      created_at: "2026-09-01T00:00:00Z",
      updated_at: "2026-09-02T00:00:00Z",
      refs: { pr: "https://github.com/x/y/pull/7", head_sha: "abcdef1234567890", report_path: "report/42.md", job_id: "job-9" },
    },
    events: [
      { id: 1, from: "backlog", to: "claimed", by: "worker-a", at: "2026-09-01T01:00:00Z" },
      { id: 2, from: "claimed", to: "in_progress", by: "worker-a", note: "started", at: "2026-09-01T02:00:00Z" },
    ],
    dwell: [
      { state: "backlog", seconds: 3600, open: false },
      { state: "claimed", seconds: 3600, open: false },
      { state: "in_progress", seconds: 120, open: true },
    ],
    linear: { issue_id: "issue-1", identifier: "ROB-42" },
    participants: {
      task_ref: "hk:task/42",
      coverage: "collected",
      segments: [
        { role: "impl", model_id: "model-a", reps: 2, rounds: 2, blockers_found: 0, completed: 2, input_tokens: 2000, output_tokens: 400 },
        { role: "verify", model_id: "model-b", reps: 1, rounds: 1, blockers_found: 1, completed: 1, input_tokens: 500, output_tokens: 100 },
        { role: null, model_id: null, reps: 1, rounds: null, blockers_found: null, completed: null, input_tokens: null, output_tokens: null },
      ],
    },
    ...partial,
  };
}

function stubDetail(body: unknown) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(JSON.stringify(body), { status: 200 })),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("TaskDetail", () => {
  it("renders refs, transitions, dwell, and linear identity", async () => {
    stubDetail(detail());
    render(<TaskDetail id={42} onClose={() => undefined} />);
    await screen.findByText("#42 detail me");

    const pr = screen.getByText("PR https://github.com/x/y/pull/7");
    expect(pr.tagName).toBe("A");
    expect(pr.getAttribute("href")).toBe("https://github.com/x/y/pull/7");
    const report = screen.getByText("report report/42.md");
    expect(report.getAttribute("href")).toBe("/ui/doc/report/42.md");
    expect(screen.getByText("head abcdef123")).toBeTruthy();
    expect(screen.getByText("linear ROB-42")).toBeTruthy();

    const events = document.querySelectorAll(".detail-events li");
    expect(events).toHaveLength(2);
    expect(events[0].textContent).toContain("backlog → claimed");
    expect(events[1].textContent).toContain("claimed → in_progress");
    expect(events[1].textContent).toContain("started");

    expect(screen.getByText("in_progress: 120s (진행 중)")).toBeTruthy();
  });

  it("renders one segment per participant model without collapsing", async () => {
    stubDetail(detail());
    render(<TaskDetail id={42} onClose={() => undefined} />);
    await screen.findByText("hk:task/42");
    const table = document.querySelector(".detail-participants table") as HTMLElement;
    const rows = within(table).getAllByRole("row");
    expect(rows).toHaveLength(4);
    expect(rows[1].textContent).toContain("model-a");
    expect(rows[2].textContent).toContain("model-b");
    expect(rows[3].textContent).toContain("unknown");
    expect(rows[3].textContent).toContain("미수집");
  });

  it("marks missing telemetry coverage explicitly", async () => {
    stubDetail(
      detail({
        linear: null,
        participants: { task_ref: "hk:task/42", coverage: "not_collected", segments: [] },
      }),
    );
    render(<TaskDetail id={42} onClose={() => undefined} />);
    await screen.findByText("수집된 bench rep이 없습니다.");
    expect(screen.getByText("not_collected")).toBeTruthy();
  });

  it("shows the error state and closes", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response("down", { status: 500 })),
    );
    const onClose = vi.fn();
    render(<TaskDetail id={42} onClose={onClose} />);
    await screen.findByText("상세 정보를 불러오지 못했습니다.");
    fireEvent.click(screen.getByText("닫기"));
    expect(onClose).toHaveBeenCalledOnce();
  });

  it("never renders an href for a non-github PR ref", async () => {
    const hostile = detail();
    hostile.task = { ...hostile.task, refs: { pr: "javascript:alert(1)" } };
    stubDetail(hostile);
    render(<TaskDetail id={42} onClose={() => undefined} />);
    await screen.findByText("#42 detail me");
    const pr = screen.getByText("PR javascript:alert(1)");
    expect(pr.tagName).not.toBe("A");
  });
});
