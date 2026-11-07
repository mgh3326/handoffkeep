// The console's inbox triage writes (hk 3343): read/unread, done/undone and
// snooze for one item or a bulk selection, plus the undo of one earlier
// write. Same guards as the other operator writes: the header CSRF token from
// the shell's meta tag, a required idempotency key, and a strict response
// shape — a malformed answer is an error, never an empty result.

import { readCsrfToken } from "./boardApi";
import { getJSON } from "./http";

export type InboxItemState = {
  item_key: string;
  read: boolean;
  done: boolean;
  snoozed_until: string | null;
};

export type InboxFailure = { item_key: string; error: string };

/** Stored state plus whether a snooze has come due. */
export type InboxStateView = InboxItemState & { woken: boolean };

export type InboxWriteResult = {
  action: string;
  items: InboxItemState[];
  failed: InboxFailure[];
  skipped: number;
};

/** A refused or failed inbox write: the HTTP status and the server's error
 * code (empty when the response carried none). */
export class InboxWriteError extends Error {
  readonly status: number;
  readonly code: string;
  constructor(status: number, code: string) {
    super(`inbox write failed: ${status} ${code}`);
    this.name = "InboxWriteError";
    this.status = status;
    this.code = code;
  }
}

/** The Korean message for a refusal. The wire code itself is never shown. */
export function inboxWriteMessage(error: unknown): string {
  const code = error instanceof InboxWriteError ? error.code : "";
  switch (code) {
    case "invalid_snooze":
      return "다시 알림 시각이 올바르지 않습니다.";
    case "invalid_inbox_item":
      return "알 수 없는 항목이라 처리하지 못했습니다.";
    case "decision_not_open":
      return "이미 닫힌 결정이라 다시 알림을 걸지 못했습니다.";
    case "not_found":
      return "항목을 찾지 못했습니다.";
    case "missing_csrf":
    case "csrf_rejected":
      return "세션이 만료되었습니다. 새로고침한 뒤 다시 시도하세요.";
    case "origin_rejected":
      return "허용되지 않은 요청입니다.";
    case "operator_only":
      return "운영자만 사용할 수 있습니다.";
    default:
      return "적용하지 못했습니다.";
  }
}

function record(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function parseItem(raw: unknown): InboxItemState {
  if (
    !record(raw) ||
    typeof raw.item_key !== "string" ||
    typeof raw.read !== "boolean" ||
    typeof raw.done !== "boolean" ||
    !(raw.snoozed_until === null || typeof raw.snoozed_until === "string")
  )
    throw new Error("malformed inbox item");
  return {
    item_key: raw.item_key,
    read: raw.read,
    done: raw.done,
    snoozed_until: raw.snoozed_until,
  };
}

function parseFailure(raw: unknown): InboxFailure {
  if (!record(raw) || typeof raw.item_key !== "string" || typeof raw.error !== "string")
    throw new Error("malformed inbox failure");
  return { item_key: raw.item_key, error: raw.error };
}

async function post(path: string, body: unknown): Promise<unknown> {
  const csrf = readCsrfToken();
  if (!csrf) throw new InboxWriteError(0, "missing_csrf");
  let response: Response;
  try {
    response = await fetch(path, {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": csrf },
      body: JSON.stringify(body),
    });
  } catch {
    throw new InboxWriteError(0, "network");
  }
  if (!response.ok) {
    let code = "";
    try {
      const parsed: unknown = await response.json();
      if (record(parsed) && typeof parsed.error === "string") code = parsed.error;
    } catch {
      // an auth gate answers with an empty body; the status still says why
    }
    throw new InboxWriteError(response.status, code);
  }
  return (await response.json()) as unknown;
}

/** Applies one action to one or more items. eventKey is the idempotency key:
 * send the same key again and the server replays instead of writing twice. */
export async function writeInbox(
  action: string,
  items: string[],
  eventKey: string,
  until?: string,
): Promise<InboxWriteResult> {
  const body: Record<string, unknown> = { action, items, event_key: eventKey };
  if (until !== undefined) body.until = until;
  const raw = await post("/ui/api/inbox/state", body);
  if (
    !record(raw) ||
    typeof raw.action !== "string" ||
    !Array.isArray(raw.items) ||
    !Array.isArray(raw.failed) ||
    typeof raw.skipped !== "number"
  )
    throw new Error("malformed inbox response");
  return {
    action: raw.action,
    items: raw.items.map(parseItem),
    failed: raw.failed.map(parseFailure),
    skipped: raw.skipped,
  };
}

export type InboxUndoResult = { items: InboxItemState[]; conflicted: string[] };

/** Restores every item of the write sent with eventKey. An item a later
 * write moved is reported in conflicted and left as that write left it. */
export async function undoInbox(eventKey: string): Promise<InboxUndoResult> {
  const raw = await post("/ui/api/inbox/undo", { event_key: eventKey });
  if (!record(raw) || !Array.isArray(raw.items) || !Array.isArray(raw.conflicted) || !raw.conflicted.every(key => typeof key === "string"))
    throw new Error("malformed inbox undo response");
  return { items: raw.items.map(parseItem), conflicted: raw.conflicted };
}

/** The stored triage state of keys, for a page that lists tasks without the
 * attention document (Work). */
export async function fetchInboxStates(keys: string[]): Promise<Map<string, InboxStateView>> {
  const out = new Map<string, InboxStateView>();
  if (keys.length === 0) return out;
  const raw = await getJSON<unknown>(`/ui/api/inbox/state?keys=${encodeURIComponent(keys.join(","))}`);
  if (!record(raw) || !Array.isArray(raw.items)) throw new Error("malformed inbox states response");
  for (const item of raw.items) {
    const parsed = parseItem(item);
    if (!record(item) || typeof item.woken !== "boolean") throw new Error("malformed inbox state");
    out.set(parsed.item_key, { ...parsed, woken: item.woken });
  }
  return out;
}
