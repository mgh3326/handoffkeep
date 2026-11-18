import { render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { WaitsPage } from "./WaitsPage";
import type { WaitsResponse } from "./types";

const waitsPayload: WaitsResponse = {
  as_of: "2026-11-18T03:00:00Z",
  sections: [
    {
      key: "needs_operator",
      count: 2,
      truncated: false,
      rows: [
        {
          key: "decision:dr-5128-1",
          section: "needs_operator",
          source: "derived:decision_request",
          task_id: 5128,
          title: "Design console waits page",
          project: "handoffkeep",
          lane: "handoffkeep-design",
          state: "developing",
          on_kind: "operator",
          on_ref: "dr-5128-1",
          reason: "Which storage option should the waits page read?",
          since: "2026-11-15T03:00:00Z",
          age_seconds: 273600,
          next_action: "answer_decision",
          grounds_link: "/ui/tasks/5128",
          request_id: "dr-5128-1",
        },
        {
          key: "decision:dr-5129-1",
          section: "needs_operator",
          source: "derived:decision_request",
          task_id: 5129,
          title: "Confirm the rollout order",
          lane: "handoffkeep-design",
          state: "reviewing",
          on_kind: "operator",
          on_ref: "dr-5129-1",
          reason: "Roll out before or after the migration?",
          since: "2026-11-16T00:00:00Z",
          age_seconds: 97200,
          next_action: "confirm_answer",
          answer: {
            result: "pending_confirmation",
            words: "After the migration, in one release",
            actor: "robin",
            at: "2026-11-17T08:00:00Z",
          },
        },
      ],
    },
    { key: "failed_start", count: 0, truncated: false, rows: [] },
    {
      key: "waiting_on",
      count: 5,
      truncated: true,
      rows: [
        {
          key: "blocks:4265:5130",
          section: "waiting_on",
          source: "derived:blocks",
          task_id: 5130,
          title: "Wait for the blocking task",
          lane: "handoffkeep-core",
          state: "developing",
          on_kind: "task",
          on_ref: "#4265",
          reason: "waits for the blocking task",
          since: "2026-11-17T00:00:00Z",
          age_seconds: 10800,
          next_action: "wait_task",
        },
      ],
    },
    {
      key: "idle_started",
      count: 1,
      truncated: false,
      rows: [
        {
          key: "drift:5131",
          section: "idle_started",
          source: "derived:drift_idle_started",
          task_id: 5131,
          title: "Quiet developing task",
          lane: "panewire",
          state: "developing",
          reason: "no activity for a day",
          since: "2026-11-16T12:00:00Z",
          age_seconds: 57600,
          next_action: "check_progress",
        },
      ],
    },
  ],
  backlog: {
    count: 40,
    truncated: false,
    rows: [
      {
        task_id: 4270,
        title: "Ordinary backlog item",
        lane: "handoffkeep",
        created_at: "2026-11-12T01:00:00Z",
      },
    ],
  },
};

function serve(body: unknown, status = 200) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(JSON.stringify(body), { status })),
  );
}

function visit(search = "") {
  window.history.pushState(null, "", `/ui/waits${search}`);
  return render(<WaitsPage />);
}

