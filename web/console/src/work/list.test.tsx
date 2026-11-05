import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { WorkPage } from "./WorkPage";

const task = { id: 101, title: "Task one", lane: "x", kind: "implement", state: "developing", priority: 2, created_by: "test", created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z", refs: {}, project: "a" };
const detail = { task, events: [], dwell: [], linear: null, participants: { task_ref: "hk:task/101", coverage: "not_collected", segments: [] }, origins: [], changes: [], jobs: [], origins_truncated: false, changes_truncated: false, jobs_truncated: false };
const response = (value: unknown) => new Response(JSON.stringify(value), { status: 200 });
let wide = false;
beforeEach(() => {
  wide = false;
  vi.stubGlobal("matchMedia", () => ({ get matches() { return wide; }, addEventListener() {}, removeEventListener() {} }));
  vi.stubGlobal("fetch", vi.fn(async (path: string) => path.includes("projects") ? response({ projects: ["a", "b"] }) : path.includes("/101") ? response(detail) : response({ tasks: [task], states: ["developing"], generated_at: "now", truncated: false })));
  history.replaceState(null, "", "/ui/work?project=a&view=active&q=%23101&lane=x&p=2");
});
afterEach(() => { vi.unstubAllGlobals(); history.replaceState(null, "", "/"); });
it("W3 restores controls and changes the URL", async () => {
  render(<WorkPage />);
  expect(await screen.findByText("#101 Task one")).toBeTruthy();
  expect((screen.getByLabelText("Search") as HTMLInputElement).value).toBe("#101");
  expect((screen.getByLabelText("Priority ≥") as HTMLInputElement).value).toBe("2");
  expect((screen.getByLabelText("lane") as HTMLSelectElement).value).toBe("x");
  fireEvent.change(screen.getByLabelText("Search"), { target: { value: "one" } });
  expect(new URLSearchParams(location.search).get("q")).toBe("one");
});
it("W10 uses a pane on wide screens and links on narrow screens", async () => {
  wide = true;
  const { unmount } = render(<WorkPage />);
  fireEvent.click(await screen.findByText("#101 Task one"));
  expect(new URLSearchParams(location.search).get("task")).toBe("101");
  expect(await screen.findByRole("heading", { name: "#101 Task one" })).toBeTruthy();
  history.replaceState(null, "", "/ui/work?project=a&view=active");
  act(() => window.dispatchEvent(new PopStateEvent("popstate")));
  await waitFor(() => expect(screen.queryByRole("heading", { name: "#101 Task one" })).toBeNull());
  unmount(); wide = false;
  render(<WorkPage />);
  expect((await screen.findByText("#101 Task one")).closest("a")?.getAttribute("href")).toBe("/ui/tasks/101");
});

it("W12 keeps the selected row and scroll after polling", async () => {
  wide = true;
  history.replaceState(null, "", "/ui/work?project=a&view=active&task=101");
  vi.useFakeTimers();
  const { container } = render(<WorkPage />);
  await act(async () => { await vi.advanceTimersByTimeAsync(0); });
  const list = container.querySelector(".work-list");
  if (!list) throw new Error("missing list");
  list.scrollTop = 120;
  await act(async () => { await vi.advanceTimersByTimeAsync(60000); });
  expect(list.querySelector("a")?.getAttribute("aria-current")).toBe("true");
  expect(list.scrollTop).toBe(120);
  vi.useRealTimers();
});

it("W3 chooses a stored project when the URL omits it", async () => {
  const values = new Map([["hk-console:work-project", "b"]]);
  vi.stubGlobal("localStorage", { getItem: (key: string) => values.get(key) ?? null, setItem: (key: string, value: string) => values.set(key, value) });
  history.replaceState(null, "", "/ui/work");
  render(<WorkPage />);
  await waitFor(() => expect(new URLSearchParams(location.search).get("project")).toBe("b"));
});
it("W2 tab counts match rendered rows for twelve tasks", async () => {
  const states = ["backlog", "unstarted", "triage", "icebox", "researching", "developing", "reviewing", "merged", "deployed", "completed", "dropped", "developing"];
  const rows = states.map((state, i) => ({ ...task, id: 200 + i, title: `Fixture ${i}`, state, awaiting_decision: i === 11, kind: i === 9 ? "decide" : "implement" }));
  vi.stubGlobal("fetch", vi.fn(async (path: string) => path.includes("projects") ? response({ projects: ["a"] }) : response({ tasks: rows, states, generated_at: "now", truncated: false })));
  history.replaceState(null, "", "/ui/work?project=a");
  const { container } = render(<WorkPage />);
  for (const [name, count] of [["Needs me", 3], ["Active", 4], ["Backlog", 4], ["Shipped", 3], ["All", 12]] as const) {
    const button = await screen.findByRole("button", { name: `${name} ${count}` });
    fireEvent.click(button);
    expect(container.querySelectorAll(".work-rows a")).toHaveLength(count);
  }
});
it("W5 renders a future state in All", async () => {
  vi.stubGlobal("fetch", vi.fn(async (path: string) => path.includes("projects") ? response({ projects: ["a"] }) : response({ tasks: [{ ...task, state: "completed" }], states: ["completed"], generated_at: "now", truncated: false })));
  history.replaceState(null, "", "/ui/work?project=a&view=all");
  render(<WorkPage />);
  expect(await screen.findByText(/알 수 없는 상태 \(completed\)/)).toBeTruthy();
});
it("W4 reports a still-truncated 5000 row cursor walk", async () => {
  let page = 0;
  vi.stubGlobal("fetch", vi.fn(async (path: string) => {
    if (path.includes("projects")) return response({ projects: ["a"] });
    const tasks = Array.from({ length: 500 }, (_, i) => ({ ...task, id: page * 500 + i + 1 }));
    page++;
    return response({ tasks, states: [], generated_at: "now", truncated: true, next_after_id: page * 500 });
  }));
  history.replaceState(null, "", "/ui/work?project=a&view=needs-me");
  render(<WorkPage />);
  expect(await screen.findByText("5000+ (truncated)")).toBeTruthy();
});
it("W14 uses the shared poller without overlapping requests", async () => {
  vi.useFakeTimers();
  const fetcher = vi.mocked(fetch);
  render(<WorkPage />);
  await act(async () => { await vi.advanceTimersByTimeAsync(0); });
  const before = fetcher.mock.calls.length;
  await act(async () => { await vi.advanceTimersByTimeAsync(60000); });
  expect(fetcher.mock.calls.length).toBe(before + 1);
  vi.useRealTimers();
});
