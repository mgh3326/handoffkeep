// Types for GET /ui/api/attention: what is waiting on the operator. The
// server builds it from the database alone (internal/ui/attention.go). `key`
// is the operator inbox item key; `read`/`woken` are the stored triage state
// (a snooze that came due unread is woken).

export type AttentionDecisionSource = "request" | "lane_event";

export type AttentionDecision = {
  key: string;
  source: AttentionDecisionSource;
  id: number;
  lane: string;
  question: string;
  created_at: string;
  due_at: string | null;
  overdue: boolean;
  read: boolean;
  woken: boolean;
};

export type AttentionTaskReason = "awaiting_decision" | "stalled";

export type AttentionTask = {
  key: string;
  id: number;
  title: string;
  lane: string;
  state: string;
  priority: number;
  reason: AttentionTaskReason;
  since: string;
  read: boolean;
  woken: boolean;
};

export type AttentionDeploy = {
  key: string;
  service: string;
  task_id: number;
  title: string;
  pr: string;
  merged_at: string;
  read: boolean;
  woken: boolean;
};

export type Attention = {
  generated_at: string;
  decisions: { count: number; overdue: number; items: AttentionDecision[] };
  tasks: {
    count: number;
    awaiting_decision: number;
    stalled: number;
    items: AttentionTask[];
  };
  deploys: { count: number; events_capped: boolean; items: AttentionDeploy[] };
};
