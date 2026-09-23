import { describe, expect, it, vi, afterEach } from "vitest";
import { render, waitFor } from "@testing-library/react";
import {
  jobForPane,
  liveAgeLabel,
  liveRequestFailed,
  liveSectionLabel,
  loadLabel,
  memoryLabel,
  taskIdForJob,
  taskLiveJobs,
  type LiveJob,
  type LiveNode,
  type LiveResponse,
} from "../live";
import { FleetApp } from "../FleetApp";
import { LiveChips } from "./TaskRow";
import { LiveStrip } from "./LiveStrip";
import type { ProtoTask } from "./types";

// Fixtures mirror the real hub wire shapes field for field:
//   - jobs: panewire hub_jobs.go:414 hubConsoleJob
//   - nodes: panewire hub.go:108 HubNode + :127 HubNodeLoad +
//     checks.go:58 HubHostMemory + session_snapshot.go:43/59
// and internal/ui/live.go for the /ui/api/live envelope.
// Nothing here invents a shape the hub cannot send.

const NOW = "2026-09-23T12:00:00Z";

function mkJob(over: Partial<LiveJob>): LiveJob {
  return {
    machine: "m1a",
    job_id: "job-b598",
    owner_lane: "b598-lane",
    pane: "w1:p1",
    tier: "A+",
    role: "builder",
    started_at: "2026-09-23T11:00:00Z",
    last_event_kind: "heartbeat",
    last_event_at: "2026-09-23T11:59:30Z",
    ...over,
  };
}

function mkLive(over: Partial<LiveResponse>): LiveResponse {
  return {
    generated_at: NOW,
    jobs: { status: "ok", fetched_at: "2026-09-23T11:59:59Z", items: [] },
    nodes: { status: "ok", fetched_at: "2026-09-23T11:59:59Z", items: [] },
    links: [],
    mismatch: { basis: "current", tasks_without_job: [], jobs_without_task: [] },
    ...over,
  };
}

function mkTask(over: Partial<ProtoTask>): ProtoTask {
  return {
    id: 598,
    title: "live load view",
    kind: "implement",
    state: "in_progress",
    lane: "director-1",
    claimant: null,
    priority: 90,
    created_at: "2026-09-23T09:00:00Z",
    state_entered_at: null,
    due_at: null,
    blocker: null,
    created_by: "test",
    updated_at: null,
    parent_lane: null,
    refs: {},
    events: [],
    dwell: [],
    coverage: { status: "not_collected", participants: null },
    ...over,
  };
}

describe("memoryLabel — source-aware honesty", () => {
  it("vm_stat free-page percentage renders as 측정 불가, never a percentage", () => {
    // panewire checks.go: vm_stat free+inactive pages are not usable memory.
    const label = memoryLabel({ free_pct: 12.5, compressed_mb: null, swap_used_mb: null, psi_some_avg10: null, source: "vm_stat" });
    expect(label).toBe("측정 불가 (vm_stat)");
    expect(label).not.toContain("12.5");
    expect(label).not.toContain("%");
  });
  it("measured sources render the percentage with the source named", () => {
    expect(memoryLabel({ free_pct: 41.52, compressed_mb: 2048, swap_used_mb: 128, psi_some_avg10: 0.4, source: "memory_pressure" })).toBe(
      "41.5% (memory_pressure)",
    );
  });
  it("nil measurement is 미수집, never 0", () => {
    expect(memoryLabel(null)).toBe("미수집");
    expect(memoryLabel({ free_pct: null, compressed_mb: null, swap_used_mb: null, psi_some_avg10: null, source: "proc_meminfo" })).toBe(
      "측정 불가 (proc_meminfo)",
    );
  });
});

describe("loadLabel", () => {
  it("renders load5/ncpu and names each missing part", () => {
    expect(loadLabel({ load1: 1, load5: 2.5, load15: 3, ncpu: 10 })).toBe("load5 2.50 / 10 cpu");
    expect(loadLabel({ load1: null, load5: 2.5, load15: null, ncpu: null })).toBe("load5 2.50 / ncpu 미상");
    expect(loadLabel({ load1: 1, load5: null, load15: 3, ncpu: 10 })).toBe("측정 불가");
    expect(loadLabel(null)).toBe("미수집");
  });
});

describe("liveAgeLabel", () => {
  it("uses minute resolution and never fabricates zero", () => {
    expect(liveAgeLabel(NOW, "2026-09-23T11:59:30Z")).toBe("방금");
    expect(liveAgeLabel(NOW, "2026-09-23T11:40:00Z")).toBe("20분");
    expect(liveAgeLabel(NOW, "2026-09-23T09:00:00Z")).toBe("3시간");
    expect(liveAgeLabel(NOW, "2026-09-21T12:00:00Z")).toBe("2일");
    expect(liveAgeLabel(NOW, null)).toBeNull();
    expect(liveAgeLabel(NOW, "not-a-date")).toBeNull();
  });
});

