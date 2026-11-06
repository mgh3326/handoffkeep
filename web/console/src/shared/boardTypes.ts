export type TaskRefs = {
  closure?: { type: string; evidence: string };
  pr?: string;
  head_sha?: string;
  report_path?: string;
  job_id?: string;
  /** The task's current structured decision request (#618). The list only
   * needs its identity and status; the drawer reads the full request and
   * its history from the detail's decision_requests. */
  decision_request?: DecisionRequestRef;
};

/** status is the stored request status; "open" is the only one the queue
 * counts (pending on live tasks, uncleaned on merged/dropped ones). */
export type DecisionRequestRef = {
  id: string;
  revision: number;
  status: "open" | "answered" | "default_applied" | "withdrawn" | string;
};

export type DecisionOption = { key: string; label: string; recommended?: boolean };

export type DecisionResolution = {
  kind: "answered" | "default_applied" | "withdrawn" | string;
  option?: string;
  text?: string;
  receipt?: string;
  responder?: string;
  by: string;
  at: string;
};

/** One request as the detail BFF derives it. state is computed server-side:
 * overdue = deadline passed with no application recorded; uncleaned = open
 * on a merged/dropped task. The console never re-derives it. */
export type DecisionRequestView = {
  id: string;
  revision: number;
  supersedes?: string;
  superseded_by?: string;
  current: boolean;
  state: "open" | "overdue" | "uncleaned" | "answered" | "default_applied" | "withdrawn" | "superseded" | string;
  state_label: string;
  question: string;
  options: DecisionOption[];
  allow_free: boolean;
  reason?: string;
  default_action: string;
  default_option?: string;
  default_trigger?: string;
  due_at?: string;
  doc?: string;
  requested_by: string;
  requested_at: string;
  resolution?: DecisionResolution;
};

/** needs_decision with no open request: the question exists only as a
 * transition note (state "unrecorded", shown as 미기록). */
export type LegacyDecisionView = {
  state: string;
  state_label: string;
  question: string;
  options: DecisionOption[];
  allow_free: boolean;
};

export type BoardWorkSession = {
  job_id: string;
  host: string;
  family: string;
  model: string;
  role: string;
  active: boolean;
  at: string;
};
export type BoardWorkBoard = {
  primary_job_id: string;
  pr: string;
  head_sha: string;
  sessions: BoardWorkSession[];
};
/** One child task as the detail BFF projects it. */
export type TaskChildRef = {
  id: number;
  title: string;
  state: string;
  category: string;
};

/** One inter-task relation from /ui/api/board/tasks/<id>. */
export type TaskRelation = {
  id: number;
  /** "blocks": src blocks dst; "duplicate_of": src is a duplicate of dst;
   * "related": symmetric association. */
  kind: "blocks" | "duplicate_of" | "related";
  src_id: number;
  dst_id: number;
  created_by: string;
  created_at: string;
};

export type ActiveJob = { role: string; model: string; profile: string; host: string; harness: string | null; effort: string | null };

