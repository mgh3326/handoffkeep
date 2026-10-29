import { describe, expect, it } from "vitest";
import { render } from "@testing-library/react";
import { DetailBody } from "./DetailDrawer";
import { RowFields } from "./TaskRow";
import { boardTaskToProto } from "./boardtask";
import type { BoardTask } from "../shared/boardTypes";
import { KNOWN_STATES, type Dataset, type ProtoTask } from "./types";

function mkBoardTask(over: Partial<BoardTask>): BoardTask {
  return {
    id: 9001,
    lane: "area-lane",
    title: "area test task",
    kind: "implement",
    state: "claimed",
    priority: 10,
    created_by: "director",
    created_at: "2026-09-21T10:00:00Z",
    updated_at: "2026-09-21T11:00:00Z",
    refs: {},
    ...over,
  };
}

const NOW = "2026-09-24T12:00:00Z";

function renderDrawer(task: ProtoTask) {
  const ds: Dataset = {
    key: "live",
    label: "live",
    source: "live",
    generatedAt: NOW,
    completeness: "complete",
    completenessNote: "test",
    states: [...KNOWN_STATES],
    tasks: [task],
    enrichment: {},
  };
  return render(<DetailBody dataset={ds} task={task} fetchDoc={() => Promise.reject(new Error("no docs"))} />);
}

describe("task area (read-only display)", () => {
  it("maps the board row's area; a null or absent area becomes null", () => {
    expect(boardTaskToProto(mkBoardTask({ area: "blender" })).area).toBe("blender");
    expect(boardTaskToProto(mkBoardTask({ area: null })).area).toBeNull();
    expect(boardTaskToProto(mkBoardTask({})).area).toBeNull();
  });

  it("list row shows the area as a plain label, and nothing when unset", () => {
    const tagged = render(<RowFields task={boardTaskToProto(mkBoardTask({ area: "web-qa" }))} now={NOW} />);
    const label = tagged.container.querySelector<HTMLElement>(".qp-area")!;
    expect(label.textContent).toBe("web-qa");
    expect(label.tagName).toBe("SPAN");
    expect(label.querySelector("a,button,input,select,textarea")).toBeNull();
    tagged.unmount();
    const untagged = render(<RowFields task={boardTaskToProto(mkBoardTask({ area: null }))} now={NOW} />);
    expect(untagged.container.querySelector(".qp-area")).toBeNull();
  });

  it("drawer lists the area in the record details with no control to change it", () => {
    const { container } = renderDrawer(boardTaskToProto(mkBoardTask({ area: "go-backend" })));
    const dts = [...container.querySelectorAll("dt")];
    const dt = dts.find((d) => d.textContent?.trim() === "task area")!;
    expect(dt.nextElementSibling?.textContent?.trim()).toBe("go-backend");
    expect(container.querySelector("dl.qp-drawer-meta")!.querySelectorAll("input,select,textarea,button,a").length).toBe(0);
  });

  it("drawer never offers a write path for area: no form control mentions it", () => {
    const { container } = renderDrawer(boardTaskToProto(mkBoardTask({ area: "docs" })));
    const controls = [...container.querySelectorAll("input,select,textarea,button,form")];
    for (const control of controls) {
      const text = `${control.getAttribute("name") ?? ""} ${control.getAttribute("aria-label") ?? ""} ${control.textContent ?? ""}`;
      expect(text.toLowerCase()).not.toContain("area");
    }
  });

  it("history labels an area event as classification, not a state change", () => {
    const task: ProtoTask = {
      ...boardTaskToProto(mkBoardTask({ area: "unity" })),
      events: [{ id: 5, kind: "area", from: "blender", to: "unity", by: "director-1", note: "retag", at: "2026-09-22T10:00:00Z" }],
    };
    const { container } = renderDrawer(task);
    const line = [...container.querySelectorAll("li")].find((li) => li.textContent?.includes("blender → unity"))!;
    expect(line.textContent).toContain("area: blender → unity by director-1");
  });
});
