import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { QueueProtoApp } from "./QueueProtoApp";
import { buildDatasets } from "./fixtures";

const datasets = buildDatasets();

describe("zero network I/O", () => {
  beforeEach(() => {
    localStorage.clear();
  });

  it("all prototype interactions issue zero fetch/XHR calls", () => {
    const fetchSpy = vi.fn();
    vi.stubGlobal("fetch", fetchSpy);
    const xhrOpen = vi.fn();
    const xhrSend = vi.fn();
    vi.stubGlobal(
      "XMLHttpRequest",
      class {
        open = xhrOpen;
        send = xhrSend;
      },
    );

    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="sample200" />);
    // exercise every interactive surface
    for (const view of ["Operator", "Active", "Backlog", "All"]) {
      fireEvent.click(screen.getByRole("button", { name: view }));
    }
    fireEvent.change(screen.getByLabelText("search"), { target: { value: "drawer" } });
    fireEvent.change(screen.getByLabelText("lane filter"), { target: { value: "synth-lane-ops" } });
    fireEvent.click(screen.getByLabelText("search"));
    const row = container.querySelector<HTMLElement>("[data-task-id]")!;
    fireEvent.keyDown(row, { key: "Enter" });
    fireEvent.click(screen.getByRole("button", { name: "Next task" }));
    fireEvent.click(screen.getByRole("button", { name: "Close detail" }));
    fireEvent.click(screen.getByRole("button", { name: /group:/ }));
    const group = container.querySelector<HTMLElement>(".qp-group");
    if (group) {
      fireEvent.click(group);
    }
    fireEvent.click(screen.getByRole("button", { name: /density:/ }));

    expect(fetchSpy).not.toHaveBeenCalled();
    expect(xhrOpen).not.toHaveBeenCalled();
    expect(xhrSend).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
  });
});
