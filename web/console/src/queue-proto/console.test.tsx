import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ConsoleBoard } from "./ConsoleBoard";
import { PageShell, navigate } from "../shared/AppShell";
import { fetchBoardTasks } from "../shared/boardApi";

const task = {
  id: 101,
  title: "Example task",
  lane: "example-project",
  kind: "build",
  state: "hold",
  priority: 70,
  created_by: "example",
  created_at: "2026-11-01T00:00:00Z",
  updated_at: "2026-11-01T00:00:00Z",
  refs: {},
};
const page = {
  generated_at: "2026-11-01T00:00:00Z",
  tasks: [task],
  states: ["hold", "needs_decision"],
  truncated: false,
};
const live = {
  generated_at: "",
  jobs: { status: "ok", fetched_at: "", items: [] },
  nodes: { status: "ok", fetched_at: "", items: [] },
  links: [],
  mismatch: {
    basis: "unavailable",
    tasks_without_job: [],
    jobs_without_task: [],
  },
};
beforeEach(() => {
  window.history.replaceState(null, "", "/ui/queue");
  localStorage.clear();
  vi.stubGlobal(
    "fetch",
    vi.fn(
      async (input: string) =>
        new Response(
          JSON.stringify(
            input.startsWith("/ui/api/board/tasks")
              ? page
              : input === "/ui/api/live"
                ? live
                : { generated_at: "", services: [], pr_source: "" },
          ),
          { status: 200 },
        ),
    ),
  );
  HTMLDialogElement.prototype.showModal = function () {
    this.setAttribute("open", "");
  };
  HTMLDialogElement.prototype.close = function () {
    this.removeAttribute("open");
  };
});
afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});
it("renders exact state groups and keeps absent execution fields explicit", async () => {
  render(<ConsoleBoard />);
  expect(await screen.findByText("Example task")).toBeTruthy();
  expect(screen.getByText("Host not collected")).toBeTruthy();
  expect(screen.getByText("Model not collected")).toBeTruthy();
  const row = screen.getByText("Example task").closest("a");
  expect(row?.getAttribute("href")).toBe("/ui/tasks/101");
  fireEvent.keyDown(document.body, { key: "j" });
  expect(document.activeElement).toBe(row?.closest("article"));
  fireEvent.change(screen.getByLabelText("Lane"), {
    target: { value: "example-project" },
  });
  await waitFor(() =>
    expect(new URLSearchParams(location.search).get("lane")).toBe(
      "example-project",
    ),
  );
});
it("requires explicit My work scope and preserves filters in the URL", async () => {
  history.replaceState(null, "", "/ui/queue?mine=1");
  render(<ConsoleBoard />);
  expect(await screen.findByText(/Select a lane or claimant/)).toBeTruthy();
  expect(screen.queryByText("Example task")).toBeNull();
  fireEvent.change(screen.getByLabelText("Lane"), {
    target: { value: "example-project" },
  });
  expect(await screen.findByText("Example task")).toBeTruthy();
});
it("walks pages before reporting a complete board and rejects malformed data", async () => {
  const fetcher = vi.mocked(fetch);
  fetcher
    .mockResolvedValueOnce(
      new Response(
        JSON.stringify({ ...page, truncated: true, next_after_id: 101 }),
      ),
    )
    .mockResolvedValueOnce(
      new Response(JSON.stringify({ ...page, tasks: [{ ...task, id: 102 }] })),
    );
  expect((await fetchBoardTasks()).tasks.map((t) => t.id)).toEqual([101, 102]);
  expect(fetcher.mock.calls[1][0]).toContain("after_id=101");
  fetcher.mockResolvedValueOnce(
    new Response(JSON.stringify({ ...page, tasks: [{ id: 1 }] })),
  );
  await expect(fetchBoardTasks()).rejects.toThrow("malformed board");
});
it("expires g prefixes and ignores letter shortcuts while typing", () => {
  const to = vi.spyOn(navigate, "to").mockImplementation(() => {});
  render(
    <PageShell current="home">
      <input aria-label="Editor" />
    </PageShell>,
  );
  fireEvent.keyDown(document.body, { key: "g" });
  fireEvent.keyDown(document.body, { key: "h" });
  expect(to).toHaveBeenCalledWith("/ui/fleet");
  to.mockClear();
  fireEvent.keyDown(screen.getByLabelText("Editor"), { key: "g" });
  fireEvent.keyDown(screen.getByLabelText("Editor"), { key: "d" });
  expect(to).not.toHaveBeenCalled();
  vi.spyOn(Date, "now").mockReturnValueOnce(1000).mockReturnValueOnce(2000);
  fireEvent.keyDown(document.body, { key: "g" });
  fireEvent.keyDown(document.body, { key: "d" });
  expect(to).not.toHaveBeenCalled();
});
it("opens the modal palette, searches real task results, and restores its trigger", async () => {
  const search = vi.fn(async () => ({
    query: "example",
    results: [
      {
        id: 101,
        title: "Example task",
        state: "hold",
        lane: "example-project",
        kind: "build",
        snippet: "",
      },
    ],
    hasMore: false,
    limit: 20,
  }));
  render(
    <PageShell current="home" search={search}>
      <p>Inbox content</p>
    </PageShell>,
  );
  const trigger = screen.getByRole("button", { name: /Search/ });
  trigger.focus();
  fireEvent.click(trigger);
  expect(screen.getByRole("dialog")).toBeTruthy();
  fireEvent.change(screen.getByLabelText("Search commands and tasks"), {
    target: { value: "example" },
  });
  expect(
    await screen.findByRole("link", { name: /#101 Example task/ }),
  ).toBeTruthy();
  expect(search).toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "Esc" }));
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(document.activeElement).toBe(trigger);
});
it("shows errors rather than rendering malformed API data as empty", async () => {
  vi.mocked(fetch).mockResolvedValue(new Response("{}"));
  render(<ConsoleBoard />);
  await screen.findByRole("alert");
  expect(screen.getByRole("alert").textContent).toContain("Board unavailable");
});
