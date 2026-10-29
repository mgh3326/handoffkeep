// hk 2280 home dashboard — the page renders quota with pace, hub hosts and
// jobs from /ui/api/live, the PR pipeline from refs.pr, and the sources hk
// does not record. Failures render as failures, never as an empty fleet.

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { DashboardApp, paceTone, relativeLabel } from "./DashboardApp";
import type { DashboardResponse } from "./types";
import type { LiveResponse } from "../../live";

afterEach(cleanup);

const NOW = "2026-10-29T12:00:00Z";

function dashboard(): DashboardResponse {
  return {
    generated_at: NOW,
    quota: [
      {
        account_key: "claude:0123456789abcdef",
        pool: "claude",
        auto: {
          measured_at: NOW,
          plan: "max",
          buckets: [
            { window: "5h", used_pct: 60, resets_at: "2026-10-29T14:00:00Z", scope: "account", elapsed_pct: 40, pace: 1.5 },
            { window: "credit", used_pct: 10, scope: "account", elapsed_pct: null, pace: null },
          ],
        },
        manual: [{ window: "7d", measured_at: NOW, expires_at: "2026-10-29T11:00:00Z", expired: true, used_pct: 55 }],
      },
    ],
    work: [
      { id: 11, title: "running task", lane: "lane-a", state: "in_progress", job_id: "job-1", updated_at: NOW },
      { id: 12, title: "lost task", lane: "lane-b", state: "claimed", updated_at: NOW },
    ],
    prs: [
      {
        repo: "mgh3326/handoffkeep",
        open: [{ pr: "https://github.com/mgh3326/handoffkeep/pull/94", number: 94, task_id: 11, title: "running task", state: "verifying", head_sha: "abcdef0123", at: NOW }],
        merged: [{ pr: "https://github.com/mgh3326/handoffkeep/pull/93", number: 93, task_id: 9, title: "landed", state: "merged", at: NOW, merged_at: NOW }],
      },
      { repo: "mgh3326/panewire", open: [], merged: [] },
      { repo: "", open: [{ pr: "#12", task_id: 13, title: "odd ref", state: "hold", at: NOW }], merged: [] },
    ],
    missing: [
      { section: "prs", source: "PR review / CI / mergeability state", note: "not recorded" },
      { section: "quota_stops", source: "quota-stop decisions", note: "no gate verdicts" },
    ],
  };
}

function live(): LiveResponse {
  return {
    generated_at: NOW,
    jobs: {
      status: "ok",
      fetched_at: NOW,
      items: [{ machine: "node-a", job_id: "job-1", owner_lane: "lane-a", role: "worker", tier: "opus", started_at: "2026-10-29T11:30:00Z" }],
    },
    nodes: {
      status: "ok",
      fetched_at: NOW,
      items: [
        {
          machine_id: "node-a",
          state: "online",
          accepting_effective: true,
          last_ping_ms: 5000,
          load: { load1: 1, load5: 1.5, load15: 1, ncpu: 8 },
          memory: null,
          session_snapshot: null,
          display_state: "empty",
          active_jobs: 1,
        },
      ],
    },
    links: [{ task_id: 11, job_id: "job-1", job_found: true, children: [] }],
    mismatch: { basis: "current", tasks_without_job: [12], jobs_without_task: [] },
  };
}

const okDashboard = () => Promise.resolve(dashboard());
const okLive = () => Promise.resolve(live());
const failed = () => Promise.reject(new Error("boom"));

describe("DashboardApp (hk 2280)", () => {
  it("renders quota, hosts, jobs, PRs and the missing sources", async () => {
    render(<DashboardApp loadDashboard={okDashboard} loadLive={okLive} />);
    expect(screen.getByRole("heading", { level: 1 }).textContent).toContain("홈");

    const quota = await screen.findByRole("region", { name: "쿼터" });
    expect(within(quota).getByText("claude")).toBeTruthy();
    expect(within(quota).getByText("×1.50").className).toContain("hd-danger");
    expect(within(quota).getByText("만료됨")).toBeTruthy();

    const hosts = await screen.findByRole("region", { name: "호스트" });
    await waitFor(() => expect(within(hosts).getByText("load5 1.50 / 8 cpu")).toBeTruthy());

    const jobs = screen.getByRole("region", { name: "실행 중 작업" });
    expect(within(jobs).getByText("opus")).toBeTruthy();
    expect(within(jobs).getAllByText("running task")[0].closest("a")!.getAttribute("href")).toBe("/ui/tasks/11");
    // An active task without a hub job is listed, not dropped.
    expect(within(jobs).getByText("lost task")).toBeTruthy();
    expect(within(jobs).getByText("job_id 미기록")).toBeTruthy();

    const prs = screen.getByRole("region", { name: "PR 파이프라인" });
    expect(within(prs).getByText("#94")).toBeTruthy();
    expect(within(prs).getByText("abcdef0")).toBeTruthy();
    expect(within(prs).getByText("mgh3326/panewire")).toBeTruthy();
    expect(within(prs).getByText("최근 머지 1")).toBeTruthy();
    // A refs.pr that is not a GitHub pull URL is text, never a link.
    expect(within(prs).getByText("#94").closest("a")!.getAttribute("href")).toBe("https://github.com/mgh3326/handoffkeep/pull/94");
    expect(within(prs).getByText("#12").closest("a")).toBeNull();

    const stops = screen.getByRole("region", { name: "쿼터 정지 결정" });
    expect(stops.textContent).toContain("hk에 기록 없음");
    expect(stops.textContent).toContain("PR review / CI / mergeability state");
  });

  it("a failed first dashboard load says so instead of rendering empty cards", async () => {
    render(<DashboardApp loadDashboard={failed} loadLive={okLive} />);
    await waitFor(() => expect(screen.getByText(/대시보드를 불러오지 못했습니다/)).toBeTruthy());
    expect(screen.queryByRole("region", { name: "쿼터" })).toBeNull();
  });

  it("a failed live load marks hosts and jobs as failed, never as an idle fleet", async () => {
    render(<DashboardApp loadDashboard={okDashboard} loadLive={failed} />);
    const hosts = await screen.findByRole("region", { name: "호스트" });
    await waitFor(() => expect(within(hosts).getByText(/조회 요청 실패/)).toBeTruthy());
    expect(within(hosts).queryByText(/등록된 호스트가 없습니다/)).toBeNull();
    const jobs = screen.getByRole("region", { name: "실행 중 작업" });
    expect(within(jobs).queryByText(/실행 중인 잡이 없습니다/)).toBeNull();
    // Without a jobs list there is no basis to call a task orphaned.
    expect(within(jobs).queryByText("lost task")).toBeNull();
  });
});

describe("dashboard helpers", () => {
  it("relativeLabel renders future and past spans", () => {
    const now = Date.parse(NOW);
    expect(relativeLabel(now, "2026-10-29T14:05:00Z")).toBe("2시간 5분 후");
    expect(relativeLabel(now, "2026-10-29T11:57:00Z")).toBe("3분 전");
    expect(relativeLabel(now, "2026-10-31T13:00:00Z")).toBe("2일 1시간 후");
    expect(relativeLabel(now, undefined)).toBeNull();
    expect(relativeLabel(now, "nope")).toBeNull();
  });

  it("paceTone flags over-pace windows", () => {
    expect(paceTone(null)).toBe("");
    expect(paceTone(0.8)).toBe("");
    expect(paceTone(1.1)).toBe("warn");
    expect(paceTone(1.25)).toBe("danger");
  });
});
