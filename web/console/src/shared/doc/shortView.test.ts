import { describe, expect, it } from "vitest";
import {
  STATE_LABEL,
  docHref,
  formatCount,
  fullDocHref,
  isShortViewKey,
  isValidUntil,
  largestStorage,
  parseReview,
  parseStatsHistory,
  shortViewApplies,
  siblingReviewKey,
  siblingStatsKey,
  tableSectionHref,
  tableState,
} from "./shortView";

const DOC_KEY = "tbls/oci/mysql/handoffkeep/doc";

function statsBody(historyJson: string): string {
  return `# stats\n\n## History\n\n\`\`\`json table-stats-history\n${historyJson}\n\`\`\`\n`;
}

const TWO_SNAPS = JSON.stringify({
  v: 1,
  snapshots: [
    {
      at: "2026-11-24T08:42:02Z",
      tables: { a: [10, 10, 1000, 200], b: [1, 1, 5000, 0], d: [1, 1, 50, 50] },
    },
    {
      at: "2026-11-25T03:15:35Z",
      tables: { a: [20, 20, 1000, 200], b: [2, 2, 6000, 1000], d: [1, 1, 50, 50], e: [1, 1, 9000, 0] },
    },
  ],
});

function reviewBody(overrides: Record<string, unknown> = {}): string {
  return JSON.stringify({
    version: 1,
    read_cost: {
      window_start: "2026-11-25T04:57:28Z",
      window_end: "2026-11-25T05:27:40Z",
      source: "findings/2026-11-25/hk5584-table-io",
      rows: [{ table: "b", rows_read: 1698508 }],
    },
    cleanup: ["c"],
    tables: {
      a: { state: "DECIDED", decision: "493", note: "done" },
      b: { state: "OPEN", note: "open" },
      c: { state: "NOT_A_PROBLEM", finding: "findings/2026-11-25/hk1", note: "fine" },
      e: { state: "WAITING", until: "2026-12-10", note: "wait" },
    },
    ...overrides,
  });
}

describe("short view key", () => {
  it.each([
    "tbls/oci/mysql/handoffkeep/doc",
    "tbls/aws/postgres/other/doc",
  ])("covers %s", (key) => {
    expect(isShortViewKey(key)).toBe(true);
  });

  it.each([
    "tbls/oci/mysql/handoffkeep/stats",
    "tbls/oci/mysql/doc",
    "tbls/oci/mysql/handoffkeep/doc/extra",
    "decision/2026-11-25/example",
    "tbls/oci/mysql/handoffkeep/review.json",
  ])("does not cover %s", (key) => {
    expect(isShortViewKey(key)).toBe(false);
  });

  it("names the sibling stats and review keys", () => {
    expect(siblingStatsKey(DOC_KEY)).toBe("tbls/oci/mysql/handoffkeep/stats");
    expect(siblingReviewKey(DOC_KEY)).toBe("tbls/oci/mysql/handoffkeep/review.json");
  });

  it.each([
    [DOC_KEY, "", "", true],
    [DOC_KEY, "?full=1", "", false],
    [DOC_KEY, "", "#user-content-file-a.md", false],
    [DOC_KEY, "?full=1", "#user-content-file-a.md", false],
    ["tbls/oci/mysql/handoffkeep/stats", "", "", false],
    ["decision/2026-11-25/example", "", "", false],
  ])("shortViewApplies(%s, %s, %s) is %s", (key, search, hash, expected) => {
    expect(shortViewApplies(key as string, search as string, hash as string)).toBe(expected);
  });
});

describe("parseStatsHistory", () => {
  it("reads two snapshots with data plus index bytes", () => {
    const snaps = parseStatsHistory(statsBody(TWO_SNAPS));
    expect(snaps).toHaveLength(2);
    expect(snaps[1]).toEqual({
      at: "2026-11-25T03:15:35Z",
      tables: { a: [20, 20, 1000, 200], b: [2, 2, 6000, 1000], d: [1, 1, 50, 50], e: [1, 1, 9000, 0] },
    });
  });

  it("accepts the shape doc's `version` spelling as well as the writer's `v`", () => {
    const snaps = parseStatsHistory(statsBody(JSON.stringify({ version: 1, snapshots: [] })));
    expect(snaps).toEqual([]);
  });

  it.each([
    ["no fence", "# stats\nno history here", "table-stats-history 블록이 없습니다"],
    ["unclosed fence", "```json table-stats-history\n{}", "닫히지 않았습니다"],
    ["broken json", statsBody("not json"), "JSON이 아닙니다"],
    ["unknown version", statsBody(JSON.stringify({ v: 9, snapshots: [] })), "version이 낯설어요: 9"],
  ])("rejects %s", (_name, body, message) => {
    expect(() => parseStatsHistory(body as string)).toThrow(message as string);
  });
});

