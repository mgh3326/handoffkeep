export interface WorkStatusCounts {
  backlog: number;
  in_work: number;
  in_review: number;
  waiting_deploy: number;
  merged_no_deploy_record: number;
  total_open: number;
  bundle_parents: number;
}

export interface WorkStatusProject {
  project: string;
  counts: WorkStatusCounts;
}

export interface WorkStatusSummary {
  total_open: number;
  backlog: number;
  in_work: number;
  in_review: number;
  waiting_deploy: number;
  merged_no_deploy_record: number;
  bundle_parents: number;
}

export interface WorkStatusInWorkTask {
  id: number;
  title: string;
  project?: string | null;
  state: string;
  job_id?: string | null;
  host?: string | null;
  pr?: string | null;
  since?: string | null;
  age_seconds: number;
}

export interface WorkStatusBacklogTask {
  id: number;
  title: string;
  project?: string | null;
  state: string;
  priority: number;
  created_at: string;
  parent_id?: number | null;
  bundle_id?: number | null;
  bundle_title?: string | null;
}

export interface WorkStatusMovementDay {
  date: string;
  created: number;
  closed: number;
}

export interface WorkStatusResponse {
  generated_at: string;
  summary: WorkStatusSummary;
  projects: WorkStatusProject[];
  in_work: WorkStatusInWorkTask[];
  backlog: WorkStatusBacklogTask[];
  movement: WorkStatusMovementDay[];
}
