import type { Dataset } from "./types";

const PERF_QUERIES = [
  "fix",
  "implement",
  "verify",
  "decide",
  "synth-lane-ops",
  "#20001",
  "#20500",
  "drawer",
  "큐",
  "preview",
  "20999",
  "hold",
  "backlog",
  "chore",
  "research",
  "adapter",
  "group",
  "#24999",
  "state",
  "signal",
];

let lastWaitUsedFallback = false;

function nextPaint(): Promise<void> {
  return new Promise((resolve) => {
    lastWaitUsedFallback = false;
    const fallback = setTimeout(() => {
      lastWaitUsedFallback = true;
      resolve();
    }, 250);
    requestAnimationFrame(() => {
      requestAnimationFrame(() => {
        clearTimeout(fallback);
        resolve();
      });
    });
  });
}

export function percentile(sorted: number[], p: number): number {
  if (sorted.length === 0) {
    return 0;
  }
  const idx = Math.min(sorted.length - 1, Math.ceil((p / 100) * sorted.length) - 1);
  return sorted[Math.max(0, idx)];
}

export type PerfResult = {
  env: {
    userAgent: string;
    devicePixelRatio: number;
    innerWidth: number;
    innerHeight: number;
    datasetSize: number;
    at: string;
    /** true when every wait resolved via requestAnimationFrame (real frames);
     * false when a setTimeout fallback drove any wait (headless quirks). */
    frameDriven: boolean;
  };
  runs: number[];
  p50: number;
  p95: number;
  max: number;
  targetP95Ms: number;
  pass: boolean;
};

/**
 * Warmup + 20 filter-to-paint measurements. Each run: performance.now() →
 * setQuery (React state update) → two rAFs (commit + paint) → now().
 * The adapter still processes every task — windowing only skips DOM rows,
 * never data. Reports all 20 raw values plus p95.
 */
export async function runPerf(setQuery: (q: string) => void, dataset: Dataset, done: (r: PerfResult) => void): Promise<PerfResult> {
  // warmup — 3 untimed changes
  for (const q of ["warmup-a", "fix", "warmup-b"]) {
    setQuery(q);
    await nextPaint();
  }
  const runs: number[] = [];
  let frameDriven = true;
  for (let i = 0; i < 20; i++) {
    const q = PERF_QUERIES[i % PERF_QUERIES.length];
    const t0 = performance.now();
    setQuery(q);
    await nextPaint();
    if (lastWaitUsedFallback) {
      frameDriven = false;
    }
    runs.push(Math.round((performance.now() - t0) * 100) / 100);
  }
  const sorted = [...runs].sort((a, b) => a - b);
  const result: PerfResult = {
    env: {
      userAgent: navigator.userAgent,
      devicePixelRatio: window.devicePixelRatio,
      innerWidth: window.innerWidth,
      innerHeight: window.innerHeight,
      datasetSize: dataset.tasks.length,
      at: new Date().toISOString(),
      frameDriven,
    },
    runs,
    p50: percentile(sorted, 50),
    p95: percentile(sorted, 95),
    max: sorted[sorted.length - 1] ?? 0,
    targetP95Ms: 200,
    pass: percentile(sorted, 95) < 200,
  };
  done(result);
  return result;
}
