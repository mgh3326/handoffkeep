export type RecordState =
  | "open"
  | "overdue"
  | "uncleaned"
  | "answered"
  | "default_applied"
  | "withdrawn"
  | "superseded"
  | "unrecorded"
  | "unknown";
export type SubmitResult =
  | "answered"
  | "conflict"
  | "stale_revision"
  | "pending_confirmation"
  | "expired"
  | "superseded"
  | "unknown";
export type ScreenState =
  | "loading"
  | "editing"
  | "confirming"
  | "sending"
  | "unconfirmed"
  | "send_failed"
  | "read_failed"
  | "partial";
export type Tab = "open" | "overdue" | "answered" | "cleanup";
export type DecisionRecord = {
  id: number;
  source: string;
  source_ref: string;
  task_id: number | null;
  lane: string;
  question: string;
  due_at: string | null;
  overdue: boolean;
  status: string;
  request_state: string;
  request_id: string;
  revision: number;
  created_at: string;
  answered_at: string | null;
  superseded_by: string;
  superseded_record_id?: number | null;
  answer: string;
};
export type Option = { key: string; label: string; recommended?: boolean };
export type Resolution = {
  kind: string;
  option?: string;
  text?: string;
  receipt?: string;
};
export type Request = {
  id: string;
  revision: number;
  state: string;
  question: string;
  options: Option[];
  allow_free: boolean;
  default_action: string;
  default_option?: string;
  default_trigger?: string;
  due_at?: string;
  doc?: string;
  refs?: string[];
  superseded_by?: string;
  human_only: boolean;
  answer_policy: string;
  resolution?: Resolution;
};
export type Attempt = {
  id: number;
  channel: string;
  provenance: string;
  actor: string;
  result: string;
  revision_request_id: string;
  question_revision: number;
  received_at: string;
};
export type Detail = {
  generated_at: string;
  record: DecisionRecord;
  task: {
    id: number;
    title: string;
    state: string;
    lane: string;
    project?: string | null;
  } | null;
  request: Request | null;
  history: Request[];
  attempts: Attempt[];
  answerable: boolean;
  disabled_reason: string;
};
export type DecisionListData = {
  generated_at: string;
  tab: Tab;
  counts: { open: number; overdue: number };
  truncated: boolean;
  items: DecisionRecord[];
};
export type ResolveBody = {
  request_id: string;
  option: string;
  text: string;
  event_key: string;
};