describe("WaitsPage", () => {
  beforeEach(() => {
    serve(waitsPayload);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    window.history.pushState(null, "", "/");
  });

  it("renders the four sections in order with Korean headings, counts and the read time", async () => {
    visit();
    const headings = (await screen.findAllByRole("heading", { level: 2 })).map((h) => h.textContent);
    expect(headings).toEqual([
      "운영자의 답변을 기다리는 것",
      "시작에 실패해 멈춘 것",
      "다른 작업·레인·PR·배포를 기다리는 것",
      "진행 중이지만 활동이 없는 것",
      "일반 백로그",
    ]);

    const needs = screen.getByRole("region", { name: "운영자의 답변을 기다리는 것" });
    expect(within(needs).getByText("2건")).toBeTruthy();
    const failed = screen.getByRole("region", { name: "시작에 실패해 멈춘 것" });
    expect(within(failed).getByText("0건")).toBeTruthy();
    expect(within(failed).getByText("없음")).toBeTruthy();
    const waiting = screen.getByRole("region", { name: "다른 작업·레인·PR·배포를 기다리는 것" });
    expect(within(waiting).getByText("5건")).toBeTruthy();
    expect(within(waiting).getByText(/목록이 잘렸습니다/)).toBeTruthy();

    expect(screen.getByText("기준 시각")).toBeTruthy();
    expect(screen.getByText("2026-11-18 03:00 UTC").tagName).toBe("TIME");
    expect(within(needs).getByText("3일 4시간")).toBeTruthy();
  });

  it("shows an answer row's words and actor with the confirmation label, and no button", async () => {
    visit();
    const needs = await screen.findByRole("region", { name: "운영자의 답변을 기다리는 것" });
    expect(within(needs).getByText("답변 기록됨 · 확인 대기")).toBeTruthy();
    expect(within(needs).getByText("답변: After the migration, in one release")).toBeTruthy();
    expect(within(needs).getByText(/기록한 사람 robin/)).toBeTruthy();
    expect(within(needs).getByText("운영자 답변 필요")).toBeTruthy();
    // The page is read-only: nothing on it is a button, answer or otherwise.
    expect(screen.queryAllByRole("button")).toEqual([]);
  });

  it("lists the ordinary backlog apart from the sections", async () => {
    visit();
    const backlog = await screen.findByRole("region", { name: "일반 백로그" });
    expect(within(backlog).getByText("40건")).toBeTruthy();
    expect(within(backlog).getByText("Ordinary backlog item")).toBeTruthy();
    expect(within(backlog).getByRole("link", { name: /Ordinary backlog item/ }).getAttribute("href")).toBe("/ui/tasks/4270");
    for (const name of ["운영자의 답변을 기다리는 것", "다른 작업·레인·PR·배포를 기다리는 것"]) {
      expect(within(screen.getByRole("region", { name })).queryByText("Ordinary backlog item")).toBeNull();
    }
  });

  it("?section= shows only that section's rows and keeps the count line for all", async () => {
    visit("?section=waiting_on");
    const waiting = await screen.findByRole("region", { name: "다른 작업·레인·PR·배포를 기다리는 것" });
    expect(within(waiting).getByText("Wait for the blocking task")).toBeTruthy();
    expect(screen.queryByText("Design console waits page")).toBeNull();
    expect(screen.queryByRole("region", { name: "운영자의 답변을 기다리는 것" })).toBeNull();
    expect(screen.queryByRole("region", { name: "일반 백로그" })).toBeNull();
    const counts = screen.getByLabelText("구획별 건수");
    expect(counts.textContent).toContain("운영자 답변 2건");
    expect(counts.textContent).toContain("대기 5건");
  });

  it("?item= highlights the matching row, and says so when no row matches", async () => {
    visit("?item=decision:dr-5128-1");
    const title = await screen.findByText("Design console waits page");
    const row = title.closest("li");
    expect(row?.getAttribute("data-focus")).toBe("true");
    expect(row?.id).toBe("wait-decision:dr-5128-1");
    expect(screen.getByText("Confirm the rollout order").closest("li")?.getAttribute("data-focus")).toBeNull();
  });

  it("?item= that matches no row says it cannot be found", async () => {
    visit("?item=decision:missing");
    expect(await screen.findByText(/찾을 수 없습니다/)).toBeTruthy();
    expect(screen.getByText("Design console waits page").closest("li")?.getAttribute("data-focus")).toBeNull();
  });

  it("a malformed payload shows the error path and renders no section", async () => {
    serve({ ...waitsPayload, sections: undefined });
    visit();
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.getByText(/데이터를 불러오지 못했습니다/)).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "운영자의 답변을 기다리는 것" })).toBeNull();
    expect(screen.queryByText("없음")).toBeNull();
  });

  it("a payload with an unknown section key is malformed, not rendered as empty", async () => {
    serve({
      ...waitsPayload,
      sections: waitsPayload.sections.map((s, i) => (i === 1 ? { ...s, key: "mystery" } : s)),
    });
    visit();
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.queryByRole("region", { name: "운영자의 답변을 기다리는 것" })).toBeNull();
  });
});
