import type {
  BoardWorkBoard,
  BoardWorkSession,
  BoardComment,
  BoardCommentsResponse,
  BoardDetail,
  BoardDoc,
  BoardTask,
  BoardTasksResponse,
} from "./boardTypes";
import { getJSON } from "./http";

export { HttpError } from "./http";

const PAGE_LIMIT = 500;
const MAX_TASKS = 5000;
const MAX_COMMENTS = 2000;

export type BoardData = {
  tasks: BoardTask[];
  states: string[];
  truncated: boolean;
  /** Server-side snapshot timestamp from the last page — the staleness "now". */
  generatedAt: string;
};

// Walks the after_id cursor until the server reports the set complete. If the
// client-side cap is hit first the board still renders, marked truncated.
export async function fetchBoardTasks(project?: string): Promise<BoardData> {
  const tasks: BoardTask[] = [];
  let states: string[] = [];
  let generatedAt = "";
  let afterID = 0;
  for (;;) {
    const params = new URLSearchParams({ limit: String(PAGE_LIMIT) });
    if (project) params.set("project", project);
    if (afterID > 0) {
      params.set("after_id", String(afterID));
    }
    const page = await getJSON<BoardTasksResponse>(
      `/ui/api/board/tasks?${params.toString()}`,
    );
    if (
      !page ||
      !Array.isArray(page.states) ||
      !page.states.every((s) => typeof s === "string") ||
      !Array.isArray(page.tasks) ||
      !page.tasks.every(validBoardTask) ||
      typeof page.generated_at !== "string" ||
      typeof page.truncated !== "boolean"
    )
      throw new Error("malformed board response");
    states = page.states;
    generatedAt = page.generated_at;
    tasks.push(...page.tasks);
    if (!page.truncated) {
      return { tasks, states, truncated: false, generatedAt };
    }
    const next = page.next_after_id ?? 0;
    if (!Number.isSafeInteger(next) || next <= afterID || page.tasks.length === 0) {
      // A cursor that does not advance would re-request the same page until
      // the cap — reject loudly instead of looping on a server violation.
      throw new Error(
        `non-advancing board cursor: after_id=${afterID} next_after_id=${next}`,
      );
    }
    if (tasks.length >= MAX_TASKS) return { tasks: tasks.slice(0, MAX_TASKS), states, truncated: true, generatedAt };
    afterID = next;
  }
}

export async function fetchTaskDetail(id: number): Promise<BoardDetail> {
  const data = await getJSON<BoardDetail>(`/ui/api/board/tasks/${id}`);
  if (
    !data ||
    !validBoardTask(data.task) ||
    !Array.isArray(data.events) ||
    !Array.isArray(data.dwell) ||
    !Array.isArray(data.documents) || !data.documents.every(d => record(d) && typeof d.key === "string" && typeof d.kind === "string" && typeof d.origin_class === "string") ||
    typeof data.documents_truncated !== "boolean" ||
    !Array.isArray(data.origins) || !data.origins.every(validOrigin) ||
    !Array.isArray(data.changes) || !data.changes.every(validChange) ||
    !Array.isArray(data.jobs) || !data.jobs.every(validJob) ||
    typeof data.origins_truncated !== "boolean" || typeof data.changes_truncated !== "boolean" || typeof data.jobs_truncated !== "boolean"
  )
    throw new Error("malformed task detail");
  return data;
}

/** Reads one hk document by key for the task overview. The key is the part of
 * body_doc before "#"; the section suffix never leaves the browser. */
export function fetchBoardDoc(key: string): Promise<BoardDoc> {
  return getJSON<BoardDoc>(`/ui/api/board/doc?key=${encodeURIComponent(key)}`);
}

export type TaskComments = { comments: BoardComment[]; truncated: boolean };

/** Reads a task's comments in creation order, walking the after_id cursor.
 * Past the client cap the list is returned marked truncated — never as if it
 * were complete. A missing task rejects with HttpError(404). */
export async function fetchTaskComments(id: number): Promise<TaskComments> {
  const comments: BoardComment[] = [];
  let afterID = 0;
  for (;;) {
    const query = afterID > 0 ? `?after_id=${afterID}` : "";
    const page = await getJSON<BoardCommentsResponse>(
      `/ui/api/board/tasks/${id}/comments${query}`,
    );
    comments.push(...page.comments);
    if (!page.truncated) {
      return { comments, truncated: false };
    }
    const next = page.next_after_id ?? 0;
    if (next <= afterID || comments.length >= MAX_COMMENTS) {
      return { comments, truncated: true };
    }
    afterID = next;
  }
}

