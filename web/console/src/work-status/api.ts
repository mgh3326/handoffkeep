import { getJSON } from "../shared/http";
import type { TaskSpawn, TaskSpawnStart } from "../shared/spawn";
import type {
  WorkStatusBacklogTask,
  WorkStatusCounts,
  WorkStatusInWorkTask,
  WorkStatusMovementDay,
  WorkStatusProject,
  WorkStatusQueuedTask,
  WorkStatusResponse,
  WorkStatusSummary,
} from "./types";

function fail(path: string, what: string): never {
  throw new Error(`malformed work-status payload: ${path} ${what}`);
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

function optionalText(o: Record<string, unknown>, key: string): string | undefined {
  const value = o[key];
  return typeof value === "string" ? value : undefined;
}

function nullableText(o: Record<string, unknown>, key: string): string | null {
  const value = o[key];
  if (value === null || value === undefined) return null;
  return typeof value === "string" ? value : null;
}

function num(o: Record<string, unknown>, key: string, path: string): number {
  const value = o[key];
  return typeof value === "number" && Number.isFinite(value)
    ? value
    : fail(`${path}.${key}`, "is not a number");
}

function parseCounts(raw: unknown, path: string): WorkStatusCounts {
  const o = object(raw, path);
  return {
    backlog: num(o, "backlog", path),
    in_work: num(o, "in_work", path),
    in_review: num(o, "in_review", path),
    waiting_deploy: num(o, "waiting_deploy", path),
    merged_no_deploy_project: num(o, "merged_no_deploy_project", path),
    merged_before_deploy: num(o, "merged_before_deploy", path),
    total_open: num(o, "total_open", path),
    bundle_parents: num(o, "bundle_parents", path),
  };
}

function parseSummary(raw: unknown, path: string): WorkStatusSummary {
  const o = object(raw, path);
  return {
    total_open: num(o, "total_open", path),
    backlog: num(o, "backlog", path),
    in_work: num(o, "in_work", path),
    in_review: num(o, "in_review", path),
    waiting_deploy: num(o, "waiting_deploy", path),
    merged_no_deploy_project: num(o, "merged_no_deploy_project", path),
    merged_before_deploy: num(o, "merged_before_deploy", path),
    bundle_parents: num(o, "bundle_parents", path),
  };
}

function parseProject(raw: unknown, path: string): WorkStatusProject {
  const o = object(raw, path);
  return {
    project: text(o, "project", path),
    counts: parseCounts(o.counts, `${path}.counts`),
  };
}

function parseInWork(raw: unknown, path: string): WorkStatusInWorkTask {
  const o = object(raw, path);
  return {
    id: num(o, "id", path),
    title: text(o, "title", path),
    project: nullableText(o, "project"),
    state: text(o, "state", path),
    job_id: optionalText(o, "job_id"),
    job_state: optionalText(o, "job_state"),
    host: optionalText(o, "host"),
    pr: optionalText(o, "pr"),
    since: nullableText(o, "since"),
    age_seconds: num(o, "age_seconds", path),
  };
}

function parseBacklog(raw: unknown, path: string): WorkStatusBacklogTask {
  const o = object(raw, path);
  return {
    id: num(o, "id", path),
    title: text(o, "title", path),
    project: nullableText(o, "project"),
    state: text(o, "state", path),
    priority: num(o, "priority", path),
    created_at: text(o, "created_at", path),
    parent_id: typeof o.parent_id === "number" ? o.parent_id : null,
    bundle_id: typeof o.bundle_id === "number" ? o.bundle_id : null,
    bundle_title: optionalText(o, "bundle_title"),
  };
}

function parseSpawnStart(raw: unknown, path: string): TaskSpawnStart | null {
  if (raw === null || raw === undefined) return null;
  const o = object(raw, path);
  return {
    task_id: num(o, "task_id", path),
    until: text(o, "until", path),
    task_state: text(o, "task_state", path),
    met: typeof o.met === "boolean" ? o.met : fail(`${path}.met`, "is not a boolean"),
  };
}

function parseSpawn(raw: unknown, path: string): TaskSpawn {
  const o = object(raw, path);
  return {
    request_id: text(o, "request_id", path),
    job: text(o, "job", path),
    role: text(o, "role", path),
    state: text(o, "state", path),
    reason: text(o, "reason", path),
    attempt: num(o, "attempt", path),
    host: text(o, "host", path),
    next_check_at: nullableText(o, "next_check_at"),
    expires_at: nullableText(o, "expires_at"),
    updated_at: text(o, "updated_at", path),
    start_condition: parseSpawnStart(o.start_condition, `${path}.start_condition`),
  };
}

function parseQueued(raw: unknown, path: string): WorkStatusQueuedTask {
  const o = object(raw, path);
  return {
    id: num(o, "id", path),
    title: text(o, "title", path),
    project: nullableText(o, "project"),
    state: text(o, "state", path),
    spawn: parseSpawn(o.spawn, `${path}.spawn`),
  };
}

function parseMovementDay(raw: unknown, path: string): WorkStatusMovementDay {
  const o = object(raw, path);
  return {
    date: text(o, "date", path),
    created: num(o, "created", path),
    closed: num(o, "closed", path),
  };
}

export function parseWorkStatus(raw: unknown): WorkStatusResponse {
  const root = object(raw, "root");
  const generatedAt = text(root, "generated_at", "root");
  const summary = parseSummary(root.summary, "root.summary");

  if (!Array.isArray(root.projects)) fail("root.projects", "is not an array");
  const projects = root.projects.map((p, i) => parseProject(p, `root.projects[${i}]`));

  if (!Array.isArray(root.in_work)) fail("root.in_work", "is not an array");
  const inWork = root.in_work.map((t, i) => parseInWork(t, `root.in_work[${i}]`));

  if (!Array.isArray(root.backlog)) fail("root.backlog", "is not an array");
  const backlog = root.backlog.map((t, i) => parseBacklog(t, `root.backlog[${i}]`));

  if (!Array.isArray(root.queued)) fail("root.queued", "is not an array");
  const queued = root.queued.map((t, i) => parseQueued(t, `root.queued[${i}]`));

  if (!Array.isArray(root.movement)) fail("root.movement", "is not an array");
  const movement = root.movement.map((m, i) => parseMovementDay(m, `root.movement[${i}]`));

  return {
    generated_at: generatedAt,
    summary,
    projects,
    in_work: inWork,
    backlog,
    queued,
    movement,
  };
}

export async function loadWorkStatus(): Promise<WorkStatusResponse> {
  const raw = await getJSON<unknown>("/ui/api/work-status");
  return parseWorkStatus(raw);
}
