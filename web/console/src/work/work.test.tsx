import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { fetchBoardTasks, fetchTaskDetail } from "../shared/boardApi";
import type { BoardTask } from "../shared/boardTypes";
import { TaskRecord } from "./TaskRecord";
import { inView } from "./views";
import { navKey } from "../app/routes";

const task: BoardTask = { id: 101, title: "Example task", lane: "x", kind: "implement", state: "developing", priority: 2, created_by: "operator", created_at: "2026-11-05T00:00:00Z", updated_at: "2026-11-05T01:00:00Z", refs: {}, project: "a" };
const detail = { task, events: [], dwell: [], linear: null, participants: { task_ref: "hk:task/101", coverage: "not_collected", segments: [] }, origins: [], documents: [], documents_truncated: false, changes: [], jobs: [], origins_truncated: false, changes_truncated: false, jobs_truncated: false };
const json = (data: unknown) => new Response(JSON.stringify(data), { status: 200, headers: { "Content-Type": "application/json" } });
afterEach(() => { vi.unstubAllGlobals(); vi.useRealTimers(); });

describe("Work", () => {
  it("W1 rejects malformed changes and shows a load error", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => json({ ...detail, changes: {} })));
    await expect(fetchTaskDetail(101)).rejects.toThrow("malformed");
    render(<TaskRecord id={101} />);
    expect(await screen.findByText("불러오지 못함")).toBeTruthy();
  });
  it("W2 counts the view predicates and excludes completed decisions", () => {
    const rows = [{ ...task, awaiting_decision: true }, { ...task, id: 102, state: "completed", kind: "decide" }, { ...task, id: 103, state: "backlog" }];
    expect(rows.filter(t => inView(t, "needs-me")).map(t => t.id)).toEqual([101, 103]);
    expect(rows.filter(t => inView(t, "active"))).toHaveLength(1);
    expect(rows.filter(t => inView(t, "all"))).toHaveLength(3);
  });
  it("W4 rejects a non-advancing cursor", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => json({ tasks: [task], states: [], generated_at: "now", truncated: true, next_after_id: 101 })));
    await expect(fetchBoardTasks("a")).rejects.toThrow("non-advancing");
  });
  it("W4 caps a long cursor walk at 5000", async () => {
    let page = 0;
    vi.stubGlobal("fetch", vi.fn(async () => {
      const rows = Array.from({ length: 500 }, (_, i) => ({ ...task, id: page * 500 + i + 1 }));
      page++;
      return json({ tasks: rows, states: [], generated_at: "now", truncated: true, next_after_id: page * 500 });
    }));
    const result = await fetchBoardTasks("a");
    expect(result.tasks).toHaveLength(5000);
    expect(result.truncated).toBe(true);
  });
  it("W5 includes unknown states in All", () => { expect(inView({ ...task, state: "future_state" }, "all")).toBe(true); });
  it("W6 displays closure evidence and never calls a merged change completion", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => json({ ...detail, task: { ...task, refs: { closure: { type: "satisfied", evidence: "result link" } } }, changes: [{ id: 1, pr: "https://example.com/pr/1", state: "merged", reviews: [], tasks: [] }] })));
    render(<TaskRecord id={101} />);
    expect(await screen.findByText(/satisfied · result link/)).toBeTruthy();
    expect(screen.queryByText("완료")).toBeNull();
  });
  it("W7 shows unknown review counts", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => json({ ...detail, changes: [{ id: 1, pr: "https://example.com/pr/1", state: "open", tasks: [], reviews: [{ id: 1, lens: "security", round: 1, pass: 1, high: null }] }] })));
    render(<TaskRecord id={101} />);
    expect(await screen.findByText(/h \?/)).toBeTruthy();
  });
  it("W8 shows task and job durations", async () => {
    vi.setSystemTime(new Date("2026-11-05T04:00:00Z"));
    const job = { job_id: "job", role: "worker", state: "spawned", host: "host", family: "openai", profile: "p", model: "m", harness: null, effort: null, skills: [], skills_supported: true, claimed_at: null, spawned_at: "2026-11-05T01:00:00Z", ready_at: "2026-11-05T01:30:00Z", ended_at: null, end_reason: null, diff_stat: null };
    vi.stubGlobal("fetch", vi.fn(async () => json({ ...detail, task: { ...task, started_at: "2026-11-05T01:00:00Z" }, jobs: [job] })));
    render(<TaskRecord id={101} />);
    expect(await screen.findByText("Queue wait: 1h 0m")).toBeTruthy();
    expect(screen.getByText("Work: 진행 중 3h 0m")).toBeTruthy();
    expect(screen.getByText(/spawned → ready 0h 30m/)).toBeTruthy();
  });
  it("W9 displays skills and unsupported harnesses", async () => {
    const job = { job_id: "job", role: "worker", state: "ended", host: "host", family: "openai", profile: "p", model: "m", harness: null, effort: null, skills: [{name:"alpha",calls:2},{name:"beta",calls:3}], skills_supported: true, claimed_at: null, spawned_at: null, ready_at: null, ended_at: null, end_reason: null, diff_stat: null };
    vi.stubGlobal("fetch", vi.fn(async () => json({ ...detail, jobs: [job, { ...job, job_id: "other", skills_supported: false }] })));
    render(<TaskRecord id={101} />);
    expect(await screen.findByText(/alpha \(2\), beta \(3\)/)).toBeTruthy();
    expect(screen.getByText(/skills not supported/)).toBeTruthy();
  });
  it("W6 reports a terminal task without closure", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => json({ ...detail, task: { ...task, state: "dropped" } })));
    render(<TaskRecord id={101} />);
    expect(await screen.findByText("종결 증거 없음")).toBeTruthy();
  });
  it("W11 reports a missing task", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("", { status: 404 })));
    render(<TaskRecord id={999} />);
    expect(await screen.findByText("#999 없음")).toBeTruthy();
  });
  it("W10 assigns the task page to Work", () => { expect(navKey({ view: "task", id: "101" })).toBe("work"); });
  it("W11 retries a failed record", async () => {
    const fetcher = vi.fn().mockResolvedValueOnce(new Response("", { status: 500 })).mockResolvedValueOnce(json(detail));
    vi.stubGlobal("fetch", fetcher);
    render(<TaskRecord id={101} />);
    fireEvent.click(await screen.findByRole("button", { name: "다시 시도" }));
    await waitFor(() => expect(screen.getByText(/Example task/)).toBeTruthy());
    expect(fetcher).toHaveBeenCalledTimes(3);
  });
});

