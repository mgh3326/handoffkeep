import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueueProtoApp } from "./QueueProtoApp";
import { TaskPage } from "./TaskPage";
import { boardTaskToProto } from "./boardtask";
import type { FetchDoc } from "./DocInline";
import type { BoardDetail, BoardDoc, BoardTask } from "../board/types";
import { KNOWN_STATES, type Dataset } from "./types";

function mkBoardTask(over: Partial<BoardTask>): BoardTask {
  return {
    id: 8001,
    lane: "body-lane",
    title: "one operator line",
    kind: "implement",
    state: "claimed",
    priority: 10,
    created_by: "director",
    created_at: "2026-09-21T10:00:00Z",
    updated_at: "2026-09-21T11:00:00Z",
    refs: {},
    ...over,
  };
}

function mkDetail(task: BoardTask, over: Partial<BoardDetail> = {}): BoardDetail {
  return {
    task,
    events: [
      { id: 1, from: "backlog", to: "claimed", by: "builder-a", note: "**not bold** <b>not html</b>", at: "2026-09-21T10:30:00Z" },
    ],
    dwell: [],
    linear: null,
    participants: { task_ref: `hk:task/${task.id}`, coverage: "not_collected", segments: [] },
    ...over,
  };
}

function dataset(tasks: BoardTask[]): Dataset {
  return {
    key: "live",
    label: "live",
    source: "live",
    generatedAt: "2026-09-21T12:00:00Z",
    completeness: "complete",
    completenessNote: "test",
    states: [...KNOWN_STATES],
    tasks: tasks.map(boardTaskToProto),
    enrichment: {},
  };
}

function doc(body: string): BoardDoc {
  return { key: "design/body", kind: "brief", sha256: "b".repeat(64), updated_at: "2026-09-21T09:00:00Z", bytes: body.length, format: "markdown", body };
}

function openPanel(tasks: BoardTask[], fetchDoc: FetchDoc, detail?: (id: number) => Promise<BoardDetail>) {
  const view = render(<QueueProtoApp datasets={{ live: dataset(tasks) }} initialSet="live" fetchDetail={detail} fetchDoc={fetchDoc} />);
  fireEvent.click(screen.getByRole("link", { name: "All" }));
  fireEvent.keyDown(view.container.querySelector<HTMLElement>(`[data-task-id="${tasks[0].id}"]`)!, { key: "Enter" });
  return { ...view, drawer: screen.getByRole("dialog") as HTMLElement };
}

function panel(root: HTMLElement, tab: string): HTMLElement {
  return root.querySelector<HTMLElement>(`[role=tabpanel][data-tab="${tab}"]`)!;
}

