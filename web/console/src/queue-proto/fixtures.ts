// Synthetic, sanitized fixtures. Every record is generated deterministically
// from a seed; lane names, claimants, refs and hosts are invented. Nothing
// here is a real task, lane, or identifier.

import { intBetween, mulberry32, pick } from "./rng";
import type { Dataset, Enrichment, ProtoTask } from "./types";

export const GENERATED_AT = "2026-09-19T09:00:00+09:00";
export const PREVIEW_CLAMP = 96;

// Declared synthetic distribution for the 200-row sample. It reproduces only
// the observed capped sample's proportions: kind=fix 103/200 (51.5%) and the
// dominant lane 152/200 (76%). Remaining mass is a declared invention, not an
// observation about the real backlog.
export const SAMPLE200_DISTRIBUTION = {
  lanes: {
    "synth-lane-ops": 152,
    "synth-lane-research": 20,
    "synth-lane-plan": 12,
    "synth-lane-review": 10,
    "synth-lane-audit": 6,
  },
  kinds: { fix: 103, implement: 41, verify: 20, decide: 16, research: 12, chore: 8 },
  states: {
    backlog: 140,
    hold: 20,
    claimed: 10,
    in_progress: 12,
    verifying: 5,
    needs_decision: 8,
    merged: 3,
    dropped: 2,
  },
} as const;

export const AREAS = ["synth-area-console", "synth-area-docs", "synth-area-ops", "synth-area-experiments"];
export const BUNDLES = [
  "synth-bundle-drawer",
  "synth-bundle-density",
  "synth-bundle-search",
  "synth-bundle-grouping",
  "synth-bundle-perf",
];

const KO_WORDS = ["큐", "보드", "목록", "서랍", "필터", "정렬", "밀도", "검색", "그룹", "신호", "상태", "열"];
const EN_WORDS = ["queue", "drawer", "preview", "filter", "density", "column", "signal", "clamp", "group", "adapter"];
const EN_FRAGS = [
  "compact list renderer",
  "bounded board column",
  "saved view restore",
  "drawer focus return",
  "unknown vs zero rendering",
  "group signal rollup",
  "windowed row paint",
  "preview clamp search",
];
const KO_FRAGS = [
  "작은 헤더와 얇은 도구막대",
  "고유 작업 수 집계",
  "접힌 그룹의 긴급 신호",
  "전체 제목 검색 유지",
  "필터 칩 표시",
  "키보드 이전 다음 이동",
];

function expandDeclared(declared: Record<string, number>): string[] {
  const out: string[] = [];
  for (const [key, count] of Object.entries(declared)) {
    for (let i = 0; i < count; i++) {
      out.push(key);
    }
  }
  return out;
}

function shuffle<T>(rand: () => number, items: T[]): T[] {
  const copy = [...items];
  for (let i = copy.length - 1; i > 0; i--) {
    const j = Math.floor(rand() * (i + 1));
    [copy[i], copy[j]] = [copy[j], copy[i]];
  }
  return copy;
}

function synthTitle(rand: () => number, id: number, longEvery: number): string {
  if (id % longEvery === 0) {
    // Deterministic long mixed title, far beyond the preview clamp.
    const ko = Array.from({ length: 6 }, () => pick(rand, KO_WORDS)).join("");
    const en = Array.from({ length: 8 }, () => pick(rand, EN_WORDS)).join("-");
    return `긴 한영 혼합 제목 ${ko} — mixed ko/en long title ${en} ${pick(rand, EN_FRAGS)} 추가 설명이 이어지는 본문형 제목 ${id}`;
  }
  if (rand() < 0.35) {
    return `${pick(rand, KO_FRAGS)} — ${pick(rand, EN_FRAGS)}`;
  }
  if (rand() < 0.5) {
    return `${pick(rand, EN_FRAGS)} (${pick(rand, KO_WORDS)}${pick(rand, KO_WORDS)})`;
  }
  return `${pick(rand, EN_FRAGS)} #${id % 97}`;
}

function synthSha(id: number): string {
  return id.toString(16).padStart(40, "0");
}

