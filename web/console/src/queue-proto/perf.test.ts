import { describe, expect, it } from "vitest";
import { percentile } from "./perf";

describe("perf helpers", () => {
  it("percentile picks the nearest-rank element", () => {
    const sorted = [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20];
    expect(percentile(sorted, 50)).toBe(10);
    expect(percentile(sorted, 95)).toBe(19);
    expect(percentile(sorted, 100)).toBe(20);
    expect(percentile([], 95)).toBe(0);
  });
});
