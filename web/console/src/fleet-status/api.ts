import { getJSON } from "../shared/http";
import type {
  FleetStatusHost,
  FleetStatusPlan,
  FleetStatusQuota,
  FleetStatusResponse,
  FleetStatusSnapshot,
  FleetStatusWindow,
} from "./types";

function fail(path: string, what: string): never {
  throw new Error(`malformed fleet-status payload: ${path} ${what}`);
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

function num(o: Record<string, unknown>, key: string, path: string): number {
  const value = o[key];
  return typeof value === "number" && Number.isFinite(value)
    ? value
    : fail(`${path}.${key}`, "is not a number");
}

function bool(o: Record<string, unknown>, key: string, path: string): boolean {
  const value = o[key];
  return typeof value === "boolean" ? value : fail(`${path}.${key}`, "is not a boolean");
}

function nullableNum(o: Record<string, unknown>, key: string, path: string): number | null {
  const value = o[key];
  if (value === null || value === undefined) return null;
  return typeof value === "number" && Number.isFinite(value)
    ? value
    : fail(`${path}.${key}`, "is not a number");
}

function list<T>(value: unknown, path: string, parse: (raw: unknown, path: string) => T): T[] {
  if (!Array.isArray(value)) return fail(path, "is not an array");
  return value.map((item, i) => parse(item, `${path}[${i}]`));
}

function parseWindow(raw: unknown, path: string): FleetStatusWindow {
  const o = object(raw, path);
  return {
    window: text(o, "window", path),
    used_pct: nullableNum(o, "used_pct", path),
    resets_at: optionalText(o, "resets_at"),
  };
}

function parseHost(raw: unknown, path: string): FleetStatusHost {
  const o = object(raw, path);
  return {
    host: text(o, "host", path),
    connected: bool(o, "connected", path),
    version: optionalText(o, "version"),
    accepting: bool(o, "accepting", path),
    lanes_used: num(o, "lanes_used", path),
    lanes_cap: num(o, "lanes_cap", path),
  };
}

function parseQuota(raw: unknown, path: string): FleetStatusQuota {
  const o = object(raw, path);
  return {
    host: text(o, "host", path),
    pool: text(o, "pool", path),
    account: text(o, "account", path),
    windows: list(o.windows, `${path}.windows`, parseWindow),
    measured_at: text(o, "measured_at", path),
    age_s: num(o, "age_s", path),
    fresh_ttl_s: num(o, "fresh_ttl_s", path),
    fresh: bool(o, "fresh", path),
    free_state: optionalText(o, "free_state"),
  };
}

function parsePlan(raw: unknown, path: string): FleetStatusPlan {
  const o = object(raw, path);
  return {
    grade: text(o, "grade", path),
    t: text(o, "t", path),
    seat: optionalText(o, "seat"),
    host: optionalText(o, "host"),
    pool: optionalText(o, "pool"),
    account: optionalText(o, "account"),
    tier: optionalText(o, "tier"),
    outcome: text(o, "outcome", path),
  };
}

function parseSnapshot(raw: unknown, path: string): FleetStatusSnapshot {
  const o = object(raw, path);
  return {
    v: num(o, "v", path),
    at: text(o, "at", path),
    hosts: list(o.hosts, `${path}.hosts`, parseHost),
    quota: list(o.quota, `${path}.quota`, parseQuota),
    plan: list(o.plan, `${path}.plan`, parsePlan),
  };
}

export function parseFleetStatus(raw: unknown): FleetStatusResponse {
  const root = object(raw, "root");
  const snapshot = root.snapshot;
  return {
    snapshot: snapshot === null || snapshot === undefined ? null : parseSnapshot(snapshot, "root.snapshot"),
    updated_at: optionalText(root, "updated_at"),
  };
}

export async function loadFleetStatus(): Promise<FleetStatusResponse> {
  const raw = await getJSON<unknown>("/ui/api/fleet-status");
  return parseFleetStatus(raw);
}
