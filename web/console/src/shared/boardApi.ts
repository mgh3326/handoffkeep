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
    (!("work_board" in value) || validWorkBoard(value.work_board))
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
