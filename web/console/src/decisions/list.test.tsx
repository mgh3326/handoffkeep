import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { DecisionList } from "./DecisionList";
const item = {
  id: 42,
  source: "task_flag",
  source_ref: "7/1",
  task_id: 7,
  lane: "example",
  question: "Ship?",
  due_at: null,
  overdue: false,
  status: "open",
  request_state: "",
  request_id: "",
  revision: 0,
  created_at: "now",
  answered_at: null,
  superseded_by: "",
  answer: "",
};
let rows = [item];
let wide = true;
const response = (v: unknown) => new Response(JSON.stringify(v));
beforeEach(() => {
  rows = [item];
  wide = true;
  localStorage.clear();
  sessionStorage.clear();
  history.replaceState(null, "", "/ui/decisions?tab=overdue");
  vi.stubGlobal("matchMedia", () => ({
    get matches() {
      return wide;
    },
    addEventListener() {},
    removeEventListener() {},
  }));
  vi.stubGlobal("scrollTo", vi.fn());
  vi.stubGlobal(
    "fetch",
    vi.fn(async (path: string) => {
      if (path === "/ui/api/decisions/42")
        return response({
          generated_at: "now",
          record: item,
          task: { id: 7, title: "Task", state: "developing", lane: "example" },
          request: null,
          history: [],
          attempts: [],
          answerable: false,
          disabled_reason: "source_elsewhere",
        });
      const tab =
        new URL(path, "http://localhost").searchParams.get("tab") ?? "open";
      return response({
        generated_at: "now",
        tab,
        counts: { open: 6, overdue: 2 },
        truncated: false,
        items: rows,
      });
    }),
  );
});
afterEach(() => {
  vi.unstubAllGlobals();
  history.replaceState(null, "", "/");
});
it("A12 uses the deep-linked tab and restores selection on back", async () => {
  render(<DecisionList />);
  expect(await screen.findByText(/#42 Ship/)).toBeTruthy();
  expect(
    screen
      .getByRole("button", { name: "기한 경과" })
      .getAttribute("aria-current"),
  ).toBe("page");
  expect(fetch).toHaveBeenCalledWith("/ui/api/decisions?tab=overdue");
  fireEvent.click(screen.getByText(/#42 Ship/));
  expect(location.pathname).toBe("/ui/decisions/42");
  expect(sessionStorage.getItem("hk-decisions:selected")).toBe("42");
  expect(await screen.findByRole("heading", { name: "Ship?" })).toBeTruthy();
  sessionStorage.setItem("hk-decisions:scroll", "120");
  history.replaceState(null, "", "/ui/decisions?tab=overdue");
  fireEvent(window, new PopStateEvent("popstate"));
  expect(window.scrollTo).toHaveBeenCalledWith(0, 120);
  expect(
    screen.getByRole("link", { name: /#42 Ship/ }).getAttribute("aria-current"),
  ).toBe("true");
});
it("A12 narrow rows link to a full detail page", async () => {
  wide = false;
  render(<DecisionList />);
  expect(
    (await screen.findByRole("link", { name: /#42 Ship/ })).getAttribute(
      "href",
    ),
  ).toBe("/ui/decisions/42?tab=overdue");
  expect(screen.queryByRole("heading", { name: "Ship?" })).toBeNull();
});
it("A19 mark seen persists the current id and revision", async () => {
  render(<DecisionList />);
  await screen.findByText(/#42 Ship/);
  expect(screen.getByText("새 항목")).toBeTruthy();
  fireEvent.click(screen.getByText("확인함"));
  expect(
    JSON.parse(localStorage.getItem("hk-console:decisions-seen") ?? "{}"),
  ).toEqual({ "42": 0 });
  await waitFor(() => expect(screen.queryByText("새 항목")).toBeNull());
});
it("A19 a poll keeps seen rows stable and marks new records and revisions", async () => {
  render(<DecisionList />);
  await screen.findByText(/#42 Ship/);
  fireEvent.click(screen.getByText("확인함"));
  rows = [
    { ...item, id: 43, question: "New?" },
    { ...item, revision: 1 },
  ];
  fireEvent(document, new Event("visibilitychange"));
  expect(await screen.findByText(/#43 New/)).toBeTruthy();
  expect(await screen.findByText("새 revision")).toBeTruthy();
  const names = screen
    .getAllByRole("link")
    .filter((x) => x.className.includes("decision-row"))
    .map((x) => x.textContent);
  expect(names[0]).toContain("#42");
  expect(names[1]).toContain("#43");
  expect(screen.getByText("새 항목")).toBeTruthy();
});
it("A14 list distinguishes empty, partial, failure and unknown status", async () => {
  const page = (items: unknown[], truncated: boolean) =>
    response({
      generated_at: "now",
      tab: "overdue",
      counts: { open: 6, overdue: 2 },
      truncated,
      items,
    });
  vi.mocked(fetch).mockResolvedValue(page([], false));
  let view = render(<DecisionList />);
  expect(await screen.findByText("0건")).toBeTruthy();
  view.unmount();
  vi.mocked(fetch).mockResolvedValue(page([], true));
  view = render(<DecisionList />);
  expect(await screen.findByText("0건만 로드")).toBeTruthy();
  expect(screen.queryByText("0건")).toBeNull();
  view.unmount();
  vi.mocked(fetch).mockRejectedValue(new Error("offline"));
  view = render(<DecisionList />);
  expect(await screen.findByText("불러오지 못함")).toBeTruthy();
  view.unmount();
  vi.mocked(fetch).mockResolvedValue(
    page([{ ...item, status: "future" }], false),
  );
  render(<DecisionList />);
  expect(await screen.findByText(/지원되지 않는 상태 future/)).toBeTruthy();
});