/** A refused or failed comment write: the HTTP status and the server's error
 * code (empty when the response carried none, e.g. an auth gate's bare 401). */
export class CommentWriteError extends Error {
  readonly status: number;
  readonly code: string;
  constructor(status: number, code: string) {
    super(`comment write failed: ${status} ${code}`);
    this.name = "CommentWriteError";
    this.status = status;
    this.code = code;
  }
}

/** The session CSRF token the queue page carries for its one write form. The
 * cookie it pairs with is HttpOnly, so the page hands the token over in a
 * meta tag; null when the page has none. */
export function readCsrfToken(): string | null {
  const value =
    document.querySelector<HTMLMetaElement>('meta[name="hk-csrf"]')?.content ??
    "";
  return value === "" ? null : value;
}

/** Appends a comment through the console's form-write path — the same
 * authentication, origin and CSRF checks as the decision answers. Only the
 * body and the CSRF token are sent: the author is whoever is signed in. */
export async function postTaskComment(
  id: number,
  body: string,
  csrf: string,
): Promise<BoardComment> {
  let response: Response;
  try {
    response = await fetch(`/ui/tasks/${id}/comments`, {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams({ body, csrf }).toString(),
    });
  } catch {
    throw new CommentWriteError(0, "network");
  }
  if (response.status === 201) {
    return (await response.json()) as BoardComment;
  }
  let code = "";
  try {
    const parsed = (await response.json()) as { error?: unknown };
    code = typeof parsed.error === "string" ? parsed.error : "";
  } catch {
    // an auth gate answers with an empty body; the status still says why
  }
  throw new CommentWriteError(response.status, code);
}

function validBoardTask(value: unknown): value is BoardTask {
  if (typeof value !== "object" || value === null) return false;
  return (
    "id" in value &&
    typeof value.id === "number" &&
    Number.isSafeInteger(value.id) &&
    value.id > 0 &&
    "title" in value &&
    typeof value.title === "string" &&
    "lane" in value &&
    typeof value.lane === "string" &&
    "state" in value &&
    typeof value.state === "string" &&
    "priority" in value &&
    typeof value.priority === "number" &&
    "kind" in value &&
    typeof value.kind === "string" &&
    "created_by" in value &&
    typeof value.created_by === "string" &&
    "created_at" in value &&
    typeof value.created_at === "string" &&
    "updated_at" in value &&
    typeof value.updated_at === "string" &&
    "refs" in value &&
    typeof value.refs === "object" &&
    value.refs !== null &&
    (!("closure" in value.refs) || (record(value.refs.closure) && typeof value.refs.closure.type === "string" && typeof value.refs.closure.evidence === "string")) &&
    (!("work_board" in value) || validWorkBoard(value.work_board)) &&
    (!("active_job" in value) || (record(value.active_job) && [value.active_job.role, value.active_job.model, value.active_job.profile, value.active_job.host].every(v => typeof v === "string") && [value.active_job.harness, value.active_job.effort].every(v => v === null || typeof v === "string")))
  );
}

function validWorkBoard(value: unknown): value is BoardWorkBoard {
  if (typeof value !== "object" || value === null) return false;
  return (
    "primary_job_id" in value &&
    typeof value.primary_job_id === "string" &&
    "pr" in value &&
    typeof value.pr === "string" &&
    "head_sha" in value &&
    typeof value.head_sha === "string" &&
    "sessions" in value &&
    Array.isArray(value.sessions) &&
    value.sessions.every(validWorkSession)
  );
}
function validWorkSession(value: unknown): value is BoardWorkSession {
  if (typeof value !== "object" || value === null) return false;
  return (
    "job_id" in value &&
    typeof value.job_id === "string" &&
    "host" in value &&
    typeof value.host === "string" &&
    "family" in value &&
    typeof value.family === "string" &&
    "model" in value &&
    typeof value.model === "string" &&
    "role" in value &&
    typeof value.role === "string" &&
    "active" in value &&
    typeof value.active === "boolean" &&
    "at" in value &&
    typeof value.at === "string"
  );
}

export async function fetchProjects(): Promise<string[]> {
  const data = await getJSON<{ projects: unknown }>("/ui/api/board/projects");
  if (!data || !Array.isArray(data.projects) || !data.projects.every((p) => typeof p === "string")) throw new Error("malformed projects");
  return data.projects;
}