it("W13 keeps the existing routes available", async () => {
  const { matchRoute } = await import("../app/routes");
  expect(matchRoute("/ui")).toEqual({ view: "home" });
  expect(matchRoute("/ui/doc/guide/example")).toEqual({ view: "doc", key: "guide/example" });
  expect(matchRoute("/ui/tasks/101")).toEqual({ view: "task", id: "101" });
});

it("shows origin namespace and linked document classes, including an unclassified document", async () => {
  vi.stubGlobal("fetch", vi.fn(async () => json({
    ...detail,
    origins: [{ provider: "github", namespace: "example/repo", display_ref: "#42", url: "https://example.com/42" }],
    documents: [
      { key: "reports/one", kind: "report", origin_class: "job" },
      { key: "reports/two", kind: "note", origin_class: "" },
    ],
  })));
  render(<TaskRecord id={101} />);
  expect(await screen.findByText(/github · example\/repo/)).toBeTruthy();
  expect(screen.getByText(/report · job/)).toBeTruthy();
  expect(screen.getByText(/note · 미분류/)).toBeTruthy();
  expect(screen.getByRole("link", { name: "reports/one" }).getAttribute("href")).toBe("/ui/doc/reports/one");
  expect(screen.queryByText(/origin class: unknown/)).toBeNull();
});

it("rejects a malformed linked document list", async () => {
  vi.stubGlobal("fetch", vi.fn(async () => json({ ...detail, documents: [{ key: "x", kind: "note", origin_class: 42 }] })));
  await expect(fetchTaskDetail(101)).rejects.toThrow("malformed");
});

it("job record includes host and omits empty fields", async () => {
  const job = { job_id: "job", role: "worker", state: "spawned", host: "fictional-device", family: "", profile: "", model: "model-x", harness: null, effort: null, skills: [], skills_supported: true, claimed_at: null, spawned_at: null, ready_at: null, ended_at: null, end_reason: null, diff_stat: null };
  vi.stubGlobal("fetch", vi.fn(async () => json({ ...detail, jobs: [job] })));
  render(<TaskRecord id={101} />);
  expect(await screen.findByText("job · worker · model-x · fictional-device")).toBeTruthy();
});
