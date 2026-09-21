import { beforeEach, describe, expect, it } from "vitest";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { HttpError } from "../board/api";
import type { BoardDetail } from "../board/types";
import { QueueProtoApp } from "./QueueProtoApp";
import { TaskPage } from "./TaskPage";
import { buildDatasets } from "./fixtures";

const datasets = buildDatasets();

function detailFor(id: number, title: string): BoardDetail {
  return {
    task: {
      id,
      lane: "ops",
      title,
      kind: "work",
      state: "claimed",
      priority: 1,
      created_by: "tester",
      created_at: "2026-09-20T00:00:00Z",
      updated_at: "2026-09-21T00:00:00Z",
      refs: {},
    },
    events: [],
    dwell: [],
    linear: null,
    participants: { task_ref: String(id), coverage: "collected", segments: [] },
  };
}

const searchParam = (name: string) => new URLSearchParams(window.location.search).get(name);

describe("task deep links (/ui/queue?task=<id>)", () => {
  beforeEach(() => {
    localStorage.clear();
    window.history.replaceState(null, "", "/ui/queue");
  });

  it("opens the panel for an in-dataset id at mount", () => {
    window.history.replaceState(null, "", "/ui/queue?view=all&task=5005");
    render(<QueueProtoApp datasets={datasets} initialSet="edge" />);
    expect(screen.getByRole("dialog").getAttribute("aria-label")).toContain("task 5005");
  });

  it("fetches single-task detail when the id is outside the dataset", async () => {
    window.history.replaceState(null, "", "/ui/queue?view=all&task=424242");
    render(<QueueProtoApp datasets={datasets} initialSet="edge" fetchDetail={async (id) => detailFor(id, "fetched task title")} />);
    await screen.findByText("fetched task title");
    expect(screen.getByRole("dialog").getAttribute("aria-label")).toContain("task 424242");
  });

  it("a nonexistent id renders a not-found state, not a blank panel", async () => {
    window.history.replaceState(null, "", "/ui/queue?view=all&task=424243");
    render(
      <QueueProtoApp
        datasets={datasets}
        initialSet="edge"
        fetchDetail={async () => {
          throw new HttpError(404);
        }}
      />,
    );
    await screen.findByText(/task #424243 not found/);
  });

  it("opening and closing a task syncs ?task= without dropping other params", () => {
    window.history.replaceState(null, "", "/ui/queue?view=all&group=area");
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="edge" />);
    fireEvent.click(container.querySelector<HTMLElement>('[data-task-id="5005"]')!);
    expect(searchParam("task")).toBe("5005");
    expect(searchParam("view")).toBe("all");
    expect(searchParam("group")).toBe("area");
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    expect(searchParam("task")).toBeNull();
    expect(searchParam("view")).toBe("all");
  });

  it("popstate restores the open/closed panel state from the URL", () => {
    window.history.replaceState(null, "", "/ui/queue?view=all&task=5005");
    render(<QueueProtoApp datasets={datasets} initialSet="edge" />);
    expect(screen.getByRole("dialog")).toBeTruthy();
    // jsdom does not navigate history entries, so Back is simulated by
    // replacing the URL and delivering the popstate the browser would fire.
    window.history.replaceState(null, "", "/ui/queue?view=all");
    act(() => {
      window.dispatchEvent(new window.Event("popstate"));
    });
    expect(screen.queryByRole("dialog")).toBeNull();
    window.history.replaceState(null, "", "/ui/queue?view=all&task=5011");
    act(() => {
      window.dispatchEvent(new window.Event("popstate"));
    });
    expect(screen.getByRole("dialog").getAttribute("aria-label")).toContain("task 5011");
  });

  it("panel head shows #<id> and a copy-link control targeting /ui/tasks/<id>", () => {
    window.history.replaceState(null, "", "/ui/queue?view=all");
    const { container } = render(<QueueProtoApp datasets={datasets} initialSet="edge" />);
    fireEvent.click(container.querySelector<HTMLElement>('[data-task-id="5005"]')!);
    const drawer = screen.getByRole("dialog");
    expect(within(drawer).getByRole("heading", { level: 3 }).textContent).toContain("#5005");
    const copy = within(drawer).getByRole("button", { name: "Copy task link" });
    expect(copy.getAttribute("title")).toContain("/ui/tasks/5005");
  });
});

describe("task page (/ui/tasks/<id>)", () => {
  it("renders the shared detail body for the fetched task", async () => {
    render(<TaskPage id={777} fetchDetail={async (id) => detailFor(id, "page task title")} />);
    await screen.findByText("page task title");
    expect(screen.getByRole("heading", { level: 3 }).textContent).toContain("#777");
    expect(screen.getByRole("button", { name: "Copy task link" })).toBeTruthy();
    expect(screen.getByRole("link", { name: "open in queue" }).getAttribute("href")).toBe("/ui/queue?task=777");
  });

  it("404 renders not-found, not a spinner", async () => {
    render(
      <TaskPage
        id={778}
        fetchDetail={async () => {
          throw new HttpError(404);
        }}
      />,
    );
    await screen.findByText(/task #778 not found/);
  });

  it("a transient failure renders error, not not-found", async () => {
    render(
      <TaskPage
        id={779}
        fetchDetail={async () => {
          throw new HttpError(500);
        }}
      />,
    );
    await screen.findByText(/detail fetch failed/);
  });
});