function record(value: unknown): value is Record<string, unknown> { return typeof value === "object" && value !== null && !Array.isArray(value); }
function optionalString(value: unknown): boolean { return value === undefined || value === null || typeof value === "string"; }
function optionalNumber(value: unknown): boolean { return value === undefined || value === null || typeof value === "number"; }
function validOrigin(value: unknown): boolean {
  return record(value) && typeof value.provider === "string" && typeof value.namespace === "string" && optionalString(value.display_ref) && optionalString(value.url);
}
function validReview(value: unknown): boolean {
  return record(value) && typeof value.id === "number" && typeof value.lens === "string" && typeof value.round === "number" && typeof value.pass === "number" && optionalString(value.family) && optionalString(value.model) && optionalString(value.verdict) && optionalNumber(value.critical) && optionalNumber(value.high) && optionalNumber(value.medium) && optionalNumber(value.low);
}
function validChange(value: unknown): boolean {
  return record(value) && typeof value.id === "number" && typeof value.pr === "string" && typeof value.state === "string" && optionalString(value.head_sha) && optionalString(value.merged_at) && optionalString(value.hold_reason) && optionalNumber(value.depends_on_change_id) && optionalString(value.depends_on_state) && (value.tasks === null || Array.isArray(value.tasks)) && (value.reviews === null || (Array.isArray(value.reviews) && value.reviews.every(validReview)));
}
function validJob(value: unknown): boolean {
  if (!record(value)) return false;
  const diff = value.diff_stat;
  return typeof value.job_id === "string" && typeof value.role === "string" && typeof value.state === "string" && typeof value.host === "string" && typeof value.family === "string" && typeof value.profile === "string" && typeof value.model === "string" && optionalString(value.harness) && optionalString(value.effort) && optionalString(value.claimed_at) && optionalString(value.spawned_at) && optionalString(value.ready_at) && optionalString(value.ended_at) && optionalString(value.end_reason) && (diff === null || (record(diff) && typeof diff.files === "number" && typeof diff.insertions === "number" && typeof diff.deletions === "number")) && (value.skills_supported === null || typeof value.skills_supported === "boolean") && (value.skills === null || (Array.isArray(value.skills) && value.skills.every(s => record(s) && typeof s.name === "string" && typeof s.calls === "number")));
}

/** A refused or failed lifecycle write: the HTTP status and the server's
 * error code. taskActionMessage turns the code into the Korean toast. */
export class TaskActionError extends Error {
  readonly status: number;
  readonly code: string;
  constructor(status: number, code: string) {
    super(`task action failed: ${status} ${code}`);
    this.name = "TaskActionError";
    this.status = status;
    this.code = code;
  }
}

const TASK_ACTION_MESSAGES: Record<string, string> = {
  operator_only: "운영자만 실행할 수 있습니다.",
  origin_rejected: "다른 출처의 요청이라 거부했습니다.",
  csrf_rejected: "보안 토큰이 만료되었습니다. 새로고침한 뒤 다시 시도하세요.",
  event_key_required: "요청 키가 없습니다. 다시 시도하세요.",
  invalid_request: "입력값을 확인하세요.",
  task_merge_path: "이 상태에서는 머지됨으로 닫을 수 없습니다.",
  task_conflict: "작업 상태가 바뀌어 실행할 수 없습니다. 새로고침한 뒤 다시 시도하세요.",
  task_terminal: "종결된 작업은 변경할 수 없습니다.",
  closure_invalid: "종료 근거가 올바르지 않습니다 (https URL 또는 hk:doc 키).",
  duplicate_of_required: "중복 대상 작업 번호를 입력하세요.",
  unknown_lane: "알 수 없는 레인입니다.",
  disposition_operator_only: "처분 항목은 콘솔에서 직접 변경할 수 없습니다.",
  decision_request_open: "열린 결정 요청을 먼저 해결하세요.",
  task_job_required: "작업 연결이 없어 실행할 수 없습니다.",
  secret_like_content: "비밀 정보로 보이는 내용은 저장할 수 없습니다.",
  not_found: "작업을 찾을 수 없습니다.",
  relay_failed: "레인은 옮겼지만 레인 알림을 보내지 못했습니다.",
  missing_csrf: "보안 토큰이 없습니다. 새로고침한 뒤 다시 시도하세요.",
  comment_empty: "코멘트 내용이 필요합니다.",
  comment_too_long: "코멘트가 너무 깁니다.",
  comment_invalid: "코멘트를 저장할 수 없습니다.",
  internal_error: "저장하지 못했습니다. 잠시 후 다시 시도하세요.",
};

