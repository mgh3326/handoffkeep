// Payload of GET /ui/api/waits: what open tasks are waiting on, in four
// sections, plus the ordinary backlog (tasks with no wait).

/** The four wait sections, in display order. */
export const WAIT_SECTION_KEYS = ["needs_operator", "failed_start", "waiting_on", "idle_started"] as const;
export type WaitSectionKey = (typeof WAIT_SECTION_KEYS)[number];

export interface WaitAnswer {
  result: string;
  words: string;
  actor: string;
  at: string;
}

/** One waiting item. `key` is the item id; the page links to it with ?item=. */
export interface WaitRow {
  key: string;
  section: string;
  source: string;
  /** 0 when the row has no task. */
  task_id: number;
  /** Task title, or the question when there is no task. */
  title: string;
  project?: string;
  /** The owner lane. */
  lane: string;
  state: string;
  /** operator | lane | task | pr | ci | external; absent for idle rows. */
  on_kind?: string;
  on_ref?: string;
  /** Short text: what is wanted. */
  reason: string;
  since: string;
  age_seconds: number;
  next_action: string;
  /** The task's console page. */
  grounds_link?: string;
  request_id?: string;
  answer?: WaitAnswer;
}

export interface WaitSection {
  key: WaitSectionKey;
  count: number;
  truncated: boolean;
  rows: WaitRow[];
}

export interface WaitBacklogRow {
  task_id: number;
  title: string;
  lane: string;
  project?: string;
  created_at: string;
}

export interface WaitBacklog {
  count: number;
  truncated: boolean;
  rows: WaitBacklogRow[];
}

export interface WaitsResponse {
  /** Read time, RFC 3339. */
  as_of: string;
  /** Present only when a section filter was requested. */
  section?: string;
  /** Always the four sections, in WAIT_SECTION_KEYS order. */
  sections: WaitSection[];
  backlog: WaitBacklog;
}
