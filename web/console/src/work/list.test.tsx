import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { WorkPage } from "./WorkPage";

const task = { id: 101, title: "Task one", lane: "x", kind: "implement", state: "developing", priority: 2, created_by: "test", created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z", refs: {}, project: "a" };
const detail = { task, events: [], dwell: [], linear: null, participants: { task_ref: "hk:task/101", coverage: "not_collected", segments: [] }, origins: [], documents: [], documents_truncated: false, changes: [], jobs: [], origins_truncated: false, changes_truncated: false, jobs_truncated: false };
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
  expect(await screen.findByText("Task one")).toBeTruthy();
  expect((screen.getByLabelText("검색") as HTMLInputElement).value).toBe("#101");
  expect((screen.getByLabelText("우선순위 ≥") as HTMLInputElement).value).toBe("2");
  expect((screen.getByLabelText("레인") as HTMLSelectElement).value).toBe("x");
  fireEvent.change(screen.getByLabelText("검색"), { target: { value: "one" } });
  expect(new URLSearchParams(location.search).get("q")).toBe("one");
});
it("W10 uses a pane on wide screens and links on narrow screens", async () => {
  wide = true;
  const { unmount } = render(<WorkPage />);
  fireEvent.click(await screen.findByText("Task one"));
  expect(new URLSearchParams(location.search).get("task")).toBe("101");
  expect(await screen.findByRole("heading", { name: "#101 Task one" })).toBeTruthy();
  history.replaceState(null, "", "/ui/work?project=a&view=active");
  act(() => window.dispatchEvent(new PopStateEvent("popstate")));
  await waitFor(() => expect(screen.queryByRole("heading", { name: "#101 Task one" })).toBeNull());
  unmount(); wide = false;
  render(<WorkPage />);
  expect((await screen.findByText("Task one")).closest("a")?.getAttribute("href")).toBe("/ui/tasks/101");
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
  for (const [name, count] of [["내 차례", 3], ["진행 중", 4], ["대기", 4], ["완료", 3], ["전체", 12]] as const) {
    const button = await screen.findByRole("button", { name: `${name} ${count}` });
    fireEvent.click(button);
    expect(container.querySelectorAll(".work-rows a")).toHaveLength(count);
  }
});
it("W5 renders a future state in All", async () => {
  vi.stubGlobal("fetch", vi.fn(async (path: string) => path.includes("projects") ? response({ projects: ["a"] }) : response({ tasks: [{ ...task, state: "future_state" }], states: ["future_state"], generated_at: "now", truncated: false })));
  history.replaceState(null, "", "/ui/work?project=a&view=all");
  render(<WorkPage />);
  expect(await screen.findByText(/알 수 없는 상태 \(future_state\)/)).toBeTruthy();
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

it("keeps URL filter values visible when they have no matching options", async () => {
  history.replaceState(null, "", "/ui/work?project=missing&view=all&lane=unknown-lane&kind=unknown-kind&state=unknown-state");
  render(<WorkPage />);
  await screen.findByRole("button", { name: "전체 0" });
  for (const [label, value] of [["프로젝트", "missing"], ["레인", "unknown-lane"], ["종류", "unknown-kind"], ["상태", "unknown-state"]]) {
    const select = screen.getByLabelText(label) as HTMLSelectElement;
    expect(select.value).toBe(value);
    expect(select.selectedOptions[0]?.textContent).toBe(value);
  }
});

it("shows active job model, profile, host fallback, chips and relative time", async () => {
  const row = { ...task, priority: 1, active_job: { role: "worker", model: "model-x", profile: "high", host: "", harness: null, effort: null } };
  vi.stubGlobal("fetch", vi.fn(async (path: string) => path.includes("projects") ? response({ projects: ["a"] }) : response({ tasks: [row], states: [], generated_at: "2026-01-01T02:00:00Z", truncated: false })));
  history.replaceState(null, "", "/ui/work?project=a&view=active");
  const { container } = render(<WorkPage />);
  expect(await screen.findByText("worker · model-x · high · 기기 미기록")).toBeTruthy();
  expect(screen.getByTitle("Task one")).toBeTruthy();
  expect(screen.getByText("2시간").getAttribute("title")).toContain("2026-01-01");
  expect(container.querySelector(".hk-chip--priority-urgent")?.textContent).toBe("P1");
});
it("arrow keys select rows and Enter opens the record", async () => {
  wide = true;
  history.replaceState(null, "", "/ui/work?project=a&view=active");
  render(<WorkPage />);
  await screen.findByText("Task one");
  fireEvent.keyDown(screen.getByText("Task one").closest("a")!, { key: "ArrowDown" });
  expect(document.activeElement?.id).toBe("work-row-101");
  fireEvent.keyDown(document.activeElement!, { key: "Enter" });
  expect(await screen.findByRole("heading", { name: "#101 Task one" })).toBeTruthy();
});

it("renders a titled state row with the live job host and metadata", async () => {
  const row = { ...task, active_job: { role: "worker", model: "model-x", profile: "high", host: "studio-device", harness: "codex", effort: "high" } };
  vi.stubGlobal("fetch", vi.fn(async (path: string) => path.includes("projects") ? response({ projects: ["a"] }) : response({ tasks: [row], states: [], generated_at: "2026-01-01T02:00:00Z", truncated: false })));
  history.replaceState(null, "", "/ui/work?project=a&view=active");
  const { container } = render(<WorkPage />);
  expect(await screen.findByText("worker · model-x · high · studio-device")).toBeTruthy();
  expect(screen.getByTitle("Task one")).toBeTruthy();
  expect(container.querySelector(".work-row-title svg[data-state-shape='developing']")).toBeTruthy();
  expect(container.querySelector('[data-chip="status"]')?.textContent).toBe("개발 중");
  expect(container.querySelector('[data-chip="lane"]')?.textContent).toBe("x");
  expect(container.querySelector('[data-chip="priority"]')?.textContent).toBe("P2");
  expect(screen.getByText("2시간").getAttribute("title")).toContain("2026-01-01");
});

it("moves selection down and up without opening a record until Enter", async () => {
  wide = true;
  const rows = [task, { ...task, id: 102, title: "Task two" }];
  vi.stubGlobal("fetch", vi.fn(async (path: string) => path.includes("projects") ? response({ projects: ["a"] }) : path.includes("/101") ? response(detail) : response({ tasks: rows, states: [], generated_at: "2026-01-01T02:00:00Z", truncated: false })));
  history.replaceState(null, "", "/ui/work?project=a&view=active");
  render(<WorkPage />);
  const firstRow = (await screen.findByText("Task two")).closest("a")!;
  fireEvent.keyDown(firstRow, { key: "ArrowUp" });
  expect(document.activeElement?.id).toBe("work-row-102");
  fireEvent.keyDown(document.activeElement!, { key: "ArrowDown" });
  expect(document.activeElement?.id).toBe("work-row-101");
  expect(new URLSearchParams(location.search).get("task")).toBeNull();
  fireEvent.keyDown(document.activeElement!, { key: "ArrowUp" });
  expect(document.activeElement?.id).toBe("work-row-102");
  expect(document.activeElement?.getAttribute("aria-current")).toBeNull();
});


it("preserves modified arrows and narrow page scrolling while supporting focused rows", async () => {
  history.replaceState(null, "", "/ui/work?project=a&view=active");
  render(<WorkPage />);
  const row = (await screen.findByText("Task one")).closest("a")!;
  const scroll = new KeyboardEvent("keydown", { key: "ArrowDown", bubbles: true, cancelable: true });
  window.dispatchEvent(scroll);
  expect(scroll.defaultPrevented).toBe(false);
  for (const modifier of ["metaKey", "ctrlKey", "altKey", "shiftKey"] as const) {
    const event = new KeyboardEvent("keydown", { key: "ArrowDown", bubbles: true, cancelable: true, [modifier]: true });
    row.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(false);
  }
  row.focus();
  const navigate = new KeyboardEvent("keydown", { key: "ArrowDown", bubbles: true, cancelable: true });
  act(() => { row.dispatchEvent(navigate); });
  expect(navigate.defaultPrevented).toBe(true);
});

it("keeps the open record selected while focus moves and follows focus after history navigation", async () => {
  wide = true;
  const rows = [task, { ...task, id: 102, title: "Task two" }];
  vi.stubGlobal("fetch", vi.fn(async (path: string) => path.includes("projects") ? response({ projects: ["a"] }) : path.includes("/101") ? response(detail) : response({ tasks: rows, states: [], generated_at: "2026-01-01T02:00:00Z", truncated: false })));
  history.replaceState(null, "", "/ui/work?project=a&view=active&task=101");
  const { container } = render(<WorkPage />);
  await screen.findByText("Task two");
  fireEvent.keyDown(container.querySelector("#work-row-101")!, { key: "ArrowUp" });
  expect(document.activeElement?.id).toBe("work-row-102");
  expect(container.querySelector("#work-row-101")?.getAttribute("aria-current")).toBe("true");
  expect(container.querySelector("#work-row-102")?.getAttribute("aria-current")).toBeNull();
  act(() => { history.replaceState(null, "", "/ui/work?project=a&view=active"); window.dispatchEvent(new PopStateEvent("popstate")); });
  fireEvent.keyDown(document.activeElement!, { key: "ArrowDown" });
  expect(document.activeElement?.id).toBe("work-row-101");
});


it("wide page arrows leave scrolling and other controls alone", async () => {
  wide = true;
  history.replaceState(null, "", "/ui/work?project=a&view=active");
  render(<WorkPage />);
  await screen.findByText("Task one");
  const event = new KeyboardEvent("keydown", { key: "ArrowDown", bubbles: true, cancelable: true });
  document.body.dispatchEvent(event);
  expect(event.defaultPrevented).toBe(false);
  expect(document.activeElement).toBe(document.body);
});


it("sorts urgent priorities first, unset last, then by update and id", async () => {
  const rows = [
    { ...task, id: 1, title: "Unset", priority: 0 },
    { ...task, id: 2, title: "P3", priority: 3 },
    { ...task, id: 3, title: "P1 old", priority: 1, updated_at: "2025-01-01T00:00:00Z" },
    { ...task, id: 4, title: "P1 newer", priority: 1 },
    { ...task, id: 5, title: "P1 tie", priority: 1 },
    { ...task, id: 6, title: "P2", priority: 2 },
  ];
  vi.stubGlobal("fetch", vi.fn(async (path: string) => path.includes("projects") ? response({ projects: ["a"] }) : response({ tasks: rows, states: [], generated_at: "2026-01-01T02:00:00Z", truncated: false })));
  history.replaceState(null, "", "/ui/work?project=a&view=active");
  const { container } = render(<WorkPage />);
  await screen.findByText("P1 tie");
  expect([...container.querySelectorAll(".work-rows a")].map(a => a.id)).toEqual(["work-row-5", "work-row-4", "work-row-3", "work-row-6", "work-row-2", "work-row-1"]);
});


it("moves relative to the row reached by Tab even after another row was selected", async () => {
  wide = true;
  const rows = [task, { ...task, id: 102, title: "Task two" }, { ...task, id: 103, title: "Task three" }];
  vi.stubGlobal("fetch", vi.fn(async (path: string) => path.includes("projects") ? response({ projects: ["a"] }) : response({ tasks: rows, states: [], generated_at: "2026-01-01T02:00:00Z", truncated: false })));
  history.replaceState(null, "", "/ui/work?project=a&view=active&task=103");
  render(<WorkPage />);
  const middle = (await screen.findByText("Task two")).closest("a")!;
  middle.focus();
  fireEvent.keyDown(middle, { key: "ArrowDown" });
  expect(document.activeElement?.id).toBe("work-row-101");
  middle.focus();
  fireEvent.keyDown(middle, { key: "ArrowUp" });
  expect(document.activeElement?.id).toBe("work-row-103");
});
it("finds Korean titles and shows the fresh translation first", async () => {
  vi.stubGlobal("fetch", vi.fn(async (path: string) => path.includes("projects") ? response({ projects: ["a"] }) : response({ tasks: [{ ...task, title_ko: "한국어 작업" }], states: ["developing"], generated_at: "now", truncated: false })));
  history.replaceState(null, "", "/ui/work?project=a&view=active&q=%ED%95%9C%EA%B5%AD%EC%96%B4");
  render(<WorkPage />);
  expect(await screen.findByText("한국어 작업")).toBeTruthy();
});
it("toggles one translated title and marks a stale one", async () => {
  const rows = [{ ...task, title_ko: "작업 번역" }, { ...task, id: 102, title: "Changed", title_ko: "오래됨", title_ko_stale: true }];
  vi.stubGlobal("fetch", vi.fn(async (path: string) => path.includes("projects") ? response({ projects: ["a"] }) : response({ tasks: rows, states: ["developing"], generated_at: "now", truncated: false })));
  history.replaceState(null, "", "/ui/work?project=a&view=active");
  render(<WorkPage />);
  expect(await screen.findByText("작업 번역")).toBeTruthy();
  expect(screen.getByText("갱신 필요")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "원문" }));
  expect(screen.getByText("Task one")).toBeTruthy();
});
