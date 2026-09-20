import { beforeEach, describe, expect, it } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { QueueProtoApp } from "./QueueProtoApp";
import { buildDatasets } from "./fixtures";
import { applyView, EMPTY_FILTERS, groupByArea } from "./adapter";
import { flattenGrouped } from "./ListView";

const datasets = buildDatasets();
const edge = datasets.edge;

function openRow(container: HTMLElement, id: number): HTMLElement {
  const row = container.querySelector<HTMLElement>(`[data-task-id="${id}"]`)!;
  fireEvent.keyDown(row, { key: "Enter" });
  return row;
}

function ddValue(drawer: HTMLElement, dtLabel: string): string {
  const dts = [...drawer.querySelectorAll("dt")];
  const dt = dts.find((d) => d.textContent?.trim() === dtLabel);
  return dt?.nextElementSibling?.textContent?.trim() ?? "<missing>";
}

describe("shared detail drawer", () => {
  beforeEach(() => {
    localStorage.clear();
  });

  it("Enter opens the focused row; drawer shows the full unclamped title", () => {
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="edge" />);
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    openRow(container, 5011);
    const drawer = screen.getByRole("dialog");
    const task = edge.tasks.find((t) => t.id === 5011)!;
    expect(within(drawer).getByText(task.title)).toBeTruthy();
    expect(within(drawer).getByText(/source status:/).textContent).toContain("synthetic fixture");
    expect(within(drawer).getByText("CLAMPED-TAIL-MARKER-5011", { exact: false })).toBeTruthy();
  });

  it("Escape closes and focus returns to the originating row", () => {
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="edge" />);
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    const row = openRow(container, 5005);
    const drawer = screen.getByRole("dialog");
    expect(document.getElementById("qp-main")!.hasAttribute("inert")).toBe(true);
    fireEvent.keyDown(drawer, { key: "Escape" });
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.getElementById("qp-main")!.hasAttribute("inert")).toBe(false);
    expect(document.activeElement).toBe(row);
  });

  it("visible close control closes and returns focus", () => {
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="edge" />);
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    const row = openRow(container, 5005);
    fireEvent.click(screen.getByRole("button", { name: "Close detail" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(row);
  });

  it("prev/next navigation works via buttons and arrow keys", () => {
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="edge" />);
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    // drawer prev/next follows the displayed (grouped) list order
    const flat = flattenGrouped(groupByArea(applyView(edge.tasks, { view: "all", filters: EMPTY_FILTERS }), edge.enrichment, edge.generatedAt), []);
    const ordered = flat.filter((r) => r.kind === "task").map((r) => (r.kind === "task" ? r.task.id : -1));
    const start = ordered[1];
    openRow(container, start);
    const drawer = screen.getByRole("dialog");
    // prev via button
    fireEvent.click(within(drawer).getByRole("button", { name: "Previous task" }));
    expect(screen.getByRole("dialog").getAttribute("aria-label")).toContain(`task ${ordered[0]}`);
    // next via ArrowDown (twice: back to start, then forward)
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "ArrowDown" });
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "ArrowDown" });
    expect(screen.getByRole("dialog").getAttribute("aria-label")).toContain(`task ${ordered[2]}`);
    // prev via ArrowUp
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "ArrowUp" });
    expect(screen.getByRole("dialog").getAttribute("aria-label")).toContain(`task ${ordered[1]}`);
  });

  it("renders unavailable due/blocker as unknown — not blank, not zero", () => {
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="edge" />);
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    openRow(container, 5003);
    const drawer = screen.getByRole("dialog") as HTMLElement;
    expect(ddValue(drawer, "due")).toBe("unknown");
    expect(ddValue(drawer, "blocker")).toBe("unknown");
    expect(ddValue(drawer, "due")).not.toBe("0");
    expect(ddValue(drawer, "due")).not.toBe("");
  });

  it("renders unknown current-state age distinctly", () => {
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="edge" />);
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    openRow(container, 5001);
    const drawer = screen.getByRole("dialog") as HTMLElement;
    expect(ddValue(drawer, "current-state age")).toContain("unknown");
    expect(ddValue(drawer, "current-state age")).toContain("state_entered_at");
    // a normal task renders a numeric age instead
    fireEvent.keyDown(drawer, { key: "Escape" });
    openRow(container, 5005);
    expect(ddValue(screen.getByRole("dialog") as HTMLElement, "current-state age")).toMatch(/^\d+d/);
  });

  it("missing coverage reads unknown; collected zero reads 0", () => {
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="edge" />);
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    openRow(container, 5002);
    let drawer = screen.getByRole("dialog") as HTMLElement;
    expect(drawer.textContent).toContain("unknown — not collected");
    expect(within(drawer).queryByTestId("participant-count")).toBeNull();
    fireEvent.keyDown(drawer, { key: "Escape" });
    openRow(container, 5014);
    drawer = screen.getByRole("dialog") as HTMLElement;
    expect(within(drawer).getByTestId("participant-count").textContent).toBe("0");
    expect(drawer.textContent).not.toContain("not collected");
  });

  it("unknown lane/claimant renders unknown, not blank", () => {
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="edge" />);
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    openRow(container, 5013);
    const drawer = screen.getByRole("dialog") as HTMLElement;
    expect(ddValue(drawer, "lane")).toBe("unknown");
    expect(ddValue(drawer, "claimant")).toBe("unknown");
  });

  it("shows relation evidence for the duplicate and implement/verify pairs", () => {
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="edge" />);
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    openRow(container, 5007);
    expect(screen.getByRole("dialog").textContent).toContain("duplicate-candidate → #5008");
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    openRow(container, 5009);
    expect(screen.getByRole("dialog").textContent).toContain("verifies → #5010");
  });
});