/** The Korean operator-facing message for a lifecycle write refusal. */
export function taskActionMessage(code: string): string {
  return TASK_ACTION_MESSAGES[code] ?? "저장하지 못했습니다.";
}

/** A fresh idempotency key for one user intent. randomUUID needs a secure
 * context; the capture harness serves the bundle over plain http, so fall
 * back to random hex there. */
export function newEventKey(): string {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") return crypto.randomUUID();
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, byte => byte.toString(16).padStart(2, "0")).join("");
}

/** eventKey is minted once per user intent (when its dialog opens), so a
 * retry of a request whose response was lost replays the recorded write. */
async function postTaskAction<T>(id: number, action: string, body: Record<string, unknown>, eventKey: string): Promise<T> {
  const csrf = readCsrfToken();
  if (!csrf) throw new TaskActionError(0, "missing_csrf");
  let response: Response;
  try {
    response = await fetch(`/ui/api/tasks/${id}/${action}`, {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf },
      body: JSON.stringify({ ...body, event_key: eventKey }),
    });
  } catch {
    throw new TaskActionError(0, "network");
  }
  const data: unknown = await response.json().catch(() => null);
  if (!response.ok) {
    const code = record(data) && typeof data.error === "string" ? data.error : "";
    throw new TaskActionError(response.status, code);
  }
  return data as T;
}

export type CloseMergedResult = { state: string; path: string[] };
export function closeTaskMerged(id: number, evidence: string, reason: string, eventKey: string): Promise<CloseMergedResult> {
  return postTaskAction<CloseMergedResult>(id, "close-merged", { evidence, reason }, eventKey);
}
export function closeTaskCompleted(id: number, evidence: string, reason: string, eventKey: string): Promise<{ state: string }> {
  return postTaskAction<{ state: string }>(id, "close-completed", { evidence, reason }, eventKey);
}
export type DropClosure = "cancelled" | "obsolete" | "superseded";
export function dropTask(id: number, closure: DropClosure, reason: string, duplicateOf: number, eventKey: string): Promise<{ state: string; closure_type: string }> {
  return postTaskAction<{ state: string; closure_type: string }>(id, "drop", { closure, reason, duplicate_of: duplicateOf }, eventKey);
}
export function holdTask(id: number, held: boolean, to: string, eventKey: string): Promise<{ previous: string; state: string }> {
  return postTaskAction<{ previous: string; state: string }>(id, "hold", { held, to }, eventKey);
}
export function reopenTask(id: number, reason: string, eventKey: string): Promise<{ state: string }> {
  return postTaskAction<{ state: string }>(id, "reopen", { reason }, eventKey);
}
export type RelaneOutcome = { previous: string; lane: string; changed: boolean; relay: "sent" | "duplicate" | "unconfigured" | "failed" | "none" };
/** fromKey names the relane this write undoes; the store lets that undo
 * restore the lane the recorded move emptied. */
export function relaneTask(id: number, lane: string, reason: string, eventKey: string, fromKey = ""): Promise<RelaneOutcome> {
  return postTaskAction<RelaneOutcome>(id, "relane", { lane, reason, from_key: fromKey }, eventKey);
}

/** Every lane a relane may target (the store's known-lane union). */
export async function fetchLanes(): Promise<string[]> {
  const data = await getJSON<{ lanes: unknown }>("/ui/api/board/lanes");
  if (!data || !Array.isArray(data.lanes) || !data.lanes.every(lane => typeof lane === "string")) throw new Error("malformed lanes");
  return data.lanes;
}

type PriorityResult = { previous: number; priority: number };
/** eventKey is minted once per change; a replay with the same key returns the
 * recorded write instead of changing the priority twice. */
export async function setTaskPriority(id: number, priority: number, eventKey = newEventKey()): Promise<PriorityResult> {
  const csrf = readCsrfToken();
  if (!csrf) throw new Error("missing csrf");
  const response = await fetch(`/ui/api/tasks/${id}/priority`, { method: "POST", headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf }, body: JSON.stringify({ priority, event_key: eventKey }) });
  const data: unknown = await response.json().catch(() => null);
  if (!response.ok) {
    const code = record(data) && typeof data.error === "string" ? data.error : "";
    throw new TaskActionError(response.status, code);
  }
  if (typeof data !== "object" || data === null || !("previous" in data) || typeof data.previous !== "number" || !("priority" in data) || typeof data.priority !== "number") throw new TaskActionError(0, "malformed_response");
  return { previous: data.previous, priority: data.priority };
}
