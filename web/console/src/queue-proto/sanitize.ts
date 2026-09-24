// Shape-based fixture sanitation. Real identifiers are rejected by
// category, never by enumerating their values: identifier-bearing fields
// must match a declared synthetic pattern, and every fixture string is
// scanned for generic real-identifier shapes. An invented real-shape
// value is rejected exactly like an actual one, so this guard itself
// stores no real identifier.

import type { Dataset } from "./types";

const SYNTH_NAME = /^synth-/;
const SYNTH_PATH = /^synth\//;
const SYNTH_PR_URL = /^https:\/\/example\.invalid\//;
const SYNTH_SHA = /^[0-9a-f]{40}$/;
const DATASET_KEY = /^[a-z][a-z0-9]*$/;
const UNKNOWN_SENTINEL = "unknown";

// Declared synthetic note families for relation notes (free-text field;
// each family is a shape, not an enumerated real value).
const DECLARED_RELATION_NOTES = [/^synthetic\b/i, /^same outcome\/scope text\b/, /^implement\/verify pair\b/];

type FieldRule = { field: string; expect: string; ok: (value: string) => boolean };

const taskRules: { field: string; get: (t: Dataset["tasks"][number]) => string | null; expect: string; ok: (v: string) => boolean }[] = [
  {
    field: "lane",
    get: (t) => t.lane,
    expect: '"synth-*" or the "unknown" sentinel',
    ok: (v) => SYNTH_NAME.test(v) || v === UNKNOWN_SENTINEL,
  },
  { field: "claimant", get: (t) => t.claimant, expect: '"synth-*"', ok: (v) => SYNTH_NAME.test(v) },
  { field: "created_by", get: (t) => t.created_by, expect: '"synth-*"', ok: (v) => SYNTH_NAME.test(v) },
];

const refRules: Record<string, FieldRule> = {
  job_id: { field: "refs.job_id", expect: '"synth-*"', ok: (v) => SYNTH_NAME.test(v) },
  report_path: { field: "refs.report_path", expect: '"synth/…"', ok: (v) => SYNTH_PATH.test(v) },
  pr: { field: "refs.pr", expect: '"https://example.invalid/…"', ok: (v) => SYNTH_PR_URL.test(v) },
  head_sha: { field: "refs.head_sha", expect: "40-hex", ok: (v) => SYNTH_SHA.test(v) },
};

const enrichmentRules: { field: string; get: (e: Dataset["enrichment"][number]) => string | null; expect: string; ok: (v: string) => boolean }[] = [
  { field: "area", get: (e) => e.area, expect: '"synth-*"', ok: (v) => SYNTH_NAME.test(v) },
  { field: "bundle", get: (e) => e.bundle, expect: '"synth-*"', ok: (v) => SYNTH_NAME.test(v) },
];

