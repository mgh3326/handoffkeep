import { getJSON } from "../shared/http";
import type {
  Attention,
  AttentionDecision,
  AttentionDeploy,
  AttentionTask,
} from "./types";

function fail(path: string, what: string): never {
  throw new Error(`malformed attention payload: ${path} ${what}`);
}

function object(value: unknown, path: string): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    return fail(path, "is not an object");
  }
  return value as Record<string, unknown>;
}

function text(o: Record<string, unknown>, key: string, path: string): string {
  const value = o[key];
  return typeof value === "string" ? value : fail(`${path}.${key}`, "is not a string");
}

function nullableText(o: Record<string, unknown>, key: string, path: string): string | null {
  const value = o[key];
  if (value === null) return null;
  return typeof value === "string" ? value : fail(`${path}.${key}`, "is not a string or null");
}

function count(o: Record<string, unknown>, key: string, path: string): number {
  const value = o[key];
  return typeof value === "number" && Number.isInteger(value) && value >= 0
    ? value
    : fail(`${path}.${key}`, "is not a count");
}

function id(o: Record<string, unknown>, key: string, path: string): number {
  const value = o[key];
  return typeof value === "number" && Number.isInteger(value) && value > 0
    ? value
    : fail(`${path}.${key}`, "is not an id");
}

function flag(o: Record<string, unknown>, key: string, path: string): boolean {
  const value = o[key];
  return typeof value === "boolean" ? value : fail(`${path}.${key}`, "is not a boolean");
}

function items<T>(
  o: Record<string, unknown>,
  path: string,
  parse: (item: Record<string, unknown>, at: string) => T,
): T[] {
  const value = o.items;
  if (!Array.isArray(value)) return fail(`${path}.items`, "is not an array");
  return value.map((item, index) => {
    const at = `${path}.items[${index}]`;
    return parse(object(item, at), at);
  });
}

function decision(o: Record<string, unknown>, at: string): AttentionDecision {
  const source = o.source;
  if (source !== "request" && source !== "lane_event") {
    return fail(`${at}.source`, "is not request or lane_event");
  }
  return {
    source,
    id: id(o, "id", at),
    lane: text(o, "lane", at),
    question: text(o, "question", at),
    question_ko: o.question_ko === undefined ? undefined : text(o, "question_ko", at),
    question_ko_stale: o.question_ko_stale === undefined ? undefined : flag(o, "question_ko_stale", at),
    created_at: text(o, "created_at", at),
    due_at: nullableText(o, "due_at", at),
    overdue: flag(o, "overdue", at),
  };
}

function task(o: Record<string, unknown>, at: string): AttentionTask {
  const reason = o.reason;
  if (reason !== "awaiting_decision" && reason !== "stalled") {
    return fail(`${at}.reason`, "is not awaiting_decision or stalled");
  }
  return {
    id: id(o, "id", at),
    title: text(o, "title", at),
    title_ko: o.title_ko === undefined ? undefined : text(o, "title_ko", at),
    title_ko_stale: o.title_ko_stale === undefined ? undefined : flag(o, "title_ko_stale", at),
    lane: text(o, "lane", at),
    state: text(o, "state", at),
    reason,
    since: text(o, "since", at),
  };
}

function deploy(o: Record<string, unknown>, at: string): AttentionDeploy {
  return {
    service: text(o, "service", at),
    task_id: id(o, "task_id", at),
    title: text(o, "title", at),
    title_ko: o.title_ko === undefined ? undefined : text(o, "title_ko", at),
    title_ko_stale: o.title_ko_stale === undefined ? undefined : flag(o, "title_ko_stale", at),
    pr: text(o, "pr", at),
    merged_at: text(o, "merged_at", at),
  };
}

/** Checks every field the screen reads and throws on a missing or mistyped
 * one, so a malformed answer is an error, never an empty list. */
export function parseAttention(raw: unknown): Attention {
  const root = object(raw, "attention");
  const decisions = object(root.decisions, "decisions");
  const tasks = object(root.tasks, "tasks");
  const deploys = object(root.deploys, "deploys");
  return {
    generated_at: text(root, "generated_at", "attention"),
    decisions: {
      count: count(decisions, "count", "decisions"),
      overdue: count(decisions, "overdue", "decisions"),
      items: items(decisions, "decisions", decision),
    },
    tasks: {
      count: count(tasks, "count", "tasks"),
      awaiting_decision: count(tasks, "awaiting_decision", "tasks"),
      stalled: count(tasks, "stalled", "tasks"),
      items: items(tasks, "tasks", task),
    },
    deploys: {
      count: count(deploys, "count", "deploys"),
      events_capped: flag(deploys, "events_capped", "deploys"),
      items: items(deploys, "deploys", deploy),
    },
  };
}

/** Module-level so usePoll sees one stable `load`. */
export async function loadAttention(): Promise<Attention> {
  return parseAttention(await getJSON<unknown>("/ui/api/attention"));
}
