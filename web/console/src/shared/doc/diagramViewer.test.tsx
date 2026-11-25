import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import DocMarkdown from "./DocMarkdown";

const mermaid = vi.hoisted(() => ({ initialize: vi.fn(), parse: vi.fn(), render: vi.fn() }));
vi.mock("mermaid", () => ({ default: mermaid }));

const erd = "```mermaid\nerDiagram\n  A ||--o{ B : has\n```";
// Mermaid's shape: a viewBox, and a max-width style the viewer must not use.
const drawn = '<svg viewBox="0 0 400 300" style="max-width: 400px;"><text>A</text></svg>';

beforeEach(() => {
  mermaid.initialize.mockReset();
  mermaid.parse.mockReset().mockResolvedValue(true);
  mermaid.render.mockReset().mockResolvedValue({ svg: drawn });
});

async function openViewer() {
  const { container } = render(<DocMarkdown body={erd} section={null} />);
  const open = await screen.findByRole("button", { name: "Open diagram" });
  fireEvent.click(open);
  return { open, container };
}

const viewerImage = () => screen.getByRole("dialog", { name: "Diagram viewer" }).querySelector("img.hk-diagram-viewer-image") as HTMLImageElement;
const scaleOf = () => Number(/scale\(([\d.]+)\)/.exec(viewerImage().style.transform)?.[1]);

describe("diagram viewer", () => {
  it("diagram viewer opens on click and on Enter, and closes on Esc and returns focus", async () => {
    const { open } = await openViewer();
    // Enter and Space activate a native button, so the diagram opens from the
    // keyboard too; jsdom does not synthesize that activation, so this asserts
    // the element is one.
    expect(open.tagName).toBe("BUTTON");
    expect(open.getAttribute("type")).toBe("button");
    expect(screen.getByRole("dialog", { name: "Diagram viewer" })).toBeTruthy();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Close" }));

    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(open);
  });

  it("diagram viewer closes on the close button and on a backdrop click, not a click in the stage", async () => {
    const { open } = await openViewer();
    const dialog = screen.getByRole("dialog");
    fireEvent.click(viewerImage());
    expect(screen.queryByRole("dialog")).not.toBeNull();

    fireEvent.pointerDown(dialog);
    fireEvent.click(dialog);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(open);
  });

  it("diagram viewer fit, zoom in and out, and 100% change the transform", async () => {
    await openViewer();
    const fit = scaleOf();
    expect(fit).toBeGreaterThanOrEqual(0.1);
    expect(fit).toBeLessThanOrEqual(8);

    fireEvent.click(screen.getByRole("button", { name: "Zoom in" }));
    expect(scaleOf()).toBeCloseTo(fit * 1.25);
    fireEvent.click(screen.getByRole("button", { name: "Zoom out" }));
    expect(scaleOf()).toBeCloseTo(fit);

    fireEvent.click(screen.getByRole("button", { name: "100%" }));
    expect(scaleOf()).toBe(1);
    expect(within(screen.getByRole("dialog")).getByRole("status").textContent).toBe("100%");

    fireEvent.click(screen.getByRole("button", { name: "Fit" }));
    expect(scaleOf()).toBeCloseTo(fit);

    for (let i = 0; i < 30; i++) fireEvent.click(screen.getByRole("button", { name: "Zoom in" }));
    expect(scaleOf()).toBe(8);
    for (let i = 0; i < 40; i++) fireEvent.click(screen.getByRole("button", { name: "Zoom out" }));
    expect(scaleOf()).toBeCloseTo(0.1);
  });

  it("diagram viewer keeps the svg as an image", async () => {
    const { container } = await openViewer();
    expect(container.querySelector('img[data-doc-diagram="svg"]')).not.toBeNull();
    expect(viewerImage().getAttribute("src")?.startsWith("data:image/svg+xml;charset=utf-8,")).toBe(true);
    expect(document.body.querySelector("svg")).toBeNull();
    // The viewer shows the drawing at its viewBox size, not the column's width.
    expect(viewerImage().style.width).toBe("400px");
    expect(viewerImage().style.height).toBe("300px");
  });
});
