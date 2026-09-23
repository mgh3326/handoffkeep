// Shared types + helpers for the /ui/api/live read-only aggregation (#598).
// The response shape mirrors internal/ui/live.go, which itself mirrors
// panewire hub_jobs.go hubConsoleJob (/v1/jobs) and hub.go HubNode
// (/v1/nodes: load, memory, session_snapshot). Field names are the hub's own.
//
// Honesty contract carried end to end: a nil hub measurement stays null and
// renders as 미수집/측정 불가 — never 0. A hub failure is a section status —
// never an empty list. A stale snapshot renders as stale — never "한가함".

export type LiveSectionStatus =
  | "ok"
  | "timeout"
  | "auth_failed"
  | "http_error"
  | "unsupported"
  | "unconfigured"
  | "request_failed";

/** panewire hub_jobs.go:414 hubConsoleJob — one active job on a machine. */
export type LiveJob = {
  machine: string;
  job_id: string;
  owner_lane?: string;
  pane?: string;
  tier?: string;
  role?: string;
  started_at?: string;
  last_event_kind?: string;
  last_event_at?: string;
};

/** panewire hub.go:127 HubNodeLoad — CPU load; nulls are unmeasured values. */
export type LiveLoad = {
  load1: number | null;
  load5: number | null;
  load15: number | null;
  ncpu: number | null;
};

/** panewire checks.go:58 HubHostMemory — free_pct is already 0..100.
 * source names the measurement ("memory_pressure", "vm_stat", "proc_meminfo",
 * "cgroup", "os-release"). vm_stat free-page % is not usable memory, so the
 * client renders it as 측정 불가 rather than a percentage. */
export type LiveMemory = {
  free_pct: number | null;
  compressed_mb: number | null;
  swap_used_mb: number | null;
  psi_some_avg10: number | null;
  source: string;
};

/** panewire session_snapshot.go:43 HubSession — metadata only, never content. */
export type LiveSession = {
  pane_id: string;
  workspace_id: string;
  label: string;
  agent_name?: string;
  display_label?: string;
  status: string;
  interactive_ready?: boolean;
  revision: number;
  state_change_seq: number;
};

/** panewire session_snapshot.go:59 HubSessionSnapshot. */
export type LiveSnapshot = {
  sessions: LiveSession[];
  snapshot_status: string;
  truncated: boolean;
  received_at: string;
  stale: boolean;
};

export type LiveNode = {
  machine_id: string;
  state: string;
  accepting_effective: boolean;
  last_ping_ms: number | null;
  load: LiveLoad | null;
  memory: LiveMemory | null;
  session_snapshot: LiveSnapshot | null;
  /** "sessions" | "empty" | "unavailable" | "unknown" | "missing". */
  display_state: string;
  /** Active hub jobs on this machine; null while no jobs data exists —
   *  an unknown count, never a fabricated 0. */
  active_jobs: number | null;
};

export type LiveLink = {
  task_id: number;
  job_id: string;
  job_found: boolean;
  /** One-hop owner_lane siblings — tester/worker jobs under the builder. */
  children: string[];
};

export type LiveMismatch = {
  /** "current" — fresh jobs list; "cached" — comparison ran on last-good jobs;
   *  "unavailable" — the hub never answered /v1/jobs, so both lists are empty
   *  by definition and must not be read as "no mismatch". */
  basis: "current" | "cached" | "unavailable";
  tasks_without_job: number[];
  jobs_without_task: string[];
};

export type LiveResponse = {
  generated_at: string;
  jobs: { status: LiveSectionStatus; fetched_at: string; items: LiveJob[] };
  nodes: { status: LiveSectionStatus; fetched_at: string; items: LiveNode[] };
  links: LiveLink[];
  mismatch: LiveMismatch;
  tasks_truncated?: boolean;
};

/** Task states that display live chips: claimed · in_progress · verifying. */
export const LIVE_TASK_STATES: ReadonlySet<string> = new Set(["claimed", "in_progress", "verifying"]);

/** A whole-request failure synthesizes failed sections — distinct from every
 * server-reported status and never an empty "ok". */
export function liveRequestFailed(): LiveResponse {
  const section = { status: "request_failed" as LiveSectionStatus, fetched_at: "", items: [] };
  return {
    generated_at: "",
    jobs: { ...section, items: [] },
    nodes: { ...section, items: [] },
    links: [],
    mismatch: { basis: "unavailable", tasks_without_job: [], jobs_without_task: [] },
  };
}

