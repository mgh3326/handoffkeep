import { expect, it } from "vitest";
import { readFileSync } from "node:fs";
const css = readFileSync("src/decisions/decisions.css", "utf8");

it("A15 interactive controls use the shared touch-height token", () => {
  for (const selector of [
    ".decision-options button",
    ".decision-page textarea",
    ".decision-page button",
    ".decision-page a",
    ".decision-page summary",
    ".decision-list button",
    ".decision-list a",
    ".decision-sheet button",
  ]) {
    const start = css.indexOf(selector);
    expect(start).toBeGreaterThanOrEqual(0);
    expect(css.slice(start, css.indexOf("}", start))).toContain(
      "var(--hk-touch-height)",
    );
  }
});