describe("largestStorage", () => {
  it("orders by data plus index bytes with the change since the previous snapshot", () => {
    const { at, rows } = largestStorage(parseStatsHistory(statsBody(TWO_SNAPS)));
    expect(at).toBe("2026-11-25T03:15:35Z");
    expect(rows.map((r) => r.table)).toEqual(["e", "b", "a", "d"]);
    expect(rows[0]).toEqual({ table: "e", bytes: 9000, delta: { kind: "new" } });
    expect(rows[1]).toEqual({ table: "b", bytes: 7000, delta: { kind: "bytes", value: 2000 } });
    expect(rows[2]).toEqual({ table: "a", bytes: 1200, delta: { kind: "bytes", value: 0 } });
  });

  it("keeps the top 5 and marks a lone snapshot as having nothing to compare", () => {
    const snaps = parseStatsHistory(
      statsBody(JSON.stringify({ v: 1, snapshots: [{ at: "2026-11-25T03:15:35Z", tables: { t1: [0, 0, 6, 0], t2: [0, 0, 5, 0], t3: [0, 0, 4, 0], t4: [0, 0, 3, 0], t5: [0, 0, 2, 0], t6: [0, 0, 1, 0] } }] })),
    );
    const { rows } = largestStorage(snaps);
    expect(rows.map((r) => r.table)).toEqual(["t1", "t2", "t3", "t4", "t5"]);
    expect(rows[0].delta).toEqual({ kind: "same" });
  });
});

describe("parseReview", () => {
  it("reads the window, source, cleanup and per-table states", () => {
    const review = parseReview(reviewBody());
    expect(review.readCost).toEqual({
      windowStart: "2026-11-25T04:57:28Z",
      windowEnd: "2026-11-25T05:27:40Z",
      source: "findings/2026-11-25/hk5584-table-io",
      rows: [{ table: "b", rowsRead: 1698508 }],
    });
    expect(review.cleanup).toEqual(["c"]);
    expect(review.tables.a).toEqual({ state: "DECIDED", decision: "493", note: "done" });
  });

  it.each([
    ["not json", "nope", "JSON이 아닙니다"],
    ["unknown version", reviewBody({ version: 2 }), "version이 낯설어요: 2"],
    ["unknown state", reviewBody({ tables: { x: { state: "CLOSED", note: "" } } }), "state가 낯설어요: CLOSED"],
    ["DECIDED without decision", reviewBody({ tables: { x: { state: "DECIDED", note: "" } } }), "DECIDED인데 decision이 없습니다"],
    ["NOT_A_PROBLEM without finding", reviewBody({ tables: { x: { state: "NOT_A_PROBLEM", note: "" } } }), "NOT_A_PROBLEM인데 finding이 없습니다"],
    ["WAITING without until", reviewBody({ tables: { x: { state: "WAITING", note: "" } } }), "WAITING인데 until이 올바른 날짜가 아닙니다"],
    ["WAITING with a wrong date shape", reviewBody({ tables: { x: { state: "WAITING", until: "2026-12-1", note: "" } } }), "until이 올바른 날짜가 아닙니다"],
    ["WAITING with a date that does not exist", reviewBody({ tables: { x: { state: "WAITING", until: "2026-02-30", note: "" } } }), "until이 올바른 날짜가 아닙니다"],
    ["missing read_cost", reviewBody({ read_cost: undefined }), "read_cost가 없습니다"],
    ["missing cleanup", reviewBody({ cleanup: [7] }), "cleanup 목록이 없습니다"],
  ])("refuses %s with one line naming the problem", (_name, body, message) => {
    expect(() => parseReview(body as string)).toThrow(message as string);
  });
});

describe("states and links", () => {
  it("labels every state in Korean and treats a missing table as OPEN", () => {
    expect(STATE_LABEL).toEqual({ OPEN: "검토 필요", DECIDED: "결정됨", NOT_A_PROBLEM: "문제 아님", WAITING: "대기" });
    const review = parseReview(reviewBody());
    expect(tableState(review.tables, "b").state).toBe("OPEN");
    expect(tableState(review.tables, "zz-missing").state).toBe("OPEN");
    expect(tableState(review.tables, "a")).toEqual({ state: "DECIDED", decision: "493", note: "done" });
  });

  it("validates until dates on the real calendar", () => {
    expect(isValidUntil("2026-12-10")).toBe(true);
    expect(isValidUntil("2026-02-30")).toBe(false);
    expect(isValidUntil("2026-1-10")).toBe(false);
    expect(isValidUntil("tomorrow")).toBe(false);
  });

  it("builds the full-view and table-section links on the shared anchor", () => {
    expect(docHref("findings/2026-11-25/hk1")).toBe("/ui/doc/findings/2026-11-25/hk1");
    expect(fullDocHref(DOC_KEY)).toBe("/ui/doc/tbls/oci/mysql/handoffkeep/doc?full=1");
    expect(tableSectionHref(DOC_KEY, "tasks")).toBe(
      "/ui/doc/tbls/oci/mysql/handoffkeep/doc?full=1#user-content-file-tasks.md",
    );
  });

  it("groups thousands without a locale", () => {
    expect(formatCount(0)).toBe("0");
    expect(formatCount(1698508)).toBe("1,698,508");
    expect(formatCount(-1024)).toBe("-1,024");
  });
});