function baseTask(id: number, rand: () => number): ProtoTask {
  const createdDay = intBetween(rand, 5, 18);
  const created = `2026-09-${String(createdDay).padStart(2, "0")}T${String(intBetween(rand, 0, 23)).padStart(2, "0")}:15:00+09:00`;
  const enteredDay = Math.min(19, createdDay + intBetween(rand, 0, 3));
  return {
    id,
    title: "",
    kind: "fix",
    state: "backlog",
    lane: "synth-lane-ops",
    claimant: rand() < 0.4 ? `synth-claim-${intBetween(rand, 1, 9)}` : null,
    priority: intBetween(rand, 10, 99),
    created_at: created,
    state_entered_at: `2026-09-${String(enteredDay).padStart(2, "0")}T10:00:00+09:00`,
    due_at: rand() < 0.25 ? `2026-09-${String(Math.min(28, enteredDay + 7)).padStart(2, "0")}T00:00:00+09:00` : null,
    blocker: rand() < 0.15 ? `synth blocker note ${id}` : null,
    created_by: "synth-op",
    refs: {
      report_path: `synth/reports/task-${id}.md`,
      job_id: `synth-job-${id}`,
      ...(rand() < 0.3 ? { pr: `https://example.invalid/synth/pull/${id}`, head_sha: synthSha(id) } : {}),
    },
    events: [
      { id: id * 10 + 1, from: "backlog", to: "claimed", by: "synth-op", at: created },
      { id: id * 10 + 2, from: "claimed", to: "in_progress", by: "synth-op", note: "synthetic transition", at: `2026-09-${String(enteredDay).padStart(2, "0")}T10:00:00+09:00` },
    ],
    dwell: [
      { state: "backlog", seconds: 86400 * (enteredDay - createdDay), open: false },
      { state: "in_progress", seconds: 3600 * intBetween(rand, 1, 40), open: true },
    ],
    coverage: rand() < 0.7 ? { status: "collected", participants: intBetween(rand, 0, 4) } : { status: "not_collected", participants: null },
  };
}

function synthEnrichment(rand: () => number, id: number): Enrichment | null {
  const roll = rand();
  if (roll < 0.08) {
    return { area: null, bundle: null, standalone: true, labels: ["synth-standalone"], relations: [] };
  }
  if (roll < 0.4) {
    return null; // unclassified — no enrichment entry at all.
  }
  const area = pick(rand, AREAS);
  const bundle = rand() < 0.85 ? pick(rand, BUNDLES) : null;
  return {
    area,
    bundle,
    standalone: false,
    labels: rand() < 0.4 ? ["synth-draft-label"] : [],
    relations:
      rand() < 0.1
        ? [{ type: "related", otherId: id + 1, note: "synthetic relation candidate — unreviewed" }]
        : [],
  };
}

export function buildSample200(seed = 7): Dataset {
  const rand = mulberry32(seed);
  const lanes = shuffle(rand, expandDeclared(SAMPLE200_DISTRIBUTION.lanes));
  const kinds = shuffle(rand, expandDeclared(SAMPLE200_DISTRIBUTION.kinds));
  const states = shuffle(rand, expandDeclared(SAMPLE200_DISTRIBUTION.states));
  const tasks: ProtoTask[] = [];
  const enrichment: Record<number, Enrichment> = {};
  for (let i = 0; i < 200; i++) {
    const id = 1001 + i;
    const task = baseTask(id, rand);
    task.lane = lanes[i];
    task.kind = kinds[i];
    task.state = states[i];
    task.title = synthTitle(rand, id, 37);
    if (id === 1111) {
      task.title = `한영 혼합 unbroken token 케이스 ${"x".repeat(140)} — 검색은 원문 전체에서 동작한다`;
    }
    if (id === 1150) {
      task.title = `preview clamp 경계 뒤 텍스트 — ${"tail ".repeat(30)}CLAMPED-TAIL-MARKER-1150 끝`;
    }
    if (task.state === "needs_decision" || task.kind === "decide") {
      task.decision = {
        question: `합성 결정 질문 #${id}: 어느 접근을 선택해야 하는가?`,
        evidence: `synth/reports/task-${id}.md`,
      };
    }
    const enr = synthEnrichment(rand, id);
    if (enr) {
      enrichment[id] = enr;
    }
    tasks.push(task);
  }
  return {
    key: "sample200",
    label: "synthetic 200-task sample",
    generatedAt: GENERATED_AT,
    completeness: "partial",
    completenessNote: "capped-sample proportions only — synthetic, not the production backlog",
    tasks,
    enrichment,
  };
}

