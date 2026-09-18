export type TaskRefs = {
  pr?: string;
  head_sha?: string;
  report_path?: string;
  job_id?: string;
};

export type BoardTask = {
  id: number;
  lane: string;
  parent_lane?: string;
  title: string;
  kind: string;
  state: string;
  priority: number;
  claimed_by?: string;
  created_by: string;
  created_at: string;
  updated_at: string;
  refs: TaskRefs;
};

export type BoardTasksResponse = {
  generated_at: string;
  states: string[];
  tasks: BoardTask[];
  next_after_id?: number;
  truncated: boolean;
};

export type BoardEvent = {
  id: number;
  from: string;
  to: string;
  by: string;
  note?: string;
  refs?: TaskRefs;
  at: string;
};

export type DwellSegment = {
  state: string;
  seconds: number;
  open: boolean;
};

export type ParticipantSegment = {
  role: string | null;
  model_id: string | null;
  reps: number;
  rounds: number | null;
  blockers_found: number | null;
  completed: number | null;
  input_tokens: number | null;
  output_tokens: number | null;
};

export type BoardParticipants = {
  task_ref: string;
  coverage: "collected" | "not_collected";
  segments: ParticipantSegment[];
};

export type BoardDetail = {
  task: BoardTask;
  events: BoardEvent[];
  dwell: DwellSegment[];
  linear: { issue_id: string; identifier: string } | null;
  participants: BoardParticipants;
};

export type PolicyItem = {
  key: string;
  title?: string;
  doc_url: string;
  exists: boolean;
};

export type PolicyResponse = {
  status: "ok" | "not_configured" | "invalid_pointer" | "manifest_missing" | "invalid_manifest";
  pointer_key: string;
  pointer_doc_url: string;
  manifest_key?: string;
  manifest_doc_url?: string;
  release?: string;
  items: PolicyItem[];
  truncated?: boolean;
};
