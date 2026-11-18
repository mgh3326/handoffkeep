import { getJSON } from "../shared/http";
import {
  WAIT_SECTION_KEYS,
  type WaitAnswer,
  type WaitBacklog,
  type WaitBacklogRow,
  type WaitRow,
  type WaitSection,
  type WaitSectionKey,
  type WaitsResponse,
} from "./types";

function fail(path: string, what: string): never {
  throw new Error(`malformed waits payload: ${path} ${what}`);
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

function optionalText(o: Record<string, unknown>, key: string, path: string): string | undefined {
  const value = o[key];
  if (value === undefined || value === null) return undefined;
  return typeof value === "string" ? value : fail(`${path}.${key}`, "is not a string");
}

function num(o: Record<string, unknown>, key: string, path: string): number {
  const value = o[key];
  return typeof value === "number" && Number.isFinite(value)
    ? value
    : fail(`${path}.${key}`, "is not a number");
}

function flag(o: Record<string, unknown>, key: string, path: string): boolean {
  const value = o[key];
  if (value === undefined) return false;
  return typeof value === "boolean" ? value : fail(`${path}.${key}`, "is not a boolean");
}

function rows(o: Record<string, unknown>, path: string): unknown[] {
  const value = o.rows;
  return Array.isArray(value) ? value : fail(`${path}.rows`, "is not an array");
}

function isSectionKey(value: string): value is WaitSectionKey {
  return (WAIT_SECTION_KEYS as readonly string[]).includes(value);
}

function parseAnswer(raw: unknown, path: string): WaitAnswer | undefined {
  if (raw === undefined || raw === null) return undefined;
  const o = object(raw, path);
  return {
    result: text(o, "result", path),
    words: text(o, "words", path),
    actor: text(o, "actor", path),
    at: text(o, "at", path),
  };
}

function parseRow(raw: unknown, path: string): WaitRow {
  const o = object(raw, path);
  return {
    key: text(o, "key", path),
    section: text(o, "section", path),
    source: text(o, "source", path),
    task_id: num(o, "task_id", path),
    title: text(o, "title", path),
    project: optionalText(o, "project", path),
    lane: text(o, "lane", path),
    state: text(o, "state", path),
    on_kind: optionalText(o, "on_kind", path),
    on_ref: optionalText(o, "on_ref", path),
    reason: text(o, "reason", path),
    since: text(o, "since", path),
    age_seconds: num(o, "age_seconds", path),
    next_action: text(o, "next_action", path),
    grounds_link: optionalText(o, "grounds_link", path),
    request_id: optionalText(o, "request_id", path),
    answer: parseAnswer(o.answer, `${path}.answer`),
  };
}

function parseSection(raw: unknown, path: string): WaitSection {
  const o = object(raw, path);
  const key = text(o, "key", path);
  if (!isSectionKey(key)) fail(`${path}.key`, `is not a known section: ${key}`);
  return {
    key,
    count: num(o, "count", path),
    truncated: flag(o, "truncated", path),
    rows: rows(o, path).map((r, i) => parseRow(r, `${path}.rows[${i}]`)),
  };
}

function parseBacklogRow(raw: unknown, path: string): WaitBacklogRow {
  const o = object(raw, path);
  return {
    task_id: num(o, "task_id", path),
    title: text(o, "title", path),
    lane: text(o, "lane", path),
    project: optionalText(o, "project", path),
    created_at: text(o, "created_at", path),
  };
}

function parseBacklog(raw: unknown, path: string): WaitBacklog {
  const o = object(raw, path);
  return {
    count: num(o, "count", path),
    truncated: flag(o, "truncated", path),
    rows: rows(o, path).map((r, i) => parseBacklogRow(r, `${path}.rows[${i}]`)),
  };
}

/** Checks the payload's shape and returns the sections in display order. A
 * malformed payload throws; it is never rendered as an empty result. */
export function parseWaits(raw: unknown): WaitsResponse {
  const root = object(raw, "root");
  const asOf = text(root, "as_of", "root");
  const section = optionalText(root, "section", "root");

  if (!Array.isArray(root.sections)) fail("root.sections", "is not an array");
  const parsed = root.sections.map((s, i) => parseSection(s, `root.sections[${i}]`));
  if (parsed.length !== WAIT_SECTION_KEYS.length) {
    fail("root.sections", `has ${parsed.length} entries, want ${WAIT_SECTION_KEYS.length}`);
  }
  const sections = WAIT_SECTION_KEYS.map((key) => {
    const found = parsed.filter((s) => s.key === key);
    if (found.length !== 1) fail("root.sections", `needs exactly one ${key}`);
    return found[0];
  });

  const backlog = parseBacklog(root.backlog, "root.backlog");

  return { as_of: asOf, section, sections, backlog };
}

export async function loadWaits(): Promise<WaitsResponse> {
  const raw = await getJSON<unknown>("/ui/api/waits");
  return parseWaits(raw);
}