describe("detail tabs — 개요 / 코멘트 / 전이", () => {
  beforeEach(() => {
    localStorage.clear();
    window.history.replaceState(null, "", "/ui/queue");
  });

  it("exactly three tabs, overview selected first; manual activation", () => {
    const { drawer } = openPanel([mkBoardTask({})], vi.fn<FetchDoc>());
    const tabs = within(drawer).getAllByRole("tab");
    expect(tabs.map((t) => t.textContent)).toEqual(["개요", "코멘트", "전이"]);
    expect(tabs.map((t) => t.getAttribute("aria-selected"))).toEqual(["true", "false", "false"]);
    expect(panel(drawer, "overview").hidden).toBe(false);
    expect(panel(drawer, "comments").hidden).toBe(true);
    // Arrow moves focus only; the panel changes on Enter (network-safe manual activation).
    tabs[0].focus();
    fireEvent.keyDown(tabs[0], { key: "ArrowRight" });
    expect(document.activeElement).toBe(tabs[1]);
    expect(tabs[1].getAttribute("aria-selected")).toBe("false");
    fireEvent.keyDown(tabs[1], { key: "Enter" });
    expect(tabs[1].getAttribute("aria-selected")).toBe("true");
    expect(panel(drawer, "comments").hidden).toBe(false);
    expect(panel(drawer, "overview").hidden).toBe(true);
    fireEvent.keyDown(tabs[1], { key: "End" });
    expect(document.activeElement).toBe(tabs[2]);
    fireEvent.keyDown(tabs[2], { key: "ArrowRight" });
    expect(document.activeElement).toBe(tabs[0]);
    // The drawer stays open and on the same task while the tab list handles keys.
    expect(drawer.getAttribute("aria-label")).toContain("task 8001");
  });

  it("without a comments client (synthetic preview) the tab says not connected — never 'no comments'", () => {
    const { drawer } = openPanel([mkBoardTask({})], vi.fn<FetchDoc>());
    fireEvent.click(within(drawer).getByRole("tab", { name: "코멘트" }));
    const comments = panel(drawer, "comments");
    expect(comments.textContent).toContain("코멘트가 연결되지 않은 화면입니다");
    expect(comments.textContent).not.toContain("코멘트 없음");
    expect(comments.textContent).not.toMatch(/\d/);
  });

  it("the transitions tab lists task_events with notes as plain text", async () => {
    const task = mkBoardTask({});
    const { drawer } = openPanel([task], vi.fn<FetchDoc>(), () => Promise.resolve(mkDetail(task)));
    fireEvent.click(within(drawer).getByRole("tab", { name: "전이" }));
    const transitions = panel(drawer, "transitions");
    await waitFor(() => expect(transitions.textContent).toContain("backlog → claimed by builder-a"));
    expect(transitions.textContent).toContain("**not bold** <b>not html</b>");
    expect(transitions.querySelector("b, strong")).toBeNull();
    expect(transitions.querySelector("time")?.textContent).toBe("2026-09-21T10:30:00Z");
  });

  it("a relane event renders as a lane change, not a state transition", async () => {
    const task = mkBoardTask({});
    const detail = mkDetail(task, {
      events: [
        { id: 1, kind: "transition", from: "backlog", to: "claimed", by: "builder-a", at: "2026-09-21T10:30:00Z" },
        { id: 2, kind: "relane", from: "admiral-1", to: "director-1", by: "ops", note: "triage", at: "2026-09-21T11:00:00Z" },
      ],
    });
    const { drawer } = openPanel([task], vi.fn<FetchDoc>(), () => Promise.resolve(detail));
    fireEvent.click(within(drawer).getByRole("tab", { name: "전이" }));
    const transitions = panel(drawer, "transitions");
    await waitFor(() => expect(transitions.textContent).toContain("lane: admiral-1 → director-1 by ops"));
    expect(transitions.textContent).toContain("backlog → claimed by builder-a");
    expect(transitions.textContent).toContain("— triage");
  });
});

describe("overview body", () => {
  beforeEach(() => {
    localStorage.clear();
    window.history.replaceState(null, "", "/ui/queue");
  });

  it("body_doc renders the document inline in the overview", async () => {
    const fetchDoc = vi.fn<FetchDoc>(() => Promise.resolve(doc("# 본문 제목\n\n운영자가 읽을 본문")));
    const { drawer } = openPanel([mkBoardTask({ body_doc: "design/body" })], fetchDoc);
    const overview = panel(drawer, "overview");
    await waitFor(() => expect(overview.querySelector("[data-doc-renderer] h1")?.textContent).toBe("본문 제목"));
    expect(fetchDoc).toHaveBeenCalledWith("design/body");
  });

  it("no body_doc: the overview says so instead of going blank (mutant c)", () => {
    const fetchDoc = vi.fn<FetchDoc>();
    const { drawer } = openPanel([mkBoardTask({})], fetchDoc);
    const body = [...panel(drawer, "overview").querySelectorAll("section")].find((s) => s.querySelector("h4")?.textContent === "본문")!;
    expect(body).toBeTruthy();
    expect(body.querySelector("[data-doc-state]")?.getAttribute("data-doc-state")).toBe("none");
    expect(body.textContent).toContain("본문 문서가 연결되지 않았습니다");
    expect(body.textContent?.replace("본문", "").trim().length).toBeGreaterThan(10);
    expect(fetchDoc).not.toHaveBeenCalled();
  });

  it("a title-only hk:doc key is a named link, never inlined or fetched", () => {
    const fetchDoc = vi.fn<FetchDoc>(() => Promise.resolve(doc("# must not render")));
    const { drawer } = openPanel([mkBoardTask({ title: "fix it — 본문 hk:doc design/2026-09-21/task-body" })], fetchDoc);
    const overview = panel(drawer, "overview");
    const link = within(overview).getByRole("link", { name: "design/2026-09-21/task-body" });
    expect(link.getAttribute("href")).toBe("/ui/doc/design/2026-09-21/task-body");
    expect(link.parentElement?.textContent).toContain("title 에서 찾은 문서");
    expect(overview.querySelector("[data-doc-state]")?.getAttribute("data-doc-state")).toBe("title-fallback");
    expect(overview.querySelector("[data-doc-renderer]")).toBeNull();
    expect(fetchDoc).not.toHaveBeenCalled();
  });

  it("body_doc wins over a key in the title; the title key is not fetched", async () => {
    const fetchDoc = vi.fn<FetchDoc>(() => Promise.resolve(doc("# from body_doc")));
    const { drawer } = openPanel([mkBoardTask({ title: "x hk:doc design/other", body_doc: "design/body" })], fetchDoc);
    await waitFor(() => expect(panel(drawer, "overview").querySelector("[data-doc-renderer] h1")?.textContent).toBe("from body_doc"));
    expect(fetchDoc.mock.calls).toEqual([["design/body"]]);
    expect(panel(drawer, "overview").textContent).not.toContain("title 에서 찾은 문서");
  });
});