describe("task↔job linkage is exact-equality only", () => {
  const live = mkLive({
    jobs: {
      status: "ok",
      fetched_at: "2026-09-23T11:59:59Z",
      items: [
        mkJob({}),
        mkJob({ job_id: "job-t598", owner_lane: "b598-lane", role: "tester", pane: "w16:p2" }),
        // owner_lane prefix-trap: "b598-lane-x" must not join "b598-lane".
        mkJob({ job_id: "job-stray", owner_lane: "b598-lane-x", role: "worker", pane: "w2:p1" }),
      ],
    },
    links: [{ task_id: 598, job_id: "job-b598", job_found: true, children: ["job-t598"] }],
  });

  it("resolves primary + one-hop children by exact ids", () => {
    const { recorded, primary, children } = taskLiveJobs(live, 598);
    expect(recorded).toBe(true);
    expect(primary?.job_id).toBe("job-b598");
    expect(children.map((j) => j.job_id)).toEqual(["job-t598"]);
  });
  it("a task without a link entry is 'not recorded', not 'no job'", () => {
    expect(taskLiveJobs(live, 999).recorded).toBe(false);
  });
  it("session pane matching is exact", () => {
    expect(jobForPane(live, "w1:p1")?.job_id).toBe("job-b598");
    expect(jobForPane(live, "w1:p10")).toBeNull();
    expect(jobForPane(live, "w1:p")).toBeNull();
    expect(jobForPane(live, "")).toBeNull();
  });
  it("job → task resolves through the link and through a child hop", () => {
    expect(taskIdForJob(live, "job-b598")).toBe(598);
    expect(taskIdForJob(live, "job-t598")).toBe(598);
    expect(taskIdForJob(live, "job-stray")).toBeNull();
    expect(taskIdForJob(live, "job-b59")).toBeNull(); // prefix is not a match
  });
});

describe("LiveChips", () => {
  const linkedTask = mkTask({ refs: { job_id: "job-b598" } });
  const live = mkLive({
    jobs: { status: "ok", fetched_at: "2026-09-23T11:59:59Z", items: [mkJob({}), mkJob({ job_id: "job-t598", role: "tester", pane: "w16:p2" })] },
    links: [{ task_id: 598, job_id: "job-b598", job_found: true, children: ["job-t598"] }],
  });

  it("renders role·machine·pane·elapsed·event-age chips for a linked task", () => {
    const { container } = render(<LiveChips task={linkedTask} now={NOW} live={live} />);
    const chips = [...container.querySelectorAll(".qp-livechip")];
    expect(chips).toHaveLength(2);
    expect(chips[0].textContent).toContain("builder");
    expect(chips[0].textContent).toContain("m1a");
    expect(chips[0].textContent).toContain("w1:p1");
    expect(chips[0].textContent).toContain("1시간째");
    expect(chips[0].textContent).toContain("evt 방금 전");
    expect(chips[1].textContent).toContain("tester");
  });

  it("a hub failure is '잡 조회 불가', never '활성 잡 없음'", () => {
    const failed = mkLive({ jobs: { status: "timeout", fetched_at: "", items: [] } });
    const { container } = render(<LiveChips task={linkedTask} now={NOW} live={failed} />);
    expect(container.textContent).toContain("잡 조회 불가");
    expect(container.textContent).toContain("시간 초과");
    expect(container.textContent).not.toContain("활성 잡 없음");
  });

  it("a degraded section still renders last-good job chips", () => {
    const degraded = mkLive({
      jobs: { status: "http_error", fetched_at: "2026-09-23T11:59:59Z", items: [mkJob({})] },
      links: [{ task_id: 598, job_id: "job-b598", job_found: true, children: [] }],
    });
    const { container } = render(<LiveChips task={linkedTask} now={NOW} live={degraded} />);
    expect(container.textContent).toContain("builder");
  });

  it("recorded-but-absent job vs unrecorded job_id are distinct", () => {
    const absent = render(
      <LiveChips task={linkedTask} now={NOW} live={mkLive({ links: [{ task_id: 598, job_id: "job-b598", job_found: false, children: [] }] })} />,
    );
    expect(absent.container.textContent).toContain("활성 잡 없음");
    const unrecorded = render(<LiveChips task={mkTask({ id: 599, refs: {} })} now={NOW} live={live} />);
    expect(unrecorded.container.textContent).toContain("잡 ID 미기록");
  });

  it("non-live states and synthetic datasets render nothing", () => {
    const backlog = render(<LiveChips task={mkTask({ state: "backlog" })} now={NOW} live={live} />);
    expect(backlog.container.textContent).toBe("");
    const synth = render(<LiveChips task={linkedTask} now={NOW} live={undefined} />);
    expect(synth.container.textContent).toBe("");
  });
});

