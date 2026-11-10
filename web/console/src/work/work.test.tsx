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
  it("lists task attachments and shows the empty state", async () => {
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("/attachments")) return json({ attachments: [{ sha256: "a".repeat(64), original_name: "notes.txt", size_bytes: 12, mime: "text/plain", created_at: "2026-11-05T00:00:00Z", created_by: "operator" }], truncated: true });
      if (url.includes("/comments")) return json({ comments: [], truncated: false });
      return json(detail);
    }));
    const { rerender } = render(<TaskRecord id={101} />);
    const link = await screen.findByRole("link", { name: "notes.txt" });
    expect(link.getAttribute("href")).toBe(`/ui/files/${"a".repeat(64)}`);
    expect(screen.getByText("100+")).toBeTruthy();
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => String(input).includes("/attachments") ? json({ attachments: [], truncated: false }) : String(input).includes("/comments") ? json({ comments: [], truncated: false }) : json(detail)));
    rerender(<TaskRecord id={102} />);
    expect(await screen.findByText("첨부 없음")).toBeTruthy();
  });
  it("W8 shows task and job durations", async () => {
    vi.setSystemTime(new Date("2026-11-05T04:00:00Z"));
    const job = { job_id: "job", role: "worker", state: "spawned", host: "host", family: "openai", profile: "p", model: "m", harness: null, effort: null, skills: [], skills_supported: true, claimed_at: null, spawned_at: "2026-11-05T01:00:00Z", ready_at: "2026-11-05T01:30:00Z", ended_at: null, end_reason: null, diff_stat: null, report_doc: "", run: { what: "모델 m", why: "선택 이유 기록 없음", others: [], result: "상태 실행 중", selection_recorded: false } };
    vi.stubGlobal("fetch", vi.fn(async () => json({ ...detail, task: { ...task, started_at: "2026-11-05T01:00:00Z" }, jobs: [job] })));
    render(<TaskRecord id={101} />);
    expect(await screen.findByText("대기: 1h 0m")).toBeTruthy();
    expect(screen.getByText("작업: 진행 중 3h 0m")).toBeTruthy();
    expect(screen.getByText(/spawned → ready 0h 30m/)).toBeTruthy();
  });
  it("W9 displays skills and unsupported harnesses", async () => {
    const job = { job_id: "job", role: "worker", state: "ended", host: "host", family: "openai", profile: "p", model: "m", harness: null, effort: null, skills: [{name:"alpha",calls:2},{name:"beta",calls:3}], skills_supported: true, claimed_at: null, spawned_at: null, ready_at: null, ended_at: null, end_reason: null, diff_stat: null, report_doc: "", run: { what: "모델 m", why: "선택 이유 기록 없음", others: [], result: "상태 실행 중", selection_recorded: false } };
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
    let detailCalls = 0;
    const fetcher = vi.fn(async (path: string) => path.endsWith("/comments") ? json({ comments: [], truncated: false }) : path.endsWith("/attachments") ? json({ attachments: [], truncated: false }) : ++detailCalls === 1 ? new Response("", { status: 500 }) : json(detail));
    vi.stubGlobal("fetch", fetcher);
    render(<TaskRecord id={101} />);
    fireEvent.click(await screen.findByRole("button", { name: "다시 시도" }));
    await waitFor(() => expect(screen.getByText(/Example task/)).toBeTruthy());
    expect(detailCalls).toBe(2);
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

it("job record shows what ran, and says so when no selection was recorded", async () => {
  const job = { job_id: "job", role: "worker", state: "spawned", host: "fictional-device", family: "", profile: "", model: "model-x", harness: null, effort: null, skills: [], skills_supported: true, claimed_at: null, spawned_at: null, ready_at: null, ended_at: null, end_reason: null, diff_stat: null, report_doc: "", run: { what: "모델 model-x · 호스트 fictional-device · 역할 worker", why: "선택 이유 기록 없음", others: [], result: "상태 실행 중", selection_recorded: false } };
  vi.stubGlobal("fetch", vi.fn(async () => json({ ...detail, jobs: [job] })));
  render(<TaskRecord id={101} />);
  expect(await screen.findByText("모델 model-x · 호스트 fictional-device · 역할 worker")).toBeTruthy();
  expect(screen.getByText("선택 이유 기록 없음")).toBeTruthy();
  expect(screen.getByText("상태 실행 중")).toBeTruthy();
  expect(screen.queryByText("다른 후보")).toBeNull();
  expect(screen.getByText("skills 없음")).toBeTruthy();
});

it("job record explains the choice, the other candidates and the result", async () => {
  const job = { job_id: "3516-builder-1", role: "builder", state: "completed", host: "mac-dev", family: "openai", profile: "terra@high", model: "gpt-6.1-terra", harness: "codex", effort: "high", skills: [], skills_supported: true, claimed_at: null, spawned_at: null, ready_at: null, ended_at: null, end_reason: "ready", diff_stat: { files: 3, insertions: 120, deletions: 8 }, report_doc: "report/3516 a",
    run: { what: "모델 gpt-6.1-terra · 하네스 codex · effort high · 프로필 terra@high · 호스트 mac-dev · 역할 builder", why: "작업 등급 B에 모델 등급 A+를 골랐습니다: 쿼터 tier waste(낭비 위험), 예상 사용률 76%, 긴급도 0.31.", override: "-m gpt-6.1-terra", others: ["claude-sonnet — 선택된 후보보다 순위가 낮음", "local-x — dropped:quota_exhausted"], result: "상태 완료 · 종료 사유 ready · 소요 1시간 5분 · 토큰 입력 1,200 / 출력 340 · diff 3 files +120 −8", selection_recorded: true } };
  vi.stubGlobal("fetch", vi.fn(async () => json({ ...detail, task: { ...task, refs: { job_id: "3516-builder-1", pr: "https://github.com/o/r/pull/9" } }, jobs: [job, { ...job, job_id: "other" }] })));
  render(<TaskRecord id={101} />);
  expect((await screen.findAllByText(/작업 등급 B에 모델 등급 A\+를 골랐습니다/)).length).toBe(2);
  expect(screen.getAllByText("직접 지정: -m gpt-6.1-terra").length).toBe(2);
  expect(screen.getAllByText("다른 후보").length).toBe(2);
  expect(screen.getAllByText("claude-sonnet — 선택된 후보보다 순위가 낮음").length).toBe(2);
  expect(screen.getAllByText("local-x — dropped:quota_exhausted").length).toBe(2);
  expect(screen.getAllByText(/소요 1시간 5분 · 토큰 입력 1,200/).length).toBe(2);
  // The report links to the document; only the job the task points at gets the PR link.
  expect(screen.getAllByRole("link", { name: "보고서" })[0].getAttribute("href")).toBe("/ui/doc/report/3516%20a");
  const prs = screen.getAllByRole("link", { name: "PR" });
  expect(prs.length).toBe(1);
  expect(prs[0].getAttribute("href")).toBe("https://github.com/o/r/pull/9");
});

it("describes priority events in Korean and omits unchanged state arrows", async () => {
  const events = [{ id: 1, kind: "metadata", field: "priority", from_value: "3", to_value: "2", from: "developing", to: "developing", by: "operator:test", note: "priority 3 → 2", at: "2026-11-05T01:00:00Z" }];
  vi.stubGlobal("fetch", vi.fn(async (path: string) => path.includes("/comments") ? json({ comments: [], truncated: false }) : json({ ...detail, events })));
  render(<TaskRecord id={101} />);
  const entry = await screen.findByText(/우선순위 P3 → P2/);
  expect(entry.textContent).not.toContain("developing");
  for (const title of ["종료 근거", "출처", "문서", "변경", "작업 실행", "소요 시간"]) expect(screen.getByRole("heading", { name: title })).toBeTruthy();
});
it("keeps translated comments and priority events in the timeline", async () => {
  const events = [{ id: 1, kind: "metadata", field: "priority", from_value: "3", to_value: "2", from: "developing", to: "developing", by: "operator", note: "priority 3 → 2", at: "2026-11-05T01:00:00Z" }];
  const comments = [{ id: 2, task_id: 101, body: "Original comment", body_ko: "댓글 번역", author: "worker", created_at: "2026-11-05T02:00:00Z" }];
  vi.stubGlobal("fetch", vi.fn(async (path: string) => path.includes("/comments") ? json({ comments, truncated: false }) : json({ ...detail, task: { ...task, title_ko: "작업 번역" }, events })));
  render(<TaskRecord id={101} />);
  expect(await screen.findByText("작업 번역")).toBeTruthy();
  expect(await screen.findByText("댓글 번역")).toBeTruthy();
  expect(screen.getByText(/우선순위 P3 → P2/)).toBeTruthy();
  expect(screen.getByRole("button", { name: "작업 #101 메뉴" })).toBeTruthy();
});
