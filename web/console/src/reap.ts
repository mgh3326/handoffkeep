// Types and helpers for GET /ui/api/reap (internal/ui/reap.go, task #603
// stage 1). The console only displays cleanup candidates; nothing here can
// close a session.

export interface ReapRow {
  machine: string;
  pane_id: string;
  tab_id?: string;
  agent_name?: string;
  status: string;
  job_id: string;
  owner_lane?: string;
  role?: string;
  reason?: string;
  basis?: "job-terminal" | "task-terminal";
  terminal_kind?: string;
  terminal_at?: string;
  task_id?: number;
  task_state?: string;
}

export interface ReapNodeSummary {
  panes: number;
  no_job: number;
  candidate: number;
  builder_task_gate: number;
  held: number;
}

export interface ReapNode {
  machine_id: string;
  state: string;
  stale: boolean;
  received_at: string;
  generated_at: string;
  grace_seconds: number;
  observed: boolean;
  jobs_readable: boolean;
  truncated: boolean;
  summary: ReapNodeSummary;
}

export interface ReapResponse {
  generated_at: string;
  status: string;
  fetched_at: string;
  candidates: ReapRow[];
  held: ReapRow[];
  nodes: ReapNode[];
  tasks_truncated?: boolean;
}

/** A body is trusted only when it carries the reap envelope; anything else
 * (an error page, another endpoint's JSON) is a failed lookup, never an
 * empty candidate list. */
export function isReapResponse(value: unknown): value is ReapResponse {
  if (typeof value !== "object" || value === null) {
    return false;
  }
  const body = value as Record<string, unknown>;
  return typeof body.status === "string" && Array.isArray(body.candidates) && Array.isArray(body.held) && Array.isArray(body.nodes);
}

const STATUS_LABELS: Record<string, string> = {
  unsupported: "허브 미지원(구버전)",
  unconfigured: "허브 미설정",
  timeout: "시간 초과",
  auth_failed: "인증 실패",
  http_error: "허브 오류",
};

export function reapStatusLabel(status: string): string {
  return STATUS_LABELS[status] ?? status;
}

const REASON_LABELS: Record<string, string> = {
  protected: "보호 표시",
  "protected-role": "보호 역할",
  "label-unknown": "이름 미상",
  "label-mismatch": "label 불일치",
  "label-conflict": "claim·spawn label 불일치",
  "label-reused": "label 재사용",
  "pane-ambiguous": "pane 중복",
  "lane-route": "레인 라우트(상주)",
  "lanes-unreadable": "레인 확인 불가",
  "no-terminal-event": "job 진행 중",
  "within-grace": "유예 중",
  "task-open": "태스크 진행 중",
  "task-unlinked": "태스크 미연결",
  "task-ambiguous": "태스크 중복 연결",
  "task-within-grace": "태스크 유예 중",
  "tasks-truncated": "태스크 목록 잘림",
  "node-not-connected": "노드 끊김",
  "node-unobserved": "노드 관측 불가",
};

export function reapReasonLabel(reason: string | undefined): string {
  if (!reason) {
    return "";
  }
  return REASON_LABELS[reason] ?? reason;
}

/** Held rows grouped by reason label, largest group first. */
export function heldByReason(rows: ReapRow[]): Array<[string, number]> {
  const counts = new Map<string, number>();
  for (const row of rows) {
    const label = reapReasonLabel(row.reason) || "사유 미상";
    counts.set(label, (counts.get(label) ?? 0) + 1);
  }
  return [...counts.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
}
