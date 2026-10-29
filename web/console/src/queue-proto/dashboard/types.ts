// /ui/api/dashboard response — mirrors internal/ui/dashboard.go. Hosts and
// hub jobs are not here: the page reads them from /ui/api/live (../../live).

export type DashboardBucket = {
  window: string;
  used_pct: number;
  resets_at?: string;
  scope: string;
  model?: string;
  /** Share of the window elapsed when the bucket was measured; null when the
   *  window has no fixed length or no reset time. */
  elapsed_pct: number | null;
  /** used_pct / elapsed_pct — 1.0 reaches 100% exactly at the reset. */
  pace: number | null;
  /** The window has reset since this measurement. */
  reset_passed?: boolean;
};

export type DashboardManual = {
  window: string;
  measured_at: string;
  expires_at?: string;
  expired: boolean;
  used_pct: number | null;
  resets_at?: string;
};

export type DashboardAccount = {
  account_key: string;
  pool: string;
  auto: {
    measured_at: string;
    confirmed_at?: string;
    plan?: string;
    buckets: DashboardBucket[];
  } | null;
  manual: DashboardManual[];
};

export type DashboardTask = {
  id: number;
  title: string;
  lane: string;
  state: string;
  claimed_by?: string;
  job_id?: string;
  tier?: string;
  pr?: string;
  updated_at: string;
};

export type DashboardPR = {
  pr: string;
  number?: number;
  task_id: number;
  title: string;
  state: string;
  lane?: string;
  head_sha?: string;
  at: string;
  merged_at?: string;
};

export type DashboardRepo = {
  /** "" groups refs.pr values that are not a GitHub pull URL. */
  repo: string;
  open: DashboardPR[];
  merged: DashboardPR[];
};

export type DashboardMissing = { section: string; source: string; note: string };

export type DashboardResponse = {
  generated_at: string;
  quota: DashboardAccount[];
  quota_truncated?: boolean;
  work: DashboardTask[];
  prs: DashboardRepo[];
  tasks_truncated?: boolean;
  missing: DashboardMissing[];
};
