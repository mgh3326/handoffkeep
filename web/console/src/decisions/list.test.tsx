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
let rows: Record<string, unknown>[] = [item];
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
  expect(await screen.findByRole("link", { name: /#42 Ship/ })).toBeTruthy();
  expect(
    screen
      .getByRole("link", { name: /기한 경과/ })
      .getAttribute("aria-current"),
  ).toBe("page");
  expect(fetch).toHaveBeenCalledWith("/ui/api/decisions?tab=overdue");
  fireEvent.click(screen.getByRole("link", { name: /#42 Ship/ }));
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
  await screen.findByRole("link", { name: /#42 Ship/ });
  expect(screen.getByText("새 항목")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "모두 확인" }));
  expect(
    JSON.parse(localStorage.getItem("hk-console:decisions-seen") ?? "{}"),
  ).toEqual({ "42": 0 });
  await waitFor(() => expect(screen.queryByText("새 항목")).toBeNull());
});
it("A19 a poll keeps seen rows stable and marks new records and revisions", async () => {
  render(<DecisionList />);
  await screen.findByRole("link", { name: /#42 Ship/ });
  fireEvent.click(screen.getByRole("button", { name: "모두 확인" }));
  rows = [
    { ...item, id: 43, question: "New?" },
    { ...item, revision: 1 },
  ];
  fireEvent(document, new Event("visibilitychange"));
  expect(await screen.findByRole("link", { name: /#43 New/ })).toBeTruthy();
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
  expect(await screen.findByText("기한이 지난 결정이 없어요")).toBeTruthy();
  view.unmount();
  vi.mocked(fetch).mockResolvedValue(page([], true));
  view = render(<DecisionList />);
  expect(await screen.findByText("0건만 로드")).toBeTruthy();
  expect(screen.queryByText("기한이 지난 결정이 없어요")).toBeNull();
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

// ---- hk 3286 (D4): the redesigned list ------------------------------------

import { readFileSync } from "node:fs";
const css = readFileSync("src/decisions/decisions.css", "utf8");
const rule = (selector: string) => {
  const start = css.indexOf(`${selector} {`);
  expect(start).toBeGreaterThanOrEqual(0);
  return css.slice(start, css.indexOf("}", start));
};

it("D4 tabs are one segmented row with counts and aria-current links", async () => {
  history.replaceState(null, "", "/ui/decisions?tab=open");
  render(<DecisionList />);
  const nav = await screen.findByRole("navigation", { name: "결정 탭" });
  await waitFor(() => expect(nav.textContent).toContain("6"));
  const links = Array.from(nav.querySelectorAll("a"));
  expect(links.map((a) => a.textContent)).toEqual(["열림6", "기한 경과2", "답변 기록", "정리 필요"]);
  expect(links.map((a) => a.getAttribute("aria-current"))).toEqual(["page", null, null, null]);
  // One row at any width: no wrapping, sideways scroll past the edge, and a
  // fade on phones. jsdom has no layout, so the rules are read from the CSS.
  expect(rule(".decision-tabs")).toContain("flex-wrap: nowrap");
  expect(rule(".decision-tabs")).toContain("overflow-x: auto");
  expect(css).toMatch(/max-width: 767\.98px\) \{\s*\.decision-tabs \{[^}]*mask-image/);
  // The mark-seen action sits beside the tabs; it is not a fifth tab.
  expect(nav.querySelector("button")).toBeNull();
  expect(screen.getByRole("button", { name: "모두 확인" })).toBeTruthy();
});

it("D4 choosing a tab keeps the URL and reloads that tab", async () => {
  history.replaceState(null, "", "/ui/decisions?tab=open");
  render(<DecisionList />);
  await screen.findByRole("link", { name: /#42 Ship/ });
  fireEvent.click(screen.getByRole("link", { name: /기한 경과/ }));
  expect(location.search).toBe("?tab=overdue");
  await waitFor(() => expect(fetch).toHaveBeenCalledWith("/ui/api/decisions?tab=overdue"));
  expect(screen.getByRole("link", { name: /기한 경과/ }).getAttribute("aria-current")).toBe("page");
});

it("D4 a row carries the state icon, a one-line summary and its chips", async () => {
  rows = [
    {
      ...item,
      source: "task_request",
      lane: "billing-lane",
      due_at: "2026-11-06T17:00:00Z",
      created_at: "2026-11-06T09:00:00Z",
      question: "## 배포 승인\n\n**v2 배포를 진행할까요?** 이어지는 긴 설명은 상세 페이지에 있습니다.",
    },
  ];
  vi.mocked(fetch).mockImplementation(
    async () =>
      response({
        generated_at: "2026-11-06T12:00:00Z",
        tab: "overdue",
        counts: { open: 6, overdue: 2 },
        truncated: false,
        items: rows,
      }),
  );
  render(<DecisionList />);
  const row = await screen.findByRole("link", { name: /#42/ });
  expect(row.querySelector("[data-state-shape='awaiting_decision']")).toBeTruthy();
  expect(row.querySelector(".decision-summary")?.textContent).toBe("배포 승인");
  const chips = Array.from(row.querySelectorAll(".hk-chip")).map((c) => c.textContent);
  expect(chips).toEqual(["요청", "기한 5시간 남음", "billing-lane"]);
  expect(row.querySelector("time")?.textContent).toBe("3시간");
  expect(rule(".decision-summary")).toContain("text-overflow: ellipsis");
  expect(rule(".decision-summary")).toContain("white-space: nowrap");
});

it("D4 an overdue row names the state in a chip with an icon", async () => {
  rows = [{ ...item, overdue: true }];
  render(<DecisionList />);
  const row = await screen.findByRole("link", { name: /#42/ });
  const chip = row.querySelector("[data-group='failed']");
  expect(chip?.textContent).toContain("기한 경과 · 기본값 미적용");
  expect(chip?.querySelector("svg")).toBeTruthy();
});

it("D4 an unseen decision is bolder than a seen one", async () => {
  rows = [item, { ...item, id: 43, question: "Seen?" }];
  localStorage.setItem("hk-console:decisions-seen", JSON.stringify({ "43": 0 }));
  render(<DecisionList />);
  const unseen = await screen.findByRole("link", { name: /#42/ });
  const seen = await screen.findByRole("link", { name: /#43/ });
  expect(unseen.className).toContain("decision-row--new");
  expect(seen.className).not.toContain("decision-row--new");
  expect(rule(".decision-list .decision-row--new")).toContain("font-weight: var(--hk-weight-strong)");
  // Not by weight alone: assistive technology hears it too.
  expect(unseen.textContent).toContain("안 읽음");
  expect(seen.textContent).not.toContain("안 읽음");
});

it.each([
  ["open", "답할 결정이 없어요", "답변 기록 보기", "answered"],
  ["overdue", "기한이 지난 결정이 없어요", "열린 결정 보기", "open"],
  ["answered", "아직 답변한 결정이 없어요", "열린 결정 보기", "open"],
  ["cleanup", "정리할 요청이 없어요", "열린 결정 보기", "open"],
])("D4 the %s tab has its own empty state", async (tab, text, next, target) => {
  history.replaceState(null, "", `/ui/decisions?tab=${tab}`);
  rows = [];
  render(<DecisionList />);
  expect(await screen.findByText(text)).toBeTruthy();
  expect(screen.queryByText("0건")).toBeNull();
  expect(screen.getByRole("link", { name: next }).getAttribute("href")).toBe(`/ui/decisions?tab=${target}`);
  const empty = rule(".decision-empty");
  expect(empty).toContain("align-items: center");
  expect(empty).toContain("gap: var(--hk-space-6)");
  expect(empty).toContain("text-align: center");
});

it("D4 an empty state's link opens the tab it names", async () => {
  history.replaceState(null, "", "/ui/decisions?tab=open");
  rows = [];
  render(<DecisionList />);
  fireEvent.click(await screen.findByRole("link", { name: "답변 기록 보기" }));
  expect(location.search).toBe("?tab=answered");
});

it("D4 the page uses tokens only: no hex colors, no inline styles", () => {
  expect(css).not.toMatch(/#[0-9a-fA-F]{3,8}\b/);
  for (const file of ["DecisionList", "DecisionPage", "ConfirmSheet", "DecisionChip", "ResultBadge"]) {
    expect(readFileSync(`src/decisions/${file}.tsx`, "utf8")).not.toMatch(/style=\{/);
  }
});