describe("one detail component for panel and page (A-7)", () => {
  it("/ui/tasks/<id> renders the same tabs and body as the panel", async () => {
    const task = mkBoardTask({ id: 8002, body_doc: "design/body" });
    const fetchDoc = vi.fn<FetchDoc>(() => Promise.resolve(doc("# page body")));
    const { container } = render(<TaskPage id={8002} fetchDetail={() => Promise.resolve(mkDetail(task))} fetchDoc={fetchDoc} />);
    await waitFor(() => expect(container.querySelector("[data-doc-renderer] h1")?.textContent).toBe("page body"));
    expect(within(container).getAllByRole("tab").map((t) => t.textContent)).toEqual(["개요", "코멘트", "전이"]);
    expect(container.querySelector(".qp-tablist")).not.toBeNull();
  });

  it("absorbs the retired TaskDetail's guarantees: linear id, per-model segments, https-only PR, report_path as text", async () => {
    const task = mkBoardTask({ id: 8003, refs: { pr: "javascript:alert(1)", report_path: "/home/op/report.md" } });
    const detail = mkDetail(task, {
      linear: { issue_id: "iss-1", identifier: "ROB-42" },
      participants: {
        task_ref: "hk:task/8003",
        coverage: "collected",
        segments: [
          { role: "worker", model_id: "model-a", reps: 1, rounds: 1, blockers_found: 0, completed: 1, input_tokens: 10, output_tokens: 5 },
          { role: "tester", model_id: "model-b", reps: 1, rounds: null, blockers_found: null, completed: null, input_tokens: null, output_tokens: null },
          { role: null, model_id: null, reps: 2, rounds: 0, blockers_found: 0, completed: 0, input_tokens: 0, output_tokens: 0 },
        ],
      },
    });
    const { container } = render(<TaskPage id={8003} fetchDetail={() => Promise.resolve(detail)} fetchDoc={vi.fn<FetchDoc>()} />);
    await waitFor(() => expect(container.textContent).toContain("linear: ROB-42"));
    const rows = container.querySelectorAll("table.qp-participants tbody tr");
    expect(rows).toHaveLength(3);
    expect(rows[0].textContent).toContain("model-a");
    expect(rows[1].textContent).toContain("미수집");
    expect(rows[2].textContent).toContain("unknown");
    expect(within(container).getByText(/pr: javascript:alert\(1\)/).querySelector("a")).toBeNull();
    // report_path is a local path: text only, never a /ui/doc link or a server file read.
    expect(container.textContent).toContain("report: /home/op/report.md");
    expect([...container.querySelectorAll("a")].some((a) => (a.getAttribute("href") ?? "").includes("report.md"))).toBe(false);
  });
});