export type BoardTask = {
  active_job?: ActiveJob;
  /** Console-safe projection of the durable board and recorded placements.
   * Absent on old servers and when no execution/PR data is recorded. */
  work_board?: BoardWorkBoard;
  id: number;
  lane: string;
  parent_lane?: string;
  title: string;
  kind: string;
  state: string;
  /** Server-derived state category (triage/backlog/unstarted/started/completed/canceled).
   * Absent on pre-hierarchy servers. */
  category?: string;
  /** true when the task is awaiting an operator decision — orthogonal to
   * the work state. Absent on pre-hierarchy servers (treat as false). */
  awaiting_decision?: boolean;
  /** Parent task id; null when no parent; absent on pre-hierarchy servers. */
  parent_id?: number | null;
  priority: number;
  claimed_by?: string;
  created_by: string;
  created_at: string;
  updated_at: string;
  /** ISO timestamp when the task entered the started category; null/absent when not yet started. */
  started_at?: string | null;
  /** ISO timestamp when the task completed; null/absent when not completed. */
  completed_at?: string | null;
  /** ISO timestamp when the task was canceled; null/absent when not canceled. */
  canceled_at?: string | null;
  /** ISO timestamp when the task entered triage; null/absent when not triaged. */
  triaged_at?: string | null;
  /** ISO timestamp when triage was started; null/absent when not entered. */
  started_triage_at?: string | null;
  refs: TaskRefs;
  /** hk document key holding the task body: "key" or the transitional
   * "key#section". Absent when no body document is attached. */
  body_doc?: string;
  /** Server-configured project classification. null on legacy rows that
   * predate the field; absent on pre-field servers — both render as
   * "no project", never as an empty string. */
  project?: string | null;
  /** Routing tag from the server's closed area vocabulary. null when unset;
   * absent on servers that predate the field. Read-only in the console. */
  area?: string | null;
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
  /** "transition" rows change state; "relane" rows move the task between
   * lanes and from/to carry lane names; "project" rows reclassify the task
   * and from/to carry project names. Absent on pre-relane servers.
   * New kinds: state_migration, awaiting_decision, parent, relation, auto_close. */
  kind?: "transition" | "relane" | "decision" | "project" | "area"
       | "metadata" | "source_event" | "state_migration" | "awaiting_decision" | "parent" | "relation" | "auto_close";
  from: string;
  to: string;
  by: string;
  note?: string;
  /** Recorded reason a researching/developing transition ran without a job
   * link (#769). Absent on pre-change servers and on linked events. */
  no_job?: string;
  refs?: TaskRefs;
  /** Source system that produced this event (e.g. "linear"). Absent on manual events. */
  source?: string;
  /** External reference in the source system. Absent when not applicable. */
  source_ref?: string;
  /** Field changed for awaiting_decision events. */
  field?: string;
  /** Previous value for field-change events. */
  from_value?: string;
  /** New value for field-change events. */
  to_value?: string;
  /** true when the event was applied automatically (e.g. auto_close). */
  auto?: boolean;
  /** true when a default was applied. */
  applied?: boolean;
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
  truncated?: boolean;
  segments: ParticipantSegment[];
};

export type WorkOrigin = { provider: string; display_ref?: string; url?: string; namespace: string };
export type WorkReview = { id: number; lens: string; round: number; pass: number; family?: string; model?: string; verdict?: string; critical?: number | null; high?: number | null; medium?: number | null; low?: number | null };
export type WorkChange = { id: number; pr: string; state: string; head_sha?: string; merged_at?: string; hold_reason?: string; depends_on_change_id?: number; depends_on_state?: string; reviews: WorkReview[] | null; tasks: unknown[] | null };
export type WorkJob = { job_id: string; role: string; state: string; host: string; family: string; model: string; harness: string | null; effort: string | null; profile: string; skills: { name: string; calls: number }[] | null; skills_supported: boolean | null; claimed_at: string | null; spawned_at: string | null; ready_at: string | null; ended_at: string | null; end_reason: string | null; diff_stat: { files: number; insertions: number; deletions: number } | null };
export type WorkDocument = { key: string; kind: string; origin_class: string };
export type BoardDetail = {
  documents: WorkDocument[];
  documents_truncated: boolean;
  origins: WorkOrigin[];
  changes: WorkChange[];
  jobs: WorkJob[];
  origins_truncated: boolean;
  changes_truncated: boolean;
  jobs_truncated: boolean;
  task: BoardTask;
  events: BoardEvent[];
  dwell: DwellSegment[];
  linear: { issue_id: string; identifier: string } | null;
  participants: BoardParticipants;
  /** Absent on servers before #618 — "not provided", never "none". */
  decision_requests?: DecisionRequestView[];
  decision_legacy?: LegacyDecisionView | null;
  /** Child tasks; absent when none or on pre-hierarchy servers. */
  children?: TaskChildRef[];
  /** Inter-task relations; absent when none or on pre-hierarchy servers. */
  relations?: TaskRelation[];
};

/** GET /ui/api/board/doc — one hk document by exact key. Only "markdown"
 * may reach the renderer; "unsupported" is shown as raw text only. */
export type BoardDoc = {
  key: string;
  kind: string;
  sha256: string;
  updated_at: string;
  bytes: number;
  format: "markdown" | "unsupported";
  reason?: "not_text" | "too_large" | "json";
  body: string;
};

/** One append-only task comment from /ui/api/board/tasks/<id>/comments.
 * author is the authenticated writer recorded by the server. */
export type BoardComment = {
  id: number;
  task_id: number;
  body: string;
  author: string;
  created_at: string;
};

export type BoardCommentsResponse = {
  comments: BoardComment[];
  truncated: boolean;
  next_after_id?: number;
};