describe("LiveStrip", () => {
  it("shows counts when healthy and names failures when not", () => {
    const ok = mkLive({ jobs: { status: "ok", fetched_at: "x", items: [mkJob({}), mkJob({ job_id: "j2" })] } });
    const { container, unmount } = render(<LiveStrip live={ok} />);
    expect(container.textContent).toContain("잡 2");
    unmount();
    const failed = mkLive({
      jobs: { status: "auth_failed", fetched_at: "", items: [] },
      mismatch: { basis: "unavailable", tasks_without_job: [], jobs_without_task: [] },
    });
    const { container: c2 } = render(<LiveStrip live={failed} />);
    expect(c2.textContent).toContain("허브 인증 실패");
    expect(c2.textContent).toContain("대조 불가");
  });

  it("lists mismatches explicitly, tagged when the basis is cached", () => {
    const live = mkLive({
      mismatch: { basis: "cached", tasks_without_job: [500, 472], jobs_without_task: ["job-orphan"] },
    });
    const { container } = render(<LiveStrip live={live} />);
    expect(container.textContent).toContain("#500");
    expect(container.textContent).toContain("#472");
    expect(container.textContent).toContain("job-orphan");
    expect(container.textContent).toContain("마지막 성공 잡 목록 기준");
  });
});

describe("FleetApp machine/session view", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  function livePayload(): LiveResponse {
    const node: LiveNode = {
      machine_id: "m1a",
      state: "ready",
      accepting_effective: true,
      last_ping_ms: 800,
      load: { load1: 1.2, load5: 2.5, load15: 3.1, ncpu: 10 },
      memory: { free_pct: 41.5, compressed_mb: 2048, swap_used_mb: 128, psi_some_avg10: 0.4, source: "memory_pressure" },
      session_snapshot: {
        sessions: [
          { pane_id: "w1:p1", workspace_id: "handoffkeep.t598", label: "b598-console-live-load", status: "working", revision: 1, state_change_seq: 1 },
          { pane_id: "opa:1", workspace_id: "ncp-trade", label: "opa-btc", status: "working", revision: 2, state_change_seq: 2 },
        ],
        snapshot_status: "ok",
        truncated: false,
        received_at: "2026-09-23T11:59:00Z",
        stale: false,
      },
      display_state: "sessions",
      active_jobs: 2,
    };
    const vmstat: LiveNode = {
      machine_id: "m1b",
      state: "ready",
      accepting_effective: false,
      last_ping_ms: null,
      load: null,
      memory: { free_pct: 12.5, compressed_mb: null, swap_used_mb: null, psi_some_avg10: null, source: "vm_stat" },
      session_snapshot: { sessions: [], snapshot_status: "ok", truncated: false, received_at: "2026-09-23T10:00:00Z", stale: true },
      display_state: "empty",
      active_jobs: 0,
    };
    return mkLive({
      jobs: {
        status: "ok",
        fetched_at: "2026-09-23T11:59:59Z",
        items: [mkJob({}), mkJob({ job_id: "job-t598", role: "tester", pane: "opa:1" })],
      },
      nodes: { status: "ok", fetched_at: "2026-09-23T11:59:59Z", items: [node, vmstat] },
      links: [{ task_id: 598, job_id: "job-b598", job_found: true, children: ["job-t598"] }],
    });
  }

  it("renders load/memory/jobs/sessions and exact session→task backlinks", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(livePayload()), { status: 200 })));
    const { container } = render(<FleetApp />);
    await waitFor(() => expect(container.textContent).toContain("m1a"));
    expect(container.textContent).toContain("load5 2.50 / 10 cpu");
    expect(container.textContent).toContain("41.5% (memory_pressure)");
    expect(container.textContent).toContain("활성 잡 2");
    // vm_stat percentage is never shown as a number.
    expect(container.textContent).toContain("측정 불가 (vm_stat)");
    expect(container.textContent).not.toContain("12.5");
    // Sessions render label/status/pane/workspace — nothing else.
    expect(container.textContent).toContain("b598-console-live-load");
    expect(container.textContent).toContain("opa-btc");
    // session pane w1:p1 → job-b598 → task #598.
    const link = [...container.querySelectorAll("a")].find((a) => a.textContent?.trim() === "#598");
    expect(link?.getAttribute("href")).toBe("/ui/queue?task=598");
    // stale snapshot is badged, not presented as idle.
    expect(container.textContent).toContain("stale");
    expect(container.textContent).toContain("중지");
  });

  it("a failed nodes section never renders as an idle fleet", async () => {
    const payload = mkLive({ nodes: { status: "timeout", fetched_at: "", items: [] } });
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(payload), { status: 200 })));
    const { container } = render(<FleetApp />);
    await waitFor(() => expect(container.textContent).toContain("시간 초과"));
    expect(container.textContent).not.toContain("등록된 머신이 없습니다");
    expect(container.textContent).toContain("머신 목록을 받지 못했습니다");
  });

  it("request failure keeps the page explicit, not empty", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("down", { status: 502 })));
    const { container } = render(<FleetApp />);
    await waitFor(() => expect(container.textContent).toContain("머신 정보를 아직 받지 못했습니다"));
  });
});

describe("liveRequestFailed", () => {
  it("is an explicit failure shape, never an ok", () => {
    const failed = liveRequestFailed();
    expect(failed.jobs.status).toBe("request_failed");
    expect(failed.nodes.status).toBe("request_failed");
    expect(failed.mismatch.basis).toBe("unavailable");
    expect(liveSectionLabel(failed.jobs.status)).toBe("조회 요청 실패");
  });
});