// Generic real-identifier shapes. Every entry is a category pattern; no
// real value is stored here. `.md`/`.ts` file extensions are deliberately
// absent from the TLD class so synthetic report paths are not flagged.
export const FORBIDDEN_SHAPES: { name: string; pattern: RegExp }[] = [
  { name: "pane/workspace id", pattern: /\bw\d+:/ },
  { name: "host:port", pattern: /:\d{4,5}(?!\d)/ },
  { name: "absolute home path", pattern: /\/(Users|home)\// },
  { name: "non-example.invalid URL", pattern: /https?:\/\/(?!example\.invalid\b)[A-Za-z0-9.-]+/ },
  { name: "real-TLD domain", pattern: /\b[\w-]+\.(?:ai|com|dev|io|net|org|app|co|kr|xyz|me|info)\b/i },
  { name: "@-handle", pattern: /@[A-Za-z0-9_]/ },
  { name: "IPv4 address", pattern: /\b\d{1,3}(?:\.\d{1,3}){3}\b/ },
];

/** Every string carried by a dataset — the full scan surface. */
export function* fixtureStrings(dataset: Dataset): Generator<string> {
  yield dataset.key;
  yield dataset.label;
  yield dataset.generatedAt;
  yield dataset.completenessNote;
  for (const task of dataset.tasks) {
    yield task.title;
    yield task.kind;
    yield task.state;
    yield task.lane;
    yield task.claimant ?? "";
    yield task.created_by;
    yield task.created_at;
    yield task.state_entered_at ?? "";
    yield task.due_at ?? "";
    yield task.blocker ?? "";
    for (const [k, v] of Object.entries(task.refs)) {
      yield `${k}:${typeof v === "string" ? v : JSON.stringify(v)}`;
    }
    for (const e of task.events) {
      yield `${e.by} ${e.note ?? ""}`;
      yield e.from;
      yield e.to;
      yield e.at;
    }
    for (const d of task.dwell) {
      yield d.state;
    }
    if (task.decision) {
      yield task.decision.question;
      yield task.decision.evidence;
    }
  }
  for (const enr of Object.values(dataset.enrichment)) {
    yield enr.area ?? "";
    yield enr.bundle ?? "";
    for (const l of enr.labels) {
      yield l;
    }
    for (const r of enr.relations) {
      yield `${r.type} ${r.note}`;
    }
  }
}

/** Layer 1: every identifier-bearing field must match its declared synthetic pattern. */
export function identifierViolations(dataset: Dataset): string[] {
  const out: string[] = [];
  if (!DATASET_KEY.test(dataset.key)) {
    out.push(`dataset key ${JSON.stringify(dataset.key)} is outside the declared synthetic namespace (lowercase alnum token)`);
  }
  for (const task of dataset.tasks) {
    for (const rule of taskRules) {
      const v = rule.get(task);
      if (v !== null && !rule.ok(v)) {
        out.push(`task ${task.id} ${rule.field} ${JSON.stringify(v)} is outside the declared synthetic namespace (${rule.expect})`);
      }
    }
    for (const [k, v] of Object.entries(task.refs)) {
      const rule = refRules[k];
      if (!rule) {
        out.push(`task ${task.id} refs.${k} has no declared synthetic pattern`);
      } else if (typeof v !== "string" || !rule.ok(v)) {
        out.push(`task ${task.id} ${rule.field} ${JSON.stringify(v)} is outside the declared synthetic namespace (${rule.expect})`);
      }
    }
    for (const e of task.events) {
      if (!SYNTH_NAME.test(e.by)) {
        out.push(`task ${task.id} event actor ${JSON.stringify(e.by)} is outside the declared synthetic namespace ("synth-*")`);
      }
    }
  }
  for (const [id, enr] of Object.entries(dataset.enrichment)) {
    for (const rule of enrichmentRules) {
      const v = rule.get(enr);
      if (v !== null && !rule.ok(v)) {
        out.push(`enrichment ${id} ${rule.field} ${JSON.stringify(v)} is outside the declared synthetic namespace (${rule.expect})`);
      }
    }
    for (const l of enr.labels) {
      if (!SYNTH_NAME.test(l)) {
        out.push(`enrichment ${id} label ${JSON.stringify(l)} is outside the declared synthetic namespace ("synth-*")`);
      }
    }
    for (const r of enr.relations) {
      if (!DECLARED_RELATION_NOTES.some((p) => p.test(r.note))) {
        out.push(`enrichment ${id} relation note ${JSON.stringify(r.note)} is outside the declared synthetic note families`);
      }
    }
  }
  return out;
}

/** Layer 2: no fixture string may match a generic real-identifier shape. */
export function shapeViolations(dataset: Dataset): string[] {
  const out: string[] = [];
  for (const s of fixtureStrings(dataset)) {
    for (const shape of FORBIDDEN_SHAPES) {
      if (shape.pattern.test(s)) {
        out.push(`fixture string ${JSON.stringify(s)} matches forbidden shape "${shape.name}"`);
      }
    }
  }
  return out;
}

export function sanitizeDataset(dataset: Dataset): string[] {
  return [...identifierViolations(dataset), ...shapeViolations(dataset)];
}
