import { describe, expect, it } from "vitest";
import { readFileSync, readdirSync, statSync } from "node:fs";
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

describe("production isolation", () => {
  it("the production board entry is the queue prototype; fleet is unchanged", () => {
    const viteConfig = readFileSync(join(root, "vite.config.ts"), "utf8");
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
});