export type EdgeCase = { id: string; description: string; taskIds: number[] };

export const EDGE_CASES: EdgeCase[] = [
  { id: "unknown-state-age", description: "current-state age is unknown (no state_entered_at)", taskIds: [5001] },
  { id: "missing-coverage", description: "participation coverage not collected — distinct from 0", taskIds: [5002] },
  { id: "unavailable-due-blocker", description: "due and blocker values unavailable", taskIds: [5003] },
  { id: "urgent-in-collapsed-group", description: "urgent high-priority task inside a collapsible group", taskIds: [5004] },
  { id: "intentional-standalone", description: "intentional standalone — not part of any bundle", taskIds: [5005] },
  { id: "unclassified", description: "unclassified task with no enrichment entry", taskIds: [5006] },
  { id: "duplicate-candidate-pair", description: "duplicate-candidate pair with relation evidence", taskIds: [5007, 5008] },
  { id: "similar-implement-verify-pair", description: "deliberately similar but distinct implement/verify pair", taskIds: [5009, 5010] },
  { id: "very-long-mixed-title", description: "very long mixed Korean/English title beyond the clamp", taskIds: [5011] },
  { id: "long-unbroken-token", description: "title with a long unbroken token", taskIds: [5012] },
  { id: "unknown-lane-claimant", description: "unknown lane and claimant", taskIds: [5013] },
  { id: "zero-vs-unknown-count", description: "collected count of 0 — distinct from not collected", taskIds: [5014] },
];

export function buildEdge(): Dataset {
  const at = (day: number, h = 10) => `2026-09-${String(day).padStart(2, "0")}T${String(h).padStart(2, "0")}:00:00+09:00`;
  const mk = (id: number, over: Partial<ProtoTask>): ProtoTask => ({
    id,
    title: `synthetic edge task ${id}`,
    kind: "fix",
    state: "backlog",
    lane: "synth-lane-ops",
    claimant: "synth-claim-1",
    priority: 50,
    created_at: at(6),
    state_entered_at: at(8),
    due_at: at(25),
    blocker: null,
    created_by: "synth-op",
    refs: { report_path: `synth/reports/task-${id}.md`, job_id: `synth-job-${id}` },
    events: [{ id: id * 10, from: "backlog", to: "backlog", by: "synth-op", note: "synthetic", at: at(6) }],
    dwell: [{ state: "backlog", seconds: 86400, open: true }],
    coverage: { status: "collected", participants: 2 },
    ...over,
  });
  const tasks: ProtoTask[] = [
    mk(5001, { state_entered_at: null, title: "edge: current-state age unknown", state: "in_progress" }),
    mk(5002, { coverage: { status: "not_collected", participants: null }, title: "edge: participation coverage missing" }),
    mk(5003, { due_at: null, blocker: null, title: "edge: due and blocker unavailable" }),
    mk(5004, {
      priority: 97,
      title: "edge: urgent task inside collapsed group",
      decision: { question: "합성 긴급 결정: 지금 개입이 필요한가?", evidence: "synth/reports/task-5004.md" },
    }),
    mk(5005, { title: "edge: intentional standalone" }),
    mk(5006, { title: "edge: unclassified task" }),
    mk(5007, { title: "edge: fix queue drawer scroll sync", kind: "fix" }),
    mk(5008, { title: "edge: fix queue drawer scroll sync", kind: "fix" }),
    mk(5009, { title: "edge: implement saved-view version gate", kind: "implement" }),
    mk(5010, { title: "edge: verify saved-view version gate", kind: "verify" }),
    mk(5011, {
      title:
        "edge: 매우 긴 한영 혼합 제목 — the quick brown fox jumps over the lazy dog 그리고 이어지는 한국어 설명이 preview clamp를 훨씬 넘어서 계속됩니다 additional english words keep flowing past the boundary CLAMPED-TAIL-MARKER-5011 final segment",
    }),
    mk(5012, { title: `edge: unbroken token ${"z".repeat(160)}` }),
    mk(5013, { lane: "unknown", claimant: null, title: "edge: unknown lane and claimant" }),
    mk(5014, { coverage: { status: "collected", participants: 0 }, title: "edge: collected count of zero" }),
  ];
  const enrichment: Record<number, Enrichment> = {
    5003: { area: "synth-area-console", bundle: "synth-bundle-edge", standalone: false, labels: [], relations: [] },
    5004: { area: "synth-area-console", bundle: "synth-bundle-edge", standalone: false, labels: ["synth-urgent"], relations: [] },
    5005: { area: null, bundle: null, standalone: true, labels: ["synth-standalone"], relations: [] },
    5007: {
      area: "synth-area-console",
      bundle: "synth-bundle-drawer",
      standalone: false,
      labels: [],
      relations: [{ type: "duplicate-candidate", otherId: 5008, note: "same outcome/scope text — review before any merge" }],
    },
    5008: {
      area: "synth-area-console",
      bundle: "synth-bundle-drawer",
      standalone: false,
      labels: [],
      relations: [{ type: "duplicate-candidate", otherId: 5007, note: "same outcome/scope text — review before any merge" }],
    },
    5009: {
      area: "synth-area-console",
      bundle: "synth-bundle-search",
      standalone: false,
      labels: [],
      relations: [{ type: "verifies", otherId: 5010, note: "implement/verify pair — intentionally similar, distinct tasks" }],
    },
    5010: {
      area: "synth-area-console",
      bundle: "synth-bundle-search",
      standalone: false,
      labels: [],
      relations: [{ type: "implements", otherId: 5009, note: "implement/verify pair — intentionally similar, distinct tasks" }],
    },
    5011: { area: "synth-area-docs", bundle: null, standalone: false, labels: [], relations: [] },
  };
  return {
    key: "edge",
    label: "synthetic 12-case edge set",
    generatedAt: GENERATED_AT,
    completeness: "partial",
    completenessNote: "explicit edge cases only — synthetic, not the production backlog",
    tasks,
    enrichment,
  };
}

