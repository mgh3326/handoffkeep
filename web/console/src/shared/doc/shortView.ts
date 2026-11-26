// The short schema view behind a tbls `<prefix>/doc` key (hk 6392). A key
// matching `tbls/<host>/<engine>/<db>/doc`, opened with no section and no
// `?full=`, shows this summary instead of the full document. It reads two
// sibling documents by key: the stats worker's `<prefix>/stats` (markdown
// with one JSON history line in a fenced `json table-stats-history` block)
// and the owning lane's `<prefix>/review.json` (the states, the read-cost
// window and the cleanup list). Pure helpers only; fetching lives in the
// ShortView component. The shape is fixed by hk doc
// findings/2026-11-25/hk6382-short-view-shape — panewire's hub follows the
// same shape, so do not change it here.

import { SANITIZED_ID_PREFIX, fileAnchorId } from "./tableLinks";

/** A tbls combined-document key: `tbls/<host>/<engine>/<db>/doc`. */
const SHORT_VIEW_KEY = /^tbls\/[^/]+\/[^/]+\/[^/]+\/doc$/;

/** True when the key is a tbls combined document the short view covers. */
export function isShortViewKey(key: string): boolean {
  return SHORT_VIEW_KEY.test(key);
}

/** The stats sibling of a tbls doc key (`…/doc` → `…/stats`). */
export function siblingStatsKey(key: string): string {
  return `${key.slice(0, -"doc".length)}stats`;
}

/** The review sibling of a tbls doc key (`…/doc` → `…/review.json`). */
export function siblingReviewKey(key: string): string {
  return `${key.slice(0, -"doc".length)}review.json`;
}

/** True when the short view applies: a tbls doc key, no `?full=`, and no
 * section asked for (the hash carries the section). Both the full-document
 * button and a table row are plain links, so back and forward work. */
