import { render, screen, within } from "@testing-library/react";
import { expect, it, vi, afterEach } from "vitest";
import { ExecutionChips } from "./ExecutionChips";
import type { BoardTask, BoardWorkSession } from "../shared/boardTypes";
import { liveRequestFailed, type LiveResponse } from "../live";
import { fetchBoardTasks } from "../shared/boardApi";

const at = "2026-11-01T12:00:00Z";
const lead: BoardWorkSession = {
  job_id: "example-lead",
  host: "example-a",
  role: "lead",
  model: "Example builder model",
  family: "openai",
  active: false,
  at,
};
const review: BoardWorkSession = {
  ...lead,
  job_id: "example-review",
  host: "example-b",
  role: "review",
  model: "Example reviewer model",
  family: "anthropic",
  active: true,
};
const task: BoardTask = {
  id: 101,
  title: "Example work",
  lane: "example-project",
  kind: "implement",
  state: "developing",
  priority: 70,
  created_by: "example-operator",
  created_at: at,
  updated_at: at,
  refs: {},
  work_board: {
    primary_job_id: lead.job_id,
    pr: "https://github.com/example/project/pull/42",
    head_sha: "example-head",
    sessions: [
      review,
      lead,
      {
        ...review,
        job_id: "example-consult",
        role: "consult",
        model: "Example consultant model",
      },
    ],
  },
};
const live: LiveResponse = {
  ...liveRequestFailed(),
  jobs: {
    status: "ok",
    fetched_at: at,
    items: [
      {
        job_id: "example-lead",
        machine: "example-observed",
        role: "worker",
        tier: "premium",
      },
    ],
  },
  links: [
    { task_id: 101, job_id: "example-lead", job_found: true, children: [] },
  ],
};
afterEach(() => vi.unstubAllGlobals());
it("keeps the lead inline, preserves every active session, and links the projected PR", () => {
  const { container } = render(
    <ExecutionChips task={task} live={liveRequestFailed()} />,
  );
  expect(screen.getByText("example-a")).toBeTruthy();
  expect(screen.getByLabelText("Role lead: Builder")).toBeTruthy();
  expect(screen.getByText("Example builder model")).toBeTruthy();
  expect(screen.queryByText("Model not collected")).toBeNull();
  expect(screen.getByRole("link", { name: "PR 42" }).getAttribute("href")).toBe(
    task.work_board?.pr,
  );
  const disclosure = screen.getByText("+2 sessions").closest("details");
  expect(disclosure).toBeTruthy();
  const details = container.querySelector("details")!;
  expect(within(details).getByText("Example reviewer model")).toBeTruthy();
  expect(
    within(details).getByLabelText("Role consult: Consultant"),
  ).toBeTruthy();
  expect(container.firstElementChild?.textContent).toBe("example-a");
  expect(container.textContent).toContain("Recorded placement");
  expect(container.textContent).not.toContain("Observed job");
});
it("distinguishes a recorded chosen_host from observed execution without overwriting the canonical role", () => {
  render(<ExecutionChips task={task} live={live} />);
  expect(screen.getByText("example-a")).toBeTruthy();
  expect(screen.getByText("Observed job on example-observed")).toBeTruthy();
  expect(screen.getByLabelText("Role lead: Builder")).toBeTruthy();
  expect(screen.getByText("Example builder model")).toBeTruthy();
});
it("falls back for old servers without guessing a model from live tier", () => {
  const legacy = {
    ...task,
    work_board: undefined,
    refs: { pr: "https://github.com/example/project/pull/43" },
  };
  render(<ExecutionChips task={legacy} live={live} />);
  expect(screen.getByText("example-observed")).toBeTruthy();
  expect(screen.getByLabelText("Role worker: Worker")).toBeTruthy();
  expect(screen.getByText("Model not collected")).toBeTruthy();
  expect(screen.queryByText("premium")).toBeNull();
  expect(screen.getByRole("link", { name: "PR 43" })).toBeTruthy();
});
it("does not label cached jobs as currently observed", () => {
  render(
    <ExecutionChips
      task={task}
      live={{ ...live, jobs: { ...live.jobs, status: "timeout" } }}
    />,
  );
  expect(
    screen.getByText("Last observed job · timeout on example-observed"),
  ).toBeTruthy();
});
it("does not turn an unsafe projected PR into a link", () => {
  const board = task.work_board!;
  render(
    <ExecutionChips
      task={{ ...task, work_board: { ...board, pr: "javascript:alert(1)" } }}
      live={liveRequestFailed()}
    />,
  );
  expect(screen.queryByRole("link")).toBeNull();
  expect(screen.getByText("PR javascript:alert(1)")).toBeTruthy();
});
it("accepts the new BFF fields and rejects malformed sessions at the read boundary", async () => {
  const response = {
    generated_at: at,
    states: ["developing"],
    tasks: [task],
    truncated: false,
  };
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(JSON.stringify(response))),
  );
  const board = await fetchBoardTasks();
  expect(board.tasks[0].work_board?.sessions[0].model).toBe(
    "Example reviewer model",
  );
  vi.mocked(fetch).mockResolvedValueOnce(
    new Response(
      JSON.stringify({
        ...response,
        tasks: [
          {
            ...task,
            work_board: {
              ...task.work_board,
              sessions: [{ ...lead, model: 42 }],
            },
          },
        ],
      }),
    ),
  );
  await expect(fetchBoardTasks()).rejects.toThrow("malformed board response");
});
