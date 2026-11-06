import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { Chip, LANE_CHIP_MAX } from "./Chip";
import { Mono } from "./Mono";
import { Icon, ICON_SHAPES, IconButton, type IconName } from "../Icon";
import { CHIP_GROUP_ORDER, statusGroup } from "../states";

describe("statusGroup", () => {
  it.each([
    [{ state: "developing", awaiting_decision: true }, "decision"],
    [{ state: "needs_decision" }, "decision"],
    [{ state: "developing", stalled: true }, "failed"],
    [{ state: "triage" }, "input"],
    [{ state: "researching" }, "working"],
    [{ state: "developing" }, "working"],
    [{ state: "reviewing" }, "working"],
    [{ state: "merged" }, "done"],
    [{ state: "deployed" }, "done"],
    [{ state: "completed" }, "done"],
    [{ state: "backlog" }, "neutral"],
    [{ state: "unstarted" }, "neutral"],
    [{ state: "icebox" }, "neutral"],
    [{ state: "dropped" }, "neutral"],
    [{ state: "duplicate" }, "neutral"],
    [{ state: "a-state-from-the-future" }, "neutral"],
  ] as const)("%j is %s", (task, group) => {
    expect(statusGroup(task)).toBe(group);
  });

  it("an open decision outranks a stall", () => {
    expect(statusGroup({ state: "developing", awaiting_decision: true, stalled: true })).toBe("decision");
  });

  it("lists the badge order decision > input > working > failed > done", () => {
    expect(CHIP_GROUP_ORDER.slice(0, 5)).toEqual(["decision", "input", "working", "failed", "done"]);
  });
});

describe("Chip", () => {
  it.each([
    [{ state: "triage" }, "분류 대기", "input"],
    [{ state: "developing" }, "개발 중", "working"],
    [{ state: "completed" }, "완료됨", "done"],
    [{ state: "backlog" }, "대기", "neutral"],
    [{ state: "developing", awaiting_decision: true }, "개발 중", "decision"],
    [{ state: "developing", stalled: true }, "개발 중", "failed"],
  ] as const)("status %j prints its words, not only a color", (task, text, group) => {
    const { container } = render(<Chip kind="status" {...task} />);
    const chip = container.querySelector(".hk-chip");
    expect(chip?.textContent).toBe(text);
    expect(chip?.className).toContain(`hk-chip--${group}`);
    expect(chip?.tagName).toBe("SPAN");
    // The icon beside the text is decorative.
    expect(chip?.querySelector("svg")?.getAttribute("aria-hidden")).toBe("true");
  });

  it("status takes an explicit label", () => {
    render(<Chip kind="status" state="developing" awaiting_decision label="결정 대기" />);
    expect(screen.getByText("결정 대기")).toBeTruthy();
  });

  it("priority prints P<n>, and an urgent one adds an icon besides the color", () => {
    const { container, rerender } = render(<Chip kind="priority" value={2} />);
    expect(container.textContent).toBe("P2");
    expect(container.querySelector("svg")).toBeNull();
    rerender(<Chip kind="priority" value={1} urgent />);
    expect(container.textContent).toBe("P1");
    expect(container.querySelector("svg")).not.toBeNull();
    expect(container.querySelector(".hk-chip--priority-urgent")).not.toBeNull();
  });

  it("lane prints a short name whole and cuts a long one but keeps it readable to assistive technology", () => {
    const { container, rerender } = render(<Chip kind="lane" lane="handoffkeep" />);
    expect(container.textContent).toBe("handoffkeep");
    const long = "a-very-long-lane-name-that-overflows";
    rerender(<Chip kind="lane" lane={long} />);
    const visible = container.querySelector('[aria-hidden="true"]');
    expect(visible?.textContent).toBe(`${long.slice(0, LANE_CHIP_MAX)}…`);
    expect(screen.getByText(long).className).toBe("hk-sr-only");
  });

  it("a Korean lane is cut by character, not by UTF-16 unit", () => {
    const lane = "가".repeat(LANE_CHIP_MAX + 3);
    const { container } = render(<Chip kind="lane" lane={lane} />);
    expect(container.querySelector('[aria-hidden="true"]')?.textContent).toBe(`${"가".repeat(LANE_CHIP_MAX)}…`);
  });

  it("count hides the digits from assistive technology and speaks the label", () => {
    const { container } = render(<Chip kind="count" group="decision" count={3} label="열린 결정 3건" />);
    expect(container.querySelector('[aria-hidden="true"]')?.textContent).toBe("3");
    expect(screen.getByText("열린 결정 3건").className).toBe("hk-sr-only");
  });
});

describe("Mono", () => {
  it("wraps text in the mono style", () => {
    render(<Mono>abc1234</Mono>);
    expect(screen.getByText("abc1234").className).toBe("hk-mono");
  });
});

describe("Icon", () => {
  const names = Object.keys(ICON_SHAPES) as IconName[];

  it("has the icons the design system names", () => {
    for (const n of ["caret-down", "x", "arrow-square-out", "copy", "magnifying-glass", "funnel", "check", "warning"]) {
      expect(names).toContain(n);
    }
  });

  it("every icon is a distinct shape", () => {
    const html = names.map((n) => render(<Icon name={n} />).container.innerHTML);
    expect(new Set(html).size).toBe(names.length);
  });

  it("is decorative unless it has a label", () => {
    const { container, rerender } = render(<Icon name="check" />);
    const svg = () => container.querySelector("svg");
    expect(svg()?.getAttribute("aria-hidden")).toBe("true");
    expect(svg()?.getAttribute("role")).toBeNull();
    rerender(<Icon name="check" label="복사함" />);
    expect(svg()?.getAttribute("aria-hidden")).toBeNull();
    expect(screen.getByRole("img", { name: "복사함" })).toBeTruthy();
  });

  it("uses the 1.75 stroke of the state shapes", () => {
    const { container } = render(<Icon name="funnel" />);
    expect(container.querySelector("svg")?.getAttribute("stroke-width")).toBe("1.75");
  });

  it("IconButton is named by its label and its icon is hidden", () => {
    render(<IconButton icon="x" label="lane 필터 해제" />);
    const button = screen.getByRole("button", { name: "lane 필터 해제" });
    expect(button.getAttribute("type")).toBe("button");
    expect(button.querySelector("svg")?.getAttribute("aria-hidden")).toBe("true");
  });
});
