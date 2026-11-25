import { describe, expect, it } from "vitest";
import { MAX_SCALE, MIN_SCALE, centred, clampScale, fitScale, naturalSize, zoomAt } from "./diagramView";

describe("naturalSize", () => {
  it("reads the viewBox, not the max-width Mermaid puts in the style", () => {
    expect(naturalSize('<svg id="x" width="100%" style="max-width: 900px;" viewBox="0 0 1234 567"></svg>')).toEqual({ width: 1234, height: 567 });
  });

  it("reads numeric width and height when there is no viewBox", () => {
    expect(naturalSize('<svg width="320px" height="200"></svg>')).toEqual({ width: 320, height: 200 });
  });

  it("falls back when the drawing states no size", () => {
    expect(naturalSize('<svg width="100%"></svg>')).toEqual({ width: 800, height: 600 });
  });
});

describe("zoom bounds", () => {
  it("clamps to the 10%–800% range", () => {
    expect(clampScale(100, 1)).toBe(MAX_SCALE);
    expect(clampScale(0.001, 1)).toBe(MIN_SCALE);
    expect(clampScale(2, 1)).toBe(2);
  });

  it("widens the range to a fit scale outside it", () => {
    expect(clampScale(0.01, 0.05)).toBe(0.05);
    expect(clampScale(100, 12)).toBe(12);
  });
});

describe("view", () => {
  it("fits the whole image inside the stage, centred", () => {
    const size = { width: 400, height: 300 };
    const box = { width: 800, height: 600 };
    const scale = fitScale(size, box);
    expect(scale).toBe(2);
    expect(centred(size, box, scale)).toEqual({ scale: 2, x: 0, y: 0 });
  });

  it("zooms about a point and keeps the content under it still", () => {
    const view = { scale: 1, x: 0, y: 0 };
    const point = { x: 100, y: 50 };
    const zoomed = zoomAt(view, 2, point, 1);
    expect(zoomed).toEqual({ scale: 2, x: -100, y: -50 });
    // The content under the point (100, 50) is still under it.
    expect((point.x - zoomed.x) / zoomed.scale).toBe(100);
    expect((point.y - zoomed.y) / zoomed.scale).toBe(50);
  });
});
