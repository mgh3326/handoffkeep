// #618 decision requests in the queue: row badge + one-line count (initial
// bundle) and the drawer card (lazy chunk). Read only — A8: no buttons, no
// inputs, no pre-selected recommendation.

import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { QueueProtoApp } from "./QueueProtoApp";
import { boardTaskToProto } from "./boardtask";
import { decisionMark } from "./adapter";
import type { FetchDoc } from "./DocInline";
import type { BoardDetail, BoardTask, DecisionRequestView } from "../board/types";
import { KNOWN_STATES, type Dataset } from "./types";

const here = dirname(fileURLToPath(import.meta.url));

function mkTask(over: Partial<BoardTask>): BoardTask {
  return {
    id: 6180,
    lane: "b618",
    title: "decision task",
    kind: "implement",
    state: "in_progress",
    priority: 10,
    created_by: "director-1",
    created_at: "2026-09-21T10:00:00Z",
    updated_at: "2026-09-21T11:00:00Z",
    refs: {},
    ...over,
  };
}

function dataset(tasks: BoardTask[], completeness: Dataset["completeness"] = "complete"): Dataset {
  return {
    key: "live",
    label: "live",
    source: "live",
    generatedAt: "2026-09-24T12:00:00Z",
    completeness,
    completenessNote: "test",
    states: [...KNOWN_STATES],
    tasks: tasks.map(boardTaskToProto),
    enrichment: {},
  };
}

function request(over: Partial<DecisionRequestView>): DecisionRequestView {
  return {
    id: "dr-6180-1",
    revision: 1,
    current: true,
    state: "open",
    state_label: "열림 · 응답 대기",
    question: "allowlist 를 어디에 구현할까?",
    options: [
      { key: "A", label: "#481 writer 명세에 통합", recommended: true },
      { key: "B", label: "백필 도구 레포" },
      { key: "C", label: "hk 투영 함수" },
    ],
    allow_free: true,
    reason: "writer 한 곳이 가장 싸다",
    default_action: "보류하고 다음 태스크로 이동",
    requested_by: "director-1",
    requested_at: "2026-09-24T01:00:00Z",
    ...over,
  };
}

function detailOf(task: BoardTask, over: Partial<BoardDetail>): BoardDetail {
  return {
    task,
    events: [],
    dwell: [],
    linear: null,
    participants: { task_ref: `hk:task/${task.id}`, coverage: "not_collected", segments: [] },
    decision_requests: [],
    ...over,
  };
}

const noDoc: FetchDoc = vi.fn<FetchDoc>(() => Promise.reject(new Error("no doc")));

function openDrawer(task: BoardTask, detail: (id: number) => Promise<BoardDetail>) {
  const view = render(<QueueProtoApp datasets={{ live: dataset([task]) }} initialSet="live" fetchDetail={detail} fetchDoc={noDoc} />);
  fireEvent.click(screen.getByRole("link", { name: "All" }));
  fireEvent.keyDown(view.container.querySelector<HTMLElement>(`[data-task-id="${task.id}"]`)!, { key: "Enter" });
  const drawer = screen.getByRole("dialog");
  return { ...view, slot: () => drawer.querySelector<HTMLElement>(".qp-decision")! };
}

