// The markdown renderer is loaded only when a task detail first renders a
// body document (2410 §3 AC 7, 2402 Q2). These checks read the static import
// graph of the queue entry — in source and in the committed build output —
// and the size of what the first detail open adds.

import { describe, expect, it } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { gzipSync } from "node:zlib";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..", "..");
const built = resolve(root, "..", "..", "internal", "ui", "static", "console");

const RENDERER_PACKAGES = /^(react-markdown|remark-|rehype-|micromark|mdast-|hast-|unified|unist-)/;
const BUDGET_RAW = 200 * 1024;
const BUDGET_GZIP = 65 * 1024;

type Graph = { files: Set<string>; bare: Map<string, string[]> };

/** Source import graph from an entry. Static edges always; dynamic import()
 * edges only when asked. Bare specifiers are recorded per importing file. */
function sourceGraph(entry: string, withDynamic: boolean): Graph {
  const files = new Set<string>();
  const bare = new Map<string, string[]>();
  const stack = [entry];
  const staticRe = /(?:^|\n)\s*(?:import|export)\s[^'";]*?from\s*["']([^"']+)["']|(?:^|\n)\s*import\s*["']([^"']+)["']/g;
  const dynamicRe = /\bimport\s*\(\s*["']([^"']+)["']\s*\)/g;
  while (stack.length > 0) {
    const file = stack.pop()!;
    if (files.has(file) || !existsSync(file)) {
      continue;
    }
    files.add(file);
    const text = readFileSync(file, "utf8");
    const specs: string[] = [];
    for (const m of text.matchAll(staticRe)) {
      specs.push(m[1] ?? m[2]);
    }
    if (withDynamic) {
      for (const m of text.matchAll(dynamicRe)) {
        specs.push(m[1]);
      }
    }
    for (const spec of specs) {
      if (!spec.startsWith(".")) {
        bare.set(file, [...(bare.get(file) ?? []), spec]);
        continue;
      }
      const base = resolve(dirname(file), spec);
      for (const cand of [base, `${base}.ts`, `${base}.tsx`, `${base}/index.ts`, `${base}/index.tsx`]) {
        if (existsSync(cand) && /\.(ts|tsx)$/.test(cand)) {
          stack.push(cand);
        }
      }
    }
  }
  return { files, bare };
}

/** Relative chunk imports of one built file, split static vs dynamic. */
function chunkImports(name: string): { static: string[]; dynamic: string[] } {
  const text = readFileSync(join(built, name), "utf8");
  const dynamic = [...text.matchAll(/import\(\s*"\.\/([^"]+)"\s*\)/g)].map((m) => m[1]);
  // `from"./x"` and side-effect `import"./x"`; an import("./x") call has a
  // parenthesis before the quote and never matches here.
  const staticImports = [...text.matchAll(/(?:from|import)\s*"\.\/([^"]+)"/g)].map((m) => m[1]);
  return { static: staticImports, dynamic };
}

function staticClosure(entry: string): Set<string> {
  const seen = new Set<string>();
  const stack = [entry];
  while (stack.length > 0) {
    const name = stack.pop()!;
    if (seen.has(name)) {
      continue;
    }
    seen.add(name);
    stack.push(...chunkImports(name).static);
  }
  return seen;
}

const isRendererChunk = (name: string) => readFileSync(join(built, name), "utf8").includes("data-doc-renderer");

describe("renderer stays out of the list's initial load (mutant b)", () => {
  const entry = join(root, "src", "queue-proto", "main.tsx");

  it("source: the queue entry's static graph has no renderer module or package", () => {
    const graph = sourceGraph(entry, false);
    expect(graph.files.size).toBeGreaterThan(10); // the walk actually ran
    const leakedFiles = [...graph.files].filter((f) => /DocMarkdown\.tsx$|docrender\.ts$/.test(f));
    expect(leakedFiles).toEqual([]);
    const leakedPackages = [...graph.bare.entries()].flatMap(([file, specs]) =>
      specs.filter((s) => RENDERER_PACKAGES.test(s)).map((s) => `${file} → ${s}`),
    );
    expect(leakedPackages).toEqual([]);
    // Non-vacuous: through the dynamic edge the renderer is reachable.
    const full = sourceGraph(entry, true);
    expect([...full.files].some((f) => f.endsWith("DocMarkdown.tsx"))).toBe(true);
    expect([...full.bare.values()].flat()).toContain("react-markdown");
  });

  it("build: board.js's static chunk closure carries no renderer code", () => {
    const closure = staticClosure("board.js");
    expect(closure.has("shared-client.js")).toBe(true);
    for (const name of closure) {
      const text = readFileSync(join(built, name), "utf8");
      expect(text.includes("data-doc-renderer"), `${name} carries the renderer`).toBe(false);
      expect(text.includes("micromarkExtensions"), `${name} carries micromark`).toBe(false);
    }
    const lazy = chunkImports("board.js").dynamic;
    expect(lazy.filter(isRendererChunk)).toHaveLength(1);
  });

  it("build: the renderer chunk never imports an entry (stamped board.js?v= would load twice)", () => {
    const renderer = chunkImports("board.js").dynamic.find(isRendererChunk)!;
    for (const name of staticClosure(renderer)) {
      expect(["board.js", "fleet.js"], `${renderer} reaches ${name}`).not.toContain(name);
    }
  });

  it("build: first detail open adds at most raw 200KiB / gzip 65KiB", () => {
    const initial = staticClosure("board.js");
    const increment = new Set<string>();
    for (const lazy of chunkImports("board.js").dynamic) {
      for (const name of staticClosure(lazy)) {
        if (!initial.has(name)) {
          increment.add(name);
        }
      }
    }
    expect(increment.size).toBeGreaterThan(0);
    let raw = 0;
    let gzip = 0;
    for (const name of increment) {
      const bytes = readFileSync(join(built, name));
      raw += bytes.length;
      gzip += gzipSync(bytes, { level: 9 }).length;
    }
    expect(raw).toBeLessThanOrEqual(BUDGET_RAW);
    expect(gzip).toBeLessThanOrEqual(BUDGET_GZIP);
  });
});

// #618 budget choice (a): the drawer's decision card is its own lazy chunk.
// Only the row badge and the one-line count stay in the initial bundle.
const isDecisionChunk = (name: string) => readFileSync(join(built, name), "utf8").includes("data-decision-options");

describe("decision card stays out of the list's initial load (#618)", () => {
  const entry = join(root, "src", "queue-proto", "main.tsx");

  it("source: DecisionCard is reachable only through a dynamic import", () => {
    expect([...sourceGraph(entry, false).files].filter((f) => f.endsWith("DecisionCard.tsx"))).toEqual([]);
    expect([...sourceGraph(entry, true).files].some((f) => f.endsWith("DecisionCard.tsx"))).toBe(true);
  });

  it("build: exactly one lazy chunk carries the card, none of the initial closure does, and it never imports an entry", () => {
    for (const name of staticClosure("board.js")) {
      expect(isDecisionChunk(name), `${name} carries the decision card`).toBe(false);
    }
    const chunks = chunkImports("board.js").dynamic.filter(isDecisionChunk);
    expect(chunks).toHaveLength(1);
    for (const name of staticClosure(chunks[0])) {
      expect(["board.js", "fleet.js"], `${chunks[0]} reaches ${name}`).not.toContain(name);
    }
  });
});
