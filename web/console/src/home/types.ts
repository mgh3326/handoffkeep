// Types for GET /ui/api/attention: what is waiting on the operator. The
// server builds it from the database alone (internal/ui/attention.go).

export type AttentionDecisionSource = "request" | "lane_event";

export type AttentionDecision = {
  source: AttentionDecisionSource;
  id: number;
  lane: string;
  question: string;
  question_ko?: string;
  question_ko_stale?: boolean;
  created_at: string;
  due_at: string | null;
  overdue: boolean;
};

export type AttentionTaskReason = "awaiting_decision" | "stalled";

export type AttentionTask = {
  id: number;
  title: string;
  title_ko?: string;
  title_ko_stale?: boolean;
  lane: string;
  state: string;
  reason: AttentionTaskReason;
  since: string;
};

export type AttentionDeploy = {
  service: string;
  task_id: number;
  title: string;
  pr: string;
  merged_at: string;
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