describe("queue row badge and one-line count", () => {
  beforeEach(() => localStorage.clear());

  it("badges only open requests; the count line splits pending and uncleaned", () => {
    const tasks = [
      mkTask({ id: 1, state: "backlog", refs: { decision_request: { id: "dr-1-1", revision: 1, status: "open" } } }),
      mkTask({ id: 2, state: "in_progress", refs: { decision_request: { id: "dr-2-1", revision: 1, status: "open" } } }),
      mkTask({ id: 3, state: "merged", refs: { decision_request: { id: "dr-3-1", revision: 1, status: "open" } } }),
      mkTask({ id: 4, state: "in_progress", refs: { decision_request: { id: "dr-4-1", revision: 1, status: "answered" } } }),
      mkTask({ id: 5, state: "needs_decision", refs: {} }),
    ];
    const view = render(<QueueProtoApp datasets={{ live: dataset(tasks) }} initialSet="live" />);
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    const status = view.container.querySelector<HTMLElement>('[data-status="decisions"]')!;
    expect(status.textContent).toBe("내 결정 대기 2건 · 미정리 요청 1건");
    const badges = [...view.container.querySelectorAll<HTMLElement>("[data-decision]")];
    expect(badges.map((b) => [b.closest("[data-task-id]")?.getAttribute("data-task-id"), b.dataset.decision, b.textContent])).toEqual(
      expect.arrayContaining([
        ["1", "pending", "내 결정 대기"],
        ["2", "pending", "내 결정 대기"],
        ["3", "uncleaned", "미정리 요청"],
      ]),
    );
    expect(badges).toHaveLength(3);
    // The row badge is a marker, never an answer control.
    expect(view.container.querySelectorAll("[data-task-id] input, [data-task-id] [data-decision] button")).toHaveLength(0);
  });

  it("a partial read is never a firm count, and zero is only shown for a complete read", () => {
    const tasks = [mkTask({ id: 1, refs: { decision_request: { id: "dr-1-1", revision: 1, status: "open" } } })];
    const partial = render(<QueueProtoApp datasets={{ live: dataset(tasks, "partial") }} initialSet="live" />);
    expect(partial.container.querySelector('[data-status="decisions"]')?.textContent).toBe("내 결정 대기 1+건");
    partial.unmount();
    const none = render(<QueueProtoApp datasets={{ live: dataset([mkTask({ id: 9 })]) }} initialSet="live" />);
    expect(none.container.querySelector('[data-status="decisions"]')?.textContent).toBe("내 결정 대기 0건");
  });

  it("uses the same split as the server (shared fixture with internal/ui)", () => {
    const fixture = JSON.parse(readFileSync(resolve(here, "..", "..", "..", "..", "internal", "ui", "testdata", "decision_marks.json"), "utf8")) as {
      tasks: { id: number; state: string; refs: BoardTask["refs"] }[];
      want: { pending: number; uncleaned: number };
    };
    const counts = { pending: 0, uncleaned: 0 };
    for (const row of fixture.tasks) {
      const mark = decisionMark(boardTaskToProto(mkTask(row)));
      if (mark !== null) counts[mark] += 1;
    }
    expect(counts).toEqual(fixture.want);
  });
});

