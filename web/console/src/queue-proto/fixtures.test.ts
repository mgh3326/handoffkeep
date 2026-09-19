import { describe, expect, it } from "vitest";
import { buildEdge, buildPerf5000, buildSample200, EDGE_CASES, PREVIEW_CLAMP, SAMPLE200_DISTRIBUTION } from "./fixtures";
import { identifierViolations, sanitizeDataset, shapeViolations } from "./sanitize";

describe("synthetic fixtures", () => {
  const sample = buildSample200();
  const edge = buildEdge();
  const perf = buildPerf5000();

  it("has the required sizes: 200 / 12 edge cases / 5000", () => {
    expect(sample.tasks).toHaveLength(200);
    expect(EDGE_CASES).toHaveLength(12);
    expect(edge.tasks).toHaveLength(14);
    expect(perf.tasks).toHaveLength(5000);
  });

  it("is deterministic: same seed → same IDs and titles", () => {
    const a = buildSample200(7);
    const b = buildSample200(7);
    expect(a.tasks.map((t) => t.id)).toEqual(b.tasks.map((t) => t.id));
    expect(a.tasks.map((t) => t.title)).toEqual(b.tasks.map((t) => t.title));
    const c = buildPerf5000(7);
    const d = buildPerf5000(7);
    expect(c.tasks.map((t) => t.id)).toEqual(d.tasks.map((t) => t.id));
  });

  it("matches the declared 200-row proportions exactly", () => {
    const kindCounts = new Map<string, number>();
    const laneCounts = new Map<string, number>();
    for (const t of sample.tasks) {
      kindCounts.set(t.kind, (kindCounts.get(t.kind) ?? 0) + 1);
      laneCounts.set(t.lane, (laneCounts.get(t.lane) ?? 0) + 1);
    }
    expect(kindCounts.get("fix")).toBe(103); // 51.5% of 200
    expect(laneCounts.get("synth-lane-ops")).toBe(152); // 76% of 200
    for (const [kind, n] of Object.entries(SAMPLE200_DISTRIBUTION.kinds)) {
      expect(kindCounts.get(kind)).toBe(n);
    }
    for (const [lane, n] of Object.entries(SAMPLE200_DISTRIBUTION.lanes)) {
      expect(laneCounts.get(lane)).toBe(n);
    }
    // lane names are synthetic — none may look like a real lane
    for (const lane of laneCounts.keys()) {
      expect(lane).toMatch(/^synth-/);
    }
  });

  it("keeps every identifier-bearing field inside the declared synthetic namespace", () => {
    for (const dataset of [sample, edge, perf]) {
      expect(identifierViolations(dataset)).toEqual([]);
    }
  });

  it("contains no real-identifier shape in any fixture string", () => {
    for (const dataset of [sample, edge, perf]) {
      expect(shapeViolations(dataset)).toEqual([]);
    }
  });

  it("rejects a newly introduced real-shape identifier in both layers", () => {
    // Invented values only — the guard must catch the shape without
    // anyone having enumerated the value.
    const badLane = buildEdge();
    badLane.tasks[0].lane = "qa-lane-east-7";
    badLane.tasks[0].claimant = "qa-actor-42";
    expect(
      identifierViolations(badLane).some((v) => v.includes("lane") && v.includes("qa-lane-east-7")),
      "non-synth lane must fail the allowlist",
    ).toBe(true);
    expect(
      identifierViolations(badLane).some((v) => v.includes("claimant") && v.includes("qa-actor-42")),
      "non-synth claimant must fail the allowlist",
    ).toBe(true);
    expect(sanitizeDataset(badLane)).not.toEqual([]);

    const badText = buildEdge();
    badText.tasks[0].title = "see pane w9:q7 for details";
    badText.tasks[0].blocker = "host on qa-box:50051 unreachable";
    expect(
      shapeViolations(badText).some((v) => v.includes("pane/workspace id")),
      "pane-shaped id in free text must fail the shape scan",
    ).toBe(true);
    expect(
      shapeViolations(badText).some((v) => v.includes("host:port")),
      "host:port shape in free text must fail the shape scan",
    ).toBe(true);
    expect(sanitizeDataset(badText)).not.toEqual([]);

    const badRef = buildEdge();
    badRef.tasks[0].refs.pr = "https://qa.acme-example.net/x";
    expect(
      identifierViolations(badRef).some((v) => v.includes("refs.pr")),
      "non-example.invalid PR URL must fail the allowlist",
    ).toBe(true);
    expect(
      shapeViolations(badRef).some((v) => v.includes("non-example.invalid URL") || v.includes("real-TLD domain")),
      "real-TLD URL must also fail the shape scan",
    ).toBe(true);
    expect(sanitizeDataset(badRef)).not.toEqual([]);
  });

  it("labels every dataset as synthetic, never the production backlog", () => {
    for (const dataset of [sample, edge, perf]) {
      expect(dataset.completenessNote).toContain("synthetic");
      expect(dataset.completenessNote).toContain("not the production backlog");
    }
  });

  it("contains long mixed ko/en titles beyond the clamp and an unbroken token", () => {
    const long = sample.tasks.filter((t) => t.title.length > PREVIEW_CLAMP);
    expect(long.length).toBeGreaterThan(3);
    expect(long.some((t) => /[가-힣]/.test(t.title) && /[a-zA-Z]/.test(t.title))).toBe(true);
    expect(sample.tasks.some((t) => /x{100,}/.test(t.title))).toBe(true);
    // a searchable marker exists only after the clamp boundary
    const t1150 = sample.tasks.find((t) => t.id === 1150)!;
    expect(t1150.title.indexOf("CLAMPED-TAIL-MARKER-1150")).toBeGreaterThan(PREVIEW_CLAMP);
  });

  it("edge set covers all 12 required cases with distinct ids", () => {
    const ids = new Set(edge.tasks.map((t) => t.id));
    expect(ids.size).toBe(edge.tasks.length);
    const caseIds = new Set(EDGE_CASES.map((c) => c.id));
    for (const required of [
      "unknown-state-age",
      "missing-coverage",
      "unavailable-due-blocker",
      "urgent-in-collapsed-group",
      "intentional-standalone",
      "unclassified",
      "duplicate-candidate-pair",
      "similar-implement-verify-pair",
      "very-long-mixed-title",
      "long-unbroken-token",
      "unknown-lane-claimant",
      "zero-vs-unknown-count",
    ]) {
      expect(caseIds).toContain(required);
    }
    // spot-check semantic payloads
    expect(edge.tasks.find((t) => t.id === 5001)!.state_entered_at).toBeNull();
    expect(edge.tasks.find((t) => t.id === 5002)!.coverage.status).toBe("not_collected");
    expect(edge.tasks.find((t) => t.id === 5014)!.coverage.participants).toBe(0);
    expect(edge.tasks.find((t) => t.id === 5004)!.priority).toBeGreaterThanOrEqual(90);
    expect(edge.enrichment[5005]?.standalone).toBe(true);
    expect(edge.enrichment[5006]).toBeUndefined();
  });
});
