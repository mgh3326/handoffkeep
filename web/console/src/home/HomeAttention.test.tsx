import { render, screen, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { HomeAttention } from "./HomeAttention";

const request = {
  source: "request",
  id: 41,
  lane: "desk",
  question: "Ship the console?",
  created_at: "2026-11-05T01:00:00Z",
  due_at: "2026-11-05T02:00:00Z",
  overdue: true,
};
const laneEvent = {
  source: "lane_event",
  id: 907,
  lane: "ops",
  question: "Which database?",
  created_at: "2026-11-05T03:00:00Z",
  due_at: null,
  overdue: false,
};
const waitingTask = {
  id: 12,
  title: "Review the plan",
  lane: "desk",
  state: "developing",
  reason: "awaiting_decision",
  since: "2026-11-04T10:00:00Z",
};
const stalledTask = { ...waitingTask, id: 13, title: "Idle build", reason: "stalled", state: "reviewing" };
const deploy = {
  service: "handoffkeep",
  task_id: 77,
  title: "Add the attention route",
  pr: "https://github.com/mgh3326/handoffkeep/pull/1",
  merged_at: "2026-11-05T04:00:00Z",
};

function payload(overrides: Record<string, unknown> = {}) {
  return {
    generated_at: "2026-11-05T05:00:00Z",
    decisions: { count: 3, items: [request, laneEvent] },
    tasks: { count: 2, awaiting_decision: 1, stalled: 1, items: [waitingTask, stalledTask] },
    deploys: { count: 1, events_capped: false, items: [deploy] },
    ...overrides,
  };
}

function serve(body: unknown) {
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(body))));
}

afterEach(() => vi.unstubAllGlobals());

it("renders a valid payload as three sections with counts and links", async () => {
  serve(payload());
  render(<HomeAttention />);
  const decisions = await screen.findByRole("region", { name: "Decisions" });
  const tasks = screen.getByRole("region", { name: "Tasks" });
  const deploys = screen.getByRole("region", { name: "Deploys" });

  expect(within(decisions).getByLabelText("3 Decisions").textContent).toBe("3");
  expect(within(tasks).getByLabelText("2 Tasks").textContent).toBe("2");
  expect(within(deploys).getByLabelText("1 Deploys").textContent).toBe("1");

  const href = (scope: HTMLElement, name: string | RegExp) =>
    within(scope).getByRole("link", { name }).getAttribute("href");
  expect(href(decisions, "Ship the console?")).toBe("/ui/decisions/41");
  expect(href(decisions, "Which database?")).toBe("/ui/decisions");
  expect(href(tasks, /Review the plan/)).toBe("/ui/tasks/12");
  expect(href(tasks, /Idle build/)).toBe("/ui/tasks/13");
  expect(href(deploys, /Add the attention route/)).toBe("/ui/tasks/77");
  expect(href(deploys, "Deploys")).toBe("/ui/deploys");

  // Three decisions are counted but two shown.
  expect(within(decisions).getByText("+1 more")).toBeTruthy();
  expect(within(tasks).queryByText(/more$/)).toBeNull();
  expect(within(tasks).getByText("1 awaiting · 1 stalled")).toBeTruthy();
  expect(within(decisions).getByText("overdue")).toBeTruthy();
  expect(screen.queryByText("Nothing is waiting on you.")).toBeNull();
});

it("renders the empty state when all three counts are 0", async () => {
  serve(
    payload({
      decisions: { count: 0, items: [] },
      tasks: { count: 0, awaiting_decision: 0, stalled: 0, items: [] },
      deploys: { count: 0, events_capped: false, items: [] },
    }),
  );
  render(<HomeAttention />);
  expect(await screen.findByText("Nothing is waiting on you.")).toBeTruthy();
  expect(screen.queryByRole("region")).toBeNull();
  expect(screen.queryByRole("alert")).toBeNull();
});

async function expectErrorNotEmpty(body: unknown) {
  serve(body);
  render(<HomeAttention />);
  expect(await screen.findByRole("alert")).toBeTruthy();
  expect(screen.queryByText("Nothing is waiting on you.")).toBeNull();
  expect(screen.queryByRole("region")).toBeNull();
}

it("shows the error state, not the empty state, when decisions is missing", async () => {
  const { decisions: _decisions, ...rest } = payload();
  await expectErrorNotEmpty(rest);
});

it("shows the error state, not the empty state, when tasks.count is a string", async () => {
  await expectErrorNotEmpty(
    payload({ tasks: { count: "0", awaiting_decision: 0, stalled: 0, items: [] } }),
  );
});

it("shows the error state, not the empty state, when an item has no id", async () => {
  const { id: _id, ...noId } = request;
  await expectErrorNotEmpty(payload({ decisions: { count: 1, items: [noId] } }));
});

it("shows the error state when the request fails", async () => {
  vi.stubGlobal("fetch", vi.fn(async () => new Response("{}", { status: 500 })));
  render(<HomeAttention />);
  expect(await screen.findByRole("alert")).toBeTruthy();
  expect(screen.queryByText("Nothing is waiting on you.")).toBeNull();
});