describe("drawer decision card (lazy chunk)", () => {
  beforeEach(() => localStorage.clear());

  it("recommend A, silence holds: recommendation, default, trigger and deadline are separate; nothing is pre-selected", async () => {
    const task = mkTask({ refs: { decision_request: { id: "dr-6180-1", revision: 1, status: "open" } } });
    const { slot } = openDrawer(task, () => Promise.resolve(detailOf(task, { decision_requests: [request({})] })));
    await waitFor(() => expect(slot().querySelector('[data-decision-request="dr-6180-1"]')).not.toBeNull());
    const text = slot().textContent!;
    expect(text).toContain("열림 · 응답 대기");
    expect(text).toContain("권고: A — writer 한 곳이 가장 싸다");
    expect(text).toContain("무응답 시: 보류하고 다음 태스크로 이동");
    expect(text).not.toContain("무응답 시: A");
    expect(text).toContain("발동 시점 미기록");
    expect(text).toContain("기한 미지정");
    expect(slot().querySelectorAll("input, button, select, textarea")).toHaveLength(0);
    expect(text).not.toContain("선택됨");
  });

  it("deadline passed without a receipt reads as overdue, never as applied", async () => {
    const task = mkTask({});
    const overdue = request({ state: "overdue", state_label: "기한 경과 · 기본값 미적용", due_at: "2026-09-24T09:00:00Z", default_option: "B" });
    const { slot } = openDrawer(task, () => Promise.resolve(detailOf(task, { decision_requests: [overdue] })));
    await waitFor(() => expect(slot().textContent).toContain("기한 경과 · 기본값 미적용"));
    expect(slot().textContent).toContain("적용되지 않았습니다");
    expect(slot().textContent).toContain("(선택지 B)");
    expect(slot().textContent).not.toContain("기본값 적용됨");
  });

  it("a superseded request stays in history with its own options; its answer never shows on the new one", async () => {
    const task = mkTask({});
    const current = request({ id: "dr-6180-2", revision: 2, supersedes: "dr-6180-1", question: "바뀐 질문", options: [{ key: "A", label: "새 A" }, { key: "B", label: "새 B", recommended: true }] });
    const old = request({ current: false, state: "superseded", state_label: "대체됨", superseded_by: "dr-6180-2" });
    const { slot } = openDrawer(task, () => Promise.resolve(detailOf(task, { decision_requests: [current, old] })));
    await waitFor(() => expect(slot().querySelector('[data-decision-request="dr-6180-2"]')).not.toBeNull());
    const now = slot().querySelector<HTMLElement>('[data-decision-request="dr-6180-2"]')!;
    expect(now.textContent).toContain("바뀐 질문");
    expect(now.textContent).not.toContain("#481 writer");
    expect(now.querySelector("[data-decision-resolution]")).toBeNull();
    const history = slot().querySelector<HTMLDetailsElement>(".qp-decision-history")!;
    expect(history.open).toBe(false);
    expect(history.textContent).toContain("결정 이력 1건");
    const earlier = history.querySelector<HTMLElement>('[data-decision-request="dr-6180-1"]')!;
    expect(earlier.dataset.state).toBe("superseded");
    expect(earlier.textContent).toContain("dr-6180-2 로 대체됨");
    expect(earlier.textContent).toContain("#481 writer");
  });

  it("history after close: answered with who and when; an uncleaned request asks for cleanup", async () => {
    const task = mkTask({ state: "merged" });
    const answered = request({
      state: "answered",
      state_label: "답변됨",
      resolution: { kind: "answered", option: "A", responder: "operator", by: "director-1", at: "2026-09-24T02:00:00Z" },
    });
    const first = openDrawer(task, () => Promise.resolve(detailOf(task, { decision_requests: [answered] })));
    await waitFor(() => expect(first.slot().querySelector("[data-decision-resolution]")).not.toBeNull());
    expect(first.slot().querySelector("[data-decision-resolution]")!.textContent).toContain("답변 A");
    expect(first.slot().querySelector("[data-decision-resolution]")!.textContent).toContain("operator (기록 director-1)");
    expect(first.slot().textContent).toContain("A #481 writer 명세에 통합 권고 ← 선택됨");
    first.unmount();

    const uncleaned = request({ state: "uncleaned", state_label: "미정리 요청 · 종료된 태스크에 열려 있음" });
    const second = openDrawer(task, () => Promise.resolve(detailOf(task, { decision_requests: [uncleaned] })));
    await waitFor(() => expect(second.slot().textContent).toContain("미정리 요청"));
    expect(second.slot().textContent).toContain("정리가 필요합니다");
  });

  it("a detail failure is not 'no request'; an older server is 'not provided'; needs_decision without a record is 미기록", async () => {
    const task = mkTask({ state: "needs_decision" });
    const failed = openDrawer(task, () => Promise.reject(new Error("boom")));
    await waitFor(() => expect(failed.slot().querySelector("[data-decision-error]")).not.toBeNull());
    expect(failed.slot().textContent).toContain('"요청 없음"이 아닙니다');
    expect(failed.slot().textContent).not.toContain("기록된 결정 요청 없음");
    failed.unmount();

    const old = openDrawer(task, () => {
      const d = detailOf(task, {});
      delete d.decision_requests;
      return Promise.resolve(d);
    });
    await waitFor(() => expect(old.slot().textContent).toContain("결정 요청 미연결"));
    old.unmount();

    const legacy = openDrawer(task, () =>
      Promise.resolve(
        detailOf(task, {
          decision_legacy: { state: "unrecorded", state_label: "미기록", question: "기록 없는 질문", options: [{ key: "A", label: "예전 A" }], allow_free: true },
        }),
      ),
    );
    await waitFor(() => expect(legacy.slot().textContent).toContain("미기록"));
    expect(legacy.slot().textContent).toContain("기록 없는 질문");
    expect(legacy.slot().textContent).toContain("권고·무응답 동작·기한은 알 수 없습니다");
    legacy.unmount();

    const none = openDrawer(mkTask({ state: "in_progress" }), () => Promise.resolve(detailOf(mkTask({}), {})));
    await waitFor(() => expect(none.slot().textContent).toContain("기록된 결정 요청 없음"));
  });
});