export function shortViewApplies(key: string, search: string, hash: string): boolean {
  if (!isShortViewKey(key)) {
    return false;
  }
  const full = new URLSearchParams(search).get("full");
  if (full !== null && full !== "0" && full !== "false") {
    return false;
  }
  return hash.replace(/^#/, "") === "";
}

/** One stats snapshot: `at` is RFC3339, and each table maps to
 * `[rows_exact, rows_estimate, data_bytes, index_bytes]`. */
export type StatsSnapshot = {
  at: string;
  tables: Record<string, [number, number, number, number]>;
};

const STATS_FENCE = "```json table-stats-history";

/** Extracts and checks the history block from a stats document body. Throws
 * a plain Error naming the problem; the view shows it as one line. */
export function parseStatsHistory(body: string): StatsSnapshot[] {
  const fenceAt = body.indexOf(STATS_FENCE);
  if (fenceAt < 0) {
    throw new Error("stats에 table-stats-history 블록이 없습니다");
  }
  const rest = body.slice(fenceAt + STATS_FENCE.length);
  const closeAt = rest.indexOf("\n```");
  if (closeAt < 0) {
    throw new Error("stats의 table-stats-history 블록이 닫히지 않았습니다");
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(rest.slice(0, closeAt).trim());
  } catch {
    throw new Error("stats의 table-stats-history가 JSON이 아닙니다");
  }
  if (typeof parsed !== "object" || parsed === null) {
    throw new Error("stats의 table-stats-history가 JSON이 아닙니다");
  }
  const doc = parsed as Record<string, unknown>;
  // The writer emits `v`; the shape doc writes `version`. Both mean 1.
  const version = doc.version ?? doc.v;
  if (version !== 1) {
    throw new Error(`stats의 table-stats-history version이 낯설어요: ${String(version)}`);
  }
  if (!Array.isArray(doc.snapshots)) {
    throw new Error("stats의 table-stats-history에 snapshots가 없습니다");
  }
  if (doc.snapshots.length === 0) {
    throw new Error("stats의 table-stats-history에 snapshot이 없습니다");
  }
  return (doc.snapshots as unknown[]).map((entry, i) => {
    if (typeof entry !== "object" || entry === null) {
      throw new Error(`stats의 snapshot ${i}가 올바르지 않습니다`);
    }
    const snap = entry as Record<string, unknown>;
    if (typeof snap.at !== "string" || !Number.isFinite(Date.parse(snap.at))) {
      throw new Error(`stats의 snapshot ${i}에 at이 없습니다`);
    }
    if (typeof snap.tables !== "object" || snap.tables === null) {
      throw new Error(`stats의 snapshot ${i}에 tables가 없습니다`);
    }
    const tables: StatsSnapshot["tables"] = {};
    for (const [name, sizes] of Object.entries(snap.tables as Record<string, unknown>)) {
      if (
        !Array.isArray(sizes) ||
        sizes.length !== 4 ||
        !sizes.every((n) => typeof n === "number" && Number.isFinite(n))
      ) {
        throw new Error(`stats의 snapshot ${i}에 테이블 '${name}'의 크기가 올바르지 않습니다`);
      }
      tables[name] = sizes as [number, number, number, number];
    }
    return { at: snap.at, tables };
  });
}

export type StorageDelta = { kind: "same" } | { kind: "new" } | { kind: "bytes"; value: number };

export type StorageRow = {
  table: string;
  bytes: number;
  delta: StorageDelta;
};

/** Top 5 tables by data plus index bytes of the newest snapshot, with the
 * change since the previous snapshot. Computed when the view is drawn, so it
 * never goes stale. */
export function largestStorage(snapshots: StatsSnapshot[], limit = 5): { at: string; rows: StorageRow[] } {
  const cur = snapshots[snapshots.length - 1];
  const prev = snapshots.length >= 2 ? snapshots[snapshots.length - 2] : null;
  const rows = Object.entries(cur.tables)
    .map(([table, sizes]) => {
      const bytes = sizes[2] + sizes[3];
      let delta: StorageDelta;
      if (prev === null) {
        delta = { kind: "same" };
      } else if (!Object.prototype.hasOwnProperty.call(prev.tables, table)) {
        delta = { kind: "new" };
      } else {
        delta = { kind: "bytes", value: bytes - (prev.tables[table][2] + prev.tables[table][3]) };
      }
      return { table, bytes, delta };
    })
    .sort((a, b) => b.bytes - a.bytes || (a.table < b.table ? -1 : a.table > b.table ? 1 : 0))
    .slice(0, limit);
  return { at: cur.at, rows };
}

export type ReviewState = "OPEN" | "DECIDED" | "NOT_A_PROBLEM" | "WAITING";

const REVIEW_STATES = new Set<string>(["OPEN", "DECIDED", "NOT_A_PROBLEM", "WAITING"]);

export type ReviewTable = {
  state: ReviewState;
  decision?: string;
  finding?: string;
  until?: string;
  note: string;
};

export type ReviewDoc = {
  readCost: {
    windowStart: string;
    windowEnd: string;
    source: string;
    rows: { table: string; rowsRead: number }[];
  };
  cleanup: string[];
  tables: Record<string, ReviewTable>;
};

function isNonEmptyString(value: unknown): value is string {
  return typeof value === "string" && value !== "";
}

/** A YYYY-MM-DD date that really exists on the calendar. */
export function isValidUntil(value: unknown): value is string {
  if (typeof value !== "string" || !/^\d{4}-\d{2}-\d{2}$/.test(value)) {
    return false;
  }
  const [y, m, d] = value.split("-").map(Number);
  const t = new Date(Date.UTC(y, m - 1, d));
  return t.getUTCFullYear() === y && t.getUTCMonth() === m - 1 && t.getUTCDate() === d;
}

/** Reads and validates a review.json body. Throws one Error naming the
 * first problem — the view shows that line and still shows the storage
 * group, never an empty or half-silent view. */
export function parseReview(body: string): ReviewDoc {
  let parsed: unknown;
  try {
    parsed = JSON.parse(body);
  } catch {
    throw new Error("review.json이 JSON이 아닙니다");
  }
  if (typeof parsed !== "object" || parsed === null) {
    throw new Error("review.json이 JSON이 아닙니다");
  }
  const doc = parsed as Record<string, unknown>;
  if (doc.version !== 1) {
    throw new Error(`review.json의 version이 낯설어요: ${String(doc.version)}`);
  }
  if (typeof doc.read_cost !== "object" || doc.read_cost === null) {
    throw new Error("review.json에 read_cost가 없습니다");
  }
  const cost = doc.read_cost as Record<string, unknown>;
  const { window_start, window_end, source, rows } = cost;
  if (typeof window_start !== "string" || typeof window_end !== "string" || typeof source !== "string") {
    throw new Error("review.json의 read_cost에 window나 source가 없습니다");
  }
  if (!Array.isArray(rows)) {
    throw new Error("review.json의 read_cost에 rows가 없습니다");
  }
  const readRows = (rows as unknown[]).map((entry, i) => {
    if (typeof entry !== "object" || entry === null) {
      throw new Error(`review.json의 read_cost rows ${i}가 올바르지 않습니다`);
    }
    const row = entry as Record<string, unknown>;
    if (typeof row.table !== "string" || typeof row.rows_read !== "number" || !Number.isFinite(row.rows_read)) {
      throw new Error(`review.json의 read_cost rows ${i}가 올바르지 않습니다`);
    }
    return { table: row.table, rowsRead: row.rows_read };
  });
  if (!Array.isArray(doc.cleanup) || !(doc.cleanup as unknown[]).every((t) => typeof t === "string")) {
    throw new Error("review.json에 cleanup 목록이 없습니다");
  }
  if (typeof doc.tables !== "object" || doc.tables === null) {
    throw new Error("review.json에 tables가 없습니다");
  }
  const tables: Record<string, ReviewTable> = {};
  for (const [name, entry] of Object.entries(doc.tables as Record<string, unknown>)) {
    if (typeof entry !== "object" || entry === null) {
      throw new Error(`review.json의 테이블 '${name}'이 올바르지 않습니다`);
    }
    const row = entry as Record<string, unknown>;
    const state = row.state;
    if (typeof state !== "string" || !REVIEW_STATES.has(state)) {
      throw new Error(`review.json의 테이블 '${name}'의 state가 낯설어요: ${String(state)}`);
    }
    if (state === "DECIDED" && !isNonEmptyString(row.decision)) {
      throw new Error(`review.json의 테이블 '${name}'은 DECIDED인데 decision이 없습니다`);
    }
    if (state === "NOT_A_PROBLEM" && !isNonEmptyString(row.finding)) {
      throw new Error(`review.json의 테이블 '${name}'은 NOT_A_PROBLEM인데 finding이 없습니다`);
    }
    if (state === "WAITING" && !isValidUntil(row.until)) {
      throw new Error(`review.json의 테이블 '${name}'은 WAITING인데 until이 올바른 날짜가 아닙니다`);
    }
    const table: ReviewTable = { state: state as ReviewState, note: typeof row.note === "string" ? row.note : "" };
    if (isNonEmptyString(row.decision)) {
      table.decision = row.decision;
    }
    if (isNonEmptyString(row.finding)) {
      table.finding = row.finding;
    }
    if (isNonEmptyString(row.until)) {
      table.until = row.until;
    }
    tables[name] = table;
  }
  return {
    readCost: { windowStart: window_start, windowEnd: window_end, source, rows: readRows },
    cleanup: doc.cleanup as string[],
    tables,
  };
}

/** The state labels, in the console's Korean, from the shape doc's table. */
export const STATE_LABEL: Record<ReviewState, string> = {
  OPEN: "검토 필요",
  DECIDED: "결정됨",
  NOT_A_PROBLEM: "문제 아님",
  WAITING: "대기",
};

/** A table with no entry in `tables` is OPEN. */
export function tableState(tables: Record<string, ReviewTable>, name: string): ReviewTable {
  return tables[name] ?? { state: "OPEN", note: "" };
}

/** Thousands-grouped digits, deterministic across environments (no locale). */
export function formatCount(n: number): string {
  const neg = n < 0 ? "-" : "";
  const digits = String(Math.abs(Math.trunc(n)));
  return neg + digits.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
}

/** One `/ui/doc/<key>` href: each segment encoded, since the route splits on
 * `/` and decodes each segment. */
export function docHref(key: string): string {
  return `/ui/doc/${key.split("/").map(encodeURIComponent).join("/")}`;
}

/** The plain link that opens today's full view. */
export function fullDocHref(key: string): string {
  return `${docHref(key)}?full=1`;
}

/** A row's plain link: the full view at that table's section, using the same
 * anchor the combined document's own table links resolve to. */
export function tableSectionHref(key: string, table: string): string {
  return `${fullDocHref(key)}#${SANITIZED_ID_PREFIX}${fileAnchorId(`${table}.md`)}`;
}
