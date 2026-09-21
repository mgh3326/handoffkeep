import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { QueueProtoApp } from "./QueueProtoApp";
import { buildDatasets } from "./fixtures";
import { STORAGE_KEY } from "./storage";

const datasets = buildDatasets();

function rowIds(container: HTMLElement, selector: string): Set<number> {
  return new Set([...container.querySelectorAll<HTMLElement>(selector)].map((el) => Number(el.dataset.taskId)));
}

describe("QueueProtoApp", () => {
  beforeEach(() => {
    localStorage.clear();
  });
  afterEach(() => {
    // Navigation state rides the URL — a test that left layout/view/task in it
    // would pollute every later render.
    window.history.replaceState(null, "", "/queue-proto.html");
  });

  it("labels the dataset as synthetic and shows the completeness status line", () => {
    render(<QueueProtoApp datasets={datasets} initialSet="sample200" />);
    expect(screen.getByText(/SYNTHETIC FIXTURE/)).toBeTruthy();
    const status = screen.getByTestId("status-line");
    expect(status.textContent).toContain("completeness: partial");
    expect(status.textContent).toContain("not the production backlog");
  });

  it("backlog defaults to list; active defaults to board; all defaults to list", () => {
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="sample200" />);
    // default = backlog + list
    expect(container.querySelectorAll(".qp-row").length).toBeGreaterThan(0);
    expect(container.querySelectorAll(".qp-col").length).toBe(0);

    fireEvent.click(screen.getByRole("link", { name: "Active" }));
    expect(container.querySelectorAll(".qp-col").length).toBeGreaterThan(0);
    expect(container.querySelectorAll(".qp-row").length).toBe(0);

    fireEvent.click(screen.getByRole("link", { name: "Backlog" }));
    expect(container.querySelectorAll(".qp-row").length).toBeGreaterThan(0);

    fireEvent.click(screen.getByRole("link", { name: "All" }));
    expect(container.querySelectorAll(".qp-row").length).toBeGreaterThan(0);
    expect(container.querySelectorAll(".qp-col").length).toBe(0);
  });

  it("board never shows all nine state columns by default", () => {
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="sample200" />);
    fireEvent.click(screen.getByRole("link", { name: "Active" }));
    const cols = container.querySelectorAll(".qp-col");
    expect(cols.length).toBeGreaterThan(0);
    expect(cols.length).toBeLessThan(9);
  });

  it("layout switch preserves the exact unique task-ID set", () => {
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="sample200" />);
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    const listSet = rowIds(container, ".qp-row");
    expect(listSet.size).toBe(200);
    fireEvent.click(within(container.querySelector("#qp-layout-toggle")!).getByRole("button", { name: "board" }));
    const boardSet = rowIds(container, ".qp-card");
    expect(boardSet).toEqual(listSet);
    fireEvent.click(within(container.querySelector("#qp-layout-toggle")!).getByRole("button", { name: "list" }));
    expect(rowIds(container, ".qp-row")).toEqual(listSet);
  });

  it("board cards render the reduced field set sized for the fixed row height", () => {
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="sample200" />);
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    fireEvent.click(within(container.querySelector("#qp-layout-toggle")!).getByRole("button", { name: "board" }));
    const card = container.querySelector(".qp-card")!;
    // Identity lines only: one age, no state cell — the column head already
    // names the state, and every extra line is clipped by the fixed-height
    // virtual row. Real fit is measured by evidence/assert-card-fit.mjs.
    expect(card.querySelectorAll(".qp-age").length).toBe(1);
    expect(card.querySelector(".qp-state")).toBeNull();
    expect(card.querySelector(".qp-title")).toBeTruthy();
    expect(card.querySelector(".qp-lane")).toBeTruthy();
  });

  it("applied filters show as chips and clear", () => {
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="sample200" />);
    fireEvent.change(screen.getByLabelText("lane filter"), { target: { value: "synth-lane-ops" } });
    const chip = screen.getByText(/lane: synth-lane-ops/);
    expect(chip).toBeTruthy();
    fireEvent.click(chip);
    expect(screen.queryByText(/lane: synth-lane-ops/)).toBeNull();
    expect(container.querySelectorAll(".qp-row").length).toBeGreaterThan(0);
  });

  it("empty filtered view offers an explicit route back to All", () => {
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="sample200" />);
    fireEvent.change(screen.getByLabelText("search"), { target: { value: "zzz-no-such-task-zzz" } });
    expect(container.querySelectorAll(".qp-row").length).toBe(0);
    const back = screen.getByRole("button", { name: "Show All view" });
    fireEvent.click(back);
    expect(container.querySelectorAll(".qp-row").length).toBe(200);
  });

  it("search by #id, numeric id, korean, and post-clamp text", () => {
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="sample200" />);
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    const search = screen.getByLabelText("search");
    fireEvent.change(search, { target: { value: "#1150" } });
    expect(rowIds(container, ".qp-row")).toEqual(new Set([1150]));
    fireEvent.change(search, { target: { value: "CLAMPED-TAIL-MARKER-1150" } });
    expect(rowIds(container, ".qp-row")).toEqual(new Set([1150]));
    fireEvent.change(search, { target: { value: "한영" } });
    expect(rowIds(container, ".qp-row").size).toBeGreaterThan(0);
  });

  it("saved-view version mismatch shows the explicit reset notice", () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify({ schemaVersion: 999, current: { view: "all" }, views: {} }));
    render(<QueueProtoApp datasets={datasets} initialSet="sample200" />);
    expect(screen.getByRole("alert").textContent).toContain("saved view reset — version mismatch");
  });

  it("measurement panel lists the fixed scripts incl. the iteration-1 trial set", async () => {
    const { EXPERIMENT_SCRIPTS } = await import("./MeasurePanel");
    expect(EXPERIMENT_SCRIPTS.map((s) => s.id)).toEqual(["U1", "U2", "U3", "U4", "U5", "T6", "T7", "I1", "I2", "I3", "I4", "I5"]);
  });
});