export function liveSectionLabel(status: LiveSectionStatus): string {
  switch (status) {
    case "ok":
      return "정상";
    case "timeout":
      return "허브 응답 시간 초과";
    case "auth_failed":
      return "허브 인증 실패";
    case "unsupported":
      return "허브가 /v1/jobs를 지원하지 않음";
    case "unconfigured":
      return "허브 미설정";
    case "request_failed":
      return "조회 요청 실패";
    default:
      return "허브 오류";
  }
}

/** Relative age for job chips — minute resolution (jobs live for minutes).
 * Returns null for absent/unparseable timestamps; callers render that as an
 * explicit unknown, never as 0. */
export function liveAgeLabel(nowIso: string, iso: string | null | undefined): string | null {
  if (iso === null || iso === undefined || iso === "") {
    return null;
  }
  const ms = Date.parse(nowIso) - Date.parse(iso);
  if (!Number.isFinite(ms)) {
    return null;
  }
  const clamped = Math.max(0, ms);
  if (clamped < 60_000) {
    return "방금";
  }
  if (clamped < 3_600_000) {
    return `${Math.floor(clamped / 60_000)}분`;
  }
  if (clamped < 86_400_000) {
    return `${Math.floor(clamped / 3_600_000)}시간`;
  }
  return `${Math.floor(clamped / 86_400_000)}일`;
}

/** Machine load: "load5 1.20 / 8 cpu". A nil load or nil load5 is an
 * unmeasured value — "미수집"/"측정 불가", never 0. */
export function loadLabel(load: LiveLoad | null): string {
  if (load === null) {
    return "미수집";
  }
  if (load.load5 === null) {
    return "측정 불가";
  }
  const cpu = load.ncpu === null ? "ncpu 미상" : `${load.ncpu} cpu`;
  return `load5 ${load.load5.toFixed(2)} / ${cpu}`;
}

/** Machine memory: "42.0% (memory_pressure)". vm_stat free-page percentages
 * are not usable memory (panewire checks.go) — rendered as 측정 불가 with the
 * source named, never as a percentage. */
export function memoryLabel(memory: LiveMemory | null): string {
  if (memory === null) {
    return "미수집";
  }
  const source = memory.source === "" ? "source 미상" : memory.source;
  if (memory.source === "vm_stat" || memory.free_pct === null) {
    return `측정 불가 (${source})`;
  }
  return `${memory.free_pct.toFixed(1)}% (${source})`;
}

export function jobsById(live: LiveResponse): Map<string, LiveJob> {
  const map = new Map<string, LiveJob>();
  for (const job of live.jobs.items) {
    map.set(job.job_id, job);
  }
  return map;
}

/** The jobs attached to one task: the task's own refs.job_id job plus its
 * one-hop owner_lane children (tester/worker jobs). `recorded` is false when
 * the task carries no refs.job_id at all — distinct from "recorded but the
 * hub lists no such job". */
export function taskLiveJobs(
  live: LiveResponse,
  taskId: number,
): { recorded: boolean; primary: LiveJob | null; children: LiveJob[] } {
  const link = live.links.find((entry) => entry.task_id === taskId);
  if (!link) {
    return { recorded: false, primary: null, children: [] };
  }
  const byId = jobsById(live);
  const children = link.children
    .map((id) => byId.get(id))
    .filter((job): job is LiveJob => job !== undefined);
  return { recorded: true, primary: byId.get(link.job_id) ?? null, children };
}

/** Session → job lookup by exact pane_id equality — the only session↔job key
 * the hub guarantees. */
export function jobForPane(live: LiveResponse, paneId: string): LiveJob | null {
  if (paneId === "") {
    return null;
  }
  return live.jobs.items.find((job) => job.pane === paneId) ?? null;
}

/** Job → task reverse link by exact job_id equality (no prefix matching). */
export function taskIdForJob(live: LiveResponse, jobId: string): number | null {
  const link = live.links.find((entry) => entry.job_id === jobId);
  if (link) {
    return link.task_id;
  }
  const parent = live.links.find((entry) => entry.children.includes(jobId));
  return parent ? parent.task_id : null;
}
