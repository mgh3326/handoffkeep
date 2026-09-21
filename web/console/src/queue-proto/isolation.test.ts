import { describe, expect, it } from "vitest";
import { existsSync, readFileSync, readdirSync, statSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..", "..");

function* walk(dir: string): Generator<string> {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) {
      if (entry === "queue-proto" || entry === "node_modules") {
        continue;
      }
      yield* walk(full);
    } else {
      yield full;
    }
  }
}

/** Static import graph from an entry file — .ts/.tsx/.css leaves only. */
function reachableFrom(entry: string): string[] {
  const seen = new Set<string>();
  const stack = [entry];
  const importRe =
    /(?:import|export)[^'"]*?from\s+["']([^"']+)["']|import\s*\(\s*["']([^"']+)["']|import\s*["']([^"']+)["']/g;
  while (stack.length > 0) {
    const file = stack.pop()!;
    if (seen.has(file) || !existsSync(file)) {
      continue;
    }
    seen.add(file);
    const text = readFileSync(file, "utf8");
    for (const m of text.matchAll(importRe)) {
      const spec = m[1] ?? m[2] ?? m[3];
      if (!spec?.startsWith(".")) {
        continue;
      }
      const base = resolve(dirname(file), spec);
      for (const cand of [base, `${base}.ts`, `${base}.tsx`, `${base}.css`, `${base}/index.ts`, `${base}/index.tsx`]) {
        if (existsSync(cand) && /\.(ts|tsx|css)$/.test(cand)) {
          stack.push(cand);
        }
      }
    }
  }
  return [...seen];
}

describe("production isolation", () => {
  it("the production board entry is the queue app; fleet is unchanged", () => {
    const viteConfig = readFileSync(join(root, "vite.config.ts"), "utf8");
    // /ui/queue serves the queue app built from src/queue-proto/main.tsx —
    // live board-API data, not the disconnected board app.
    expect(viteConfig).toContain('board: resolve(root, "src/queue-proto/main.tsx")');
    expect(viteConfig).toContain('fleet: resolve(root, "src/main.tsx")');
  });

  it("no production source imports or references queue-proto", () => {
    const prodSrc = join(root, "src");
    for (const file of walk(prodSrc)) {
      if (!/\.(ts|tsx|css)$/.test(file)) {
        continue;
      }
      const content = readFileSync(file, "utf8");
      expect(content, `${file} references queue-proto`).not.toContain("queue-proto");
    }
  });

  it("the production queue entry never reaches the fixture modules", () => {
    // The whole point of the live wiring: synthetic rows cannot ride the
    // production bundle. fixtures.ts/rng.ts and the preview-only entry must
    // stay outside the static import graph of src/queue-proto/main.tsx.
    const reachable = reachableFrom(join(root, "src", "queue-proto", "main.tsx"));
    expect(reachable.length).toBeGreaterThan(5); // graph walk actually ran
    const leaked = reachable.filter((f) => /fixtures\.ts$|rng\.ts$|preview\.tsx$/.test(f));
    expect(leaked).toEqual([]);
    // and it really is wired to the board BFF
    expect(reachable.some((f) => f.endsWith(join("src", "board", "api.ts")))).toBe(true);
  });
});
