import type { BoardComment, BoardCommentsResponse, BoardDetail, BoardDoc, BoardTask, BoardTasksResponse, PolicyResponse } from "./types";
import type { LiveResponse } from "../live";

const PAGE_LIMIT = 500;
const MAX_TASKS = 5000;
const MAX_COMMENTS = 2000;

/** HTTP failure with the status preserved — callers distinguish a missing
 * task (404 → "없음") from a transient error without parsing the message. */
export class HttpError extends Error {
  readonly status: number;
  constructor(status: number) {
    super(`request failed: ${status}`);
    this.name = "HttpError";
    this.status = status;
  }
}

async function getJSON<T>(path: string): Promise<T> {
  const response = await fetch(path);
  if (!response.ok) {
    throw new HttpError(response.status);
  }
  return (await response.json()) as T;
}

export type BoardData = {
  tasks: BoardTask[];
  states: string[];
  truncated: boolean;
  /** Server-side snapshot timestamp from the last page — the staleness "now". */
  generatedAt: string;
};

// Walks the after_id cursor until the server reports the set complete. If the
// client-side cap is hit first the board still renders, marked truncated.
export async function fetchBoardTasks(): Promise<BoardData> {
  const tasks: BoardTask[] = [];
  let states: string[] = [];
  let generatedAt = "";
  let afterID = 0;
  for (;;) {
    const params = new URLSearchParams({ limit: String(PAGE_LIMIT) });
    if (afterID > 0) {
      params.set("after_id", String(afterID));
    }
    const page = await getJSON<BoardTasksResponse>(`/ui/api/board/tasks?${params.toString()}`);
    states = page.states;
    generatedAt = page.generated_at;
    tasks.push(...page.tasks);
    if (!page.truncated) {
      return { tasks, states, truncated: false, generatedAt };
    }
    const next = page.next_after_id ?? 0;
    if (next <= 0 || tasks.length >= MAX_TASKS) {
      return { tasks, states, truncated: true, generatedAt };
    }
    if (next <= afterID) {
      // A cursor that does not advance would re-request the same page until
      // the cap — reject loudly instead of looping on a server violation.
      throw new Error(`non-advancing board cursor: after_id=${afterID} next_after_id=${next}`);
    }
    afterID = next;
  }
}

export function fetchTaskDetail(id: number): Promise<BoardDetail> {
  return getJSON<BoardDetail>(`/ui/api/board/tasks/${id}`);
}

/** The live jobs/machine aggregation (#598). Read-only; section statuses
 * inside the payload carry hub failures explicitly. */
export function fetchLive(): Promise<LiveResponse> {
  return getJSON<LiveResponse>("/ui/api/live");
}

/** Reads one hk document by key for the task overview. The key is the part of
 * body_doc before "#"; the section suffix never leaves the browser. */
export function fetchBoardDoc(key: string): Promise<BoardDoc> {
  return getJSON<BoardDoc>(`/ui/api/board/doc?key=${encodeURIComponent(key)}`);
}

export function fetchPolicyActive(): Promise<PolicyResponse> {
  return getJSON<PolicyResponse>("/ui/api/policy/active");
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
    const page = await getJSON<BoardCommentsResponse>(`/ui/api/board/tasks/${id}/comments${query}`);
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
  const value = document.querySelector<HTMLMetaElement>('meta[name="hk-csrf"]')?.content ?? "";
  return value === "" ? null : value;
}

/** Appends a comment through the console's form-write path — the same
 * authentication, origin and CSRF checks as the decision answers. Only the
 * body and the CSRF token are sent: the author is whoever is signed in. */
export async function postTaskComment(id: number, body: string, csrf: string): Promise<BoardComment> {
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