export function buildPerf5000(seed = 7): Dataset {
  const rand = mulberry32(seed);
  const laneNames = Object.keys(SAMPLE200_DISTRIBUTION.lanes);
  const laneWeights = [0.76, 0.1, 0.06, 0.05, 0.03];
  const kindNames = ["fix", "implement", "verify", "decide", "research", "chore"];
  const kindWeights = [0.515, 0.2, 0.1, 0.08, 0.06, 0.045];
  const stateNames = ["backlog", "hold", "claimed", "in_progress", "verifying", "needs_decision", "merged", "dropped"];
  const stateWeights = [0.6, 0.12, 0.06, 0.08, 0.04, 0.04, 0.04, 0.02];
  const weighted = (weights: number[]) => {
    const roll = rand();
    let acc = 0;
    for (let i = 0; i < weights.length; i++) {
      acc += weights[i];
      if (roll < acc) {
        return i;
      }
    }
    return weights.length - 1;
  };
  const tasks: ProtoTask[] = [];
  const enrichment: Record<number, Enrichment> = {};
  for (let i = 0; i < 5000; i++) {
    const id = 20001 + i;
    const task = baseTask(id, rand);
    task.lane = laneNames[weighted(laneWeights)];
    task.kind = kindNames[weighted(kindWeights)];
    task.state = stateNames[weighted(stateWeights)];
    task.title = synthTitle(rand, id, 97);
    if (task.state === "needs_decision" || task.kind === "decide") {
      task.decision = {
        question: `합성 결정 질문 #${id}`,
        evidence: `synth/reports/task-${id}.md`,
      };
    }
    const enr = synthEnrichment(rand, id);
    if (enr) {
      enrichment[id] = enr;
    }
    tasks.push(task);
  }
  return {
    key: "perf5000",
    label: "synthetic 5000-task performance set",
    generatedAt: GENERATED_AT,
    completeness: "complete",
    completenessNote: "self-contained synthetic set — deterministic seed, not the production backlog",
    tasks,
    enrichment,
  };
}

export function buildDatasets(): Record<string, Dataset> {
  return {
    sample200: buildSample200(),
    edge: buildEdge(),
    perf5000: buildPerf5000(),
  };
}
