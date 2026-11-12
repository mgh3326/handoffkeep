import { fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { WorkStatusPage } from "./WorkStatusPage";
import type { WorkStatusResponse } from "./types";

const mockWorkStatus: WorkStatusResponse = {
  generated_at: "2026-11-12T10:00:00Z",
  summary: {
    total_open: 6,
    backlog: 3,
    in_work: 1,
    in_review: 1,
    waiting_deploy: 1,
    merged_no_deploy_record: 0,
    bundle_parents: 1,
  },
  projects: [
    {
      project: "handoffkeep",
      counts: {
        total_open: 4,
        backlog: 2,
        in_work: 1,
        in_review: 0,
        waiting_deploy: 1,
        merged_no_deploy_record: 0,
        bundle_parents: 1,
      },
    },
    {
      project: "panewire",
      counts: {
        total_open: 2,
        backlog: 1,
        in_work: 0,
        in_review: 1,
        waiting_deploy: 0,
        merged_no_deploy_record: 0,
        bundle_parents: 0,
      },
    },
  ],
  in_work: [
    {
      id: 4265,
      title: "Console work status page",
      project: "handoffkeep",
      state: "developing",
      since: "2026-11-12T09:00:00Z",
      age_seconds: 3600,
      job_id: "job-101",
      host: "instance-1",
      pr: "https://github.com/example/repo/pull/4265",
    },
    {
      id: 4266,
      title: "Review work status page",
      project: "panewire",
      state: "reviewing",
      since: "2026-11-12T09:30:00Z",
      age_seconds: 1800,
      job_id: null,
      host: null,
      pr: null,
    },
  ],
  backlog: [
    {
      id: 4270,
      title: "Backlog item 1",
      project: "handoffkeep",
      state: "drafting",
      priority: 1,
      created_at: "2026-11-12T01:00:00Z",
      bundle_id: 4200,
      bundle_title: "[bundle hk-core] Infrastructure epic",
    },
    {
      id: 4271,
      title: "Backlog item 2",
      project: "handoffkeep",
      state: "queued",
      priority: 3,
      created_at: "2026-11-12T02:00:00Z",
      bundle_id: null,
      bundle_title: null,
    },
    {
      id: 4280,
      title: "Panewire backlog item",
      project: "panewire",
      state: "queued",
      priority: 2,
      created_at: "2026-11-12T03:00:00Z",
      bundle_id: null,
      bundle_title: null,
    },
  ],
  movement: [
    { date: "2026-11-11", created: 5, closed: 3 },
    { date: "2026-11-12", created: 2, closed: 4 },
  ],
};

function serve(body: unknown, status = 200) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(JSON.stringify(body), { status })),
  );
}

describe("WorkStatusPage", () => {
  beforeEach(() => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(mockWorkStatus))));
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("renders loading status initially", () => {
    vi.stubGlobal("fetch", vi.fn(() => new Promise(() => {})));
    render(<WorkStatusPage />);
    expect(screen.getByRole("status").textContent).toContain("작업 현황 불러오는 중");
  });

  it("renders error alert on fetch failure", async () => {
    serve({ error: "server error" }, 500);
    render(<WorkStatusPage />);
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.getByText(/데이터를 불러오지 못했습니다/)).toBeTruthy();
  });

  it("renders summary cards with counts", async () => {
    render(<WorkStatusPage />);
    const region = await screen.findByRole("region", { name: "요약 지표" });
    expect(within(region).getByText("전체 열린 작업")).toBeTruthy();
    expect(within(region).getByText("6")).toBeTruthy();
    expect(within(region).getByText("머지됨 (배포 기록 없음)")).toBeTruthy();
    expect(within(region).getByText("번들 (작업 제외)")).toBeTruthy();
  });

  it("renders project table and summary footer", async () => {
    render(<WorkStatusPage />);
    const region = await screen.findByRole("region", { name: "프로젝트별 작업 건수" });
    expect(within(region).getByText("handoffkeep")).toBeTruthy();
    expect(within(region).getByText("panewire")).toBeTruthy();
    expect(within(region).getByText("합계")).toBeTruthy();
  });

  it("renders movement stats", async () => {
    render(<WorkStatusPage />);
    const region = await screen.findByRole("region", { name: "이동 현황 (최근 7일 및 오늘)" });
    expect(within(region).getByText("11-11")).toBeTruthy();
    expect(within(region).getByText("+5")).toBeTruthy();
    expect(within(region).getByText("-3")).toBeTruthy();
    expect(within(region).getAllByTitle("순 증감").length).toBe(2);
  });

  it("renders in-work list with metadata, job host, and PR link", async () => {
    render(<WorkStatusPage />);
    const region = await screen.findByRole("region", { name: "진행 중인 작업 목록" });
    expect(within(region).getByText("Console work status page")).toBeTruthy();
    expect(within(region).getByText("instance-1 · job-101")).toBeTruthy();
    const prLink = within(region).getByRole("link", { name: "PR ↗" });
    expect(prLink.getAttribute("href")).toBe("https://github.com/example/repo/pull/4265");
    expect(within(region).getByText("1시간")).toBeTruthy();
  });

  it("renders backlog list and filters by project", async () => {
    render(<WorkStatusPage />);
    expect(await screen.findByRole("region", { name: "대기 목록" })).toBeTruthy();
    expect(screen.getByText("Backlog item 1")).toBeTruthy();
    expect(screen.getByText("Panewire backlog item")).toBeTruthy();

    const bundleTag = screen.getByRole("link", { name: "번들 #4200" });
    expect(bundleTag.getAttribute("href")).toBe("/ui/tasks/4200");
    expect(bundleTag.getAttribute("title")).toBe("[bundle hk-core] Infrastructure epic");

    const select = screen.getByLabelText("프로젝트 필터:");
    fireEvent.change(select, { target: { value: "panewire" } });

    expect(screen.queryByText("Backlog item 1")).toBeNull();
    expect(screen.getByText("Panewire backlog item")).toBeTruthy();
  });
});
