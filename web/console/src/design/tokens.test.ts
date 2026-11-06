// The design tokens, checked from the stylesheet itself: every text and chip
// pair meets WCAG AA (4.5:1) in both themes, and every var(--hk-*) a source
// file reads is defined. The CSS is parsed here, so the check cannot drift from
// the values that ship.

import { readdirSync, readFileSync, statSync } from "node:fs";
import { describe, expect, it } from "vitest";

const TOKENS = readFileSync("src/design/tokens.css", "utf8").replace(/\/\*[\s\S]*?\*\//g, "");

type Theme = "dark" | "light";

function declarations(selector: RegExp): Map<string, string> {
  const out = new Map<string, string>();
  for (const block of TOKENS.matchAll(selector)) {
    for (const d of block[1].matchAll(/(--hk-[a-z0-9-]+)\s*:\s*([^;]+);/g)) out.set(d[1], d[2].trim());
  }
  return out;
}

// CSS custom properties resolve at the element, so the light theme is the dark
// declarations overlaid with the light ones, then resolved together.
const DARK = declarations(/(?:^|\n)\s*:root\s*\{([^{}]*)\}/g);
const LIGHT = new Map([...DARK, ...declarations(/:root\[data-theme="light"\]\s*\{([^{}]*)\}/g)]);
const THEMES: Record<Theme, Map<string, string>> = { dark: DARK, light: LIGHT };

type RGB = [number, number, number];

function hex(value: string): RGB {
  const h = value.replace("#", "");
  const full = h.length === 3 ? [...h].map((c) => c + c).join("") : h;
  return [0, 2, 4].map((i) => parseInt(full.slice(i, i + 2), 16)) as RGB;
}

function resolve(theme: Theme, value: string, depth = 0): RGB {
  if (depth > 12) throw new Error(`token cycle at ${value}`);
  const v = value.trim();
  if (v.startsWith("#")) return hex(v);
  const ref = /^var\((--hk-[a-z0-9-]+)\)$/.exec(v);
  if (ref) {
    const next = THEMES[theme].get(ref[1]);
    if (next === undefined) throw new Error(`undefined token ${ref[1]}`);
    return resolve(theme, next, depth + 1);
  }
  const mix = /^color-mix\(in srgb,\s*(.+?)\s+(var\(--hk-[a-z0-9-]+\)|[0-9.]+%)\s*,\s*(.+)\)$/.exec(v);
  if (mix) {
    const pct = mix[2].startsWith("var(") ? THEMES[theme].get(mix[2].slice(4, -1)) : mix[2];
    const p = parseFloat(pct ?? "NaN") / 100;
    if (Number.isNaN(p)) throw new Error(`bad mix percentage in ${v}`);
    const a = resolve(theme, mix[1], depth + 1);
    const b = resolve(theme, mix[3], depth + 1);
    return a.map((c, i) => Math.round(c * p + b[i] * (1 - p))) as RGB;
  }
  throw new Error(`cannot resolve ${v}`);
}

const color = (theme: Theme, token: string): RGB => resolve(theme, `var(--hk-${token})`);

function luminance([r, g, b]: RGB): number {
  const lin = (c: number) => {
    const s = c / 255;
    return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
}

export function contrast(a: RGB, b: RGB): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

const AA = 4.5;
const SURFACES = ["surface-0", "surface-1", "surface-2", "surface-selected"];
const CHIP_GROUPS = ["decision", "input", "working", "failed", "done", "neutral"];

describe.each(["dark", "light"] as const)("%s theme contrast", (theme) => {
  const rows: [string, string, string][] = [];
  for (const surface of SURFACES) {
    for (const text of ["text-primary", "text-heading", "text-secondary", "text-muted"]) rows.push([text, surface, text]);
    for (const state of ["success", "warning", "danger", "info"]) rows.push([state, surface, state]);
  }
  for (const group of CHIP_GROUPS) rows.push([`chip-${group}-fg`, `chip-${group}-bg`, group]);
  rows.push(["action-fg", "action-bg", "action"], ["accent-fg", "accent", "accent"]);

  it.each(rows)("%s on %s is at least 4.5:1", (fg, bg) => {
    const ratio = contrast(color(theme, fg), color(theme, bg));
    expect(ratio, `${fg} on ${bg} = ${ratio.toFixed(2)}`).toBeGreaterThanOrEqual(AA);
  });

  it("the control border is visible (3:1) on the page", () => {
    expect(contrast(color(theme, "border-control"), color(theme, "surface-0"))).toBeGreaterThanOrEqual(3);
  });

  it("every chip group has a text color and a fill", () => {
    for (const g of CHIP_GROUPS) {
      expect(THEMES[theme].has(`--hk-chip-${g}-fg`)).toBe(true);
      expect(THEMES[theme].has(`--hk-chip-${g}-bg`)).toBe(true);
    }
  });
});

describe("token set", () => {
  it("light overrides the chip text of every group", () => {
    const light = declarations(/:root\[data-theme="light"\]\s*\{([^{}]*)\}/g);
    for (const g of CHIP_GROUPS) expect(light.has(`--hk-chip-${g}-fg`)).toBe(true);
  });

  it("the smallest text is 12px and nothing falls back to the old 10px micro", () => {
    expect(DARK.get("--hk-type-meta")).toBe("12px");
    expect(resolve("dark", "#fff")).toEqual([255, 255, 255]);
    expect(DARK.get("--hk-type-micro")).toBe("var(--hk-type-meta)");
  });

  it("motion collapses under prefers-reduced-motion", () => {
    expect(TOKENS).toMatch(/@media \(prefers-reduced-motion: reduce\)\s*\{\s*:root\s*\{[^}]*--hk-motion-fast:\s*0ms/);
  });

  it("the UI font leads with Pretendard and the mono font with JetBrains Mono", () => {
    expect(DARK.get("--hk-font-ui")).toMatch(/^"Pretendard Variable"/);
    expect(DARK.get("--hk-font-mono")).toMatch(/^"JetBrains Mono"/);
  });
});

describe("token references", () => {
  const defined = new Set<string>();
  const files: string[] = [];
  const walk = (dir: string) => {
    for (const name of readdirSync(dir)) {
      const child = `${dir}/${name}`;
      if (statSync(child).isDirectory()) walk(child);
      else if (/\.(css|tsx?)$/.test(name) && !/\.test\./.test(name)) files.push(child);
    }
  };
  walk("src");
  for (const f of files) for (const d of readFileSync(f, "utf8").matchAll(/(--hk-[a-z0-9-]+)\s*:/g)) defined.add(d[1]);

  it("every var(--hk-*) read in src is defined somewhere", () => {
    const missing: string[] = [];
    for (const f of files) {
      for (const m of readFileSync(f, "utf8").matchAll(/var\((--hk-[a-z0-9-]+)/g)) {
        if (!defined.has(m[1])) missing.push(`${f}: ${m[1]}`);
      }
    }
    expect(missing).toEqual([]);
  });
});
