import { fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { HomeAttention, summarizeQuestion } from "./HomeAttention";

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
    decisions: { count: 3, overdue: 1, items: [request, laneEvent] },
    tasks: { count: 2, awaiting_decision: 1, stalled: 1, items: [waitingTask, stalledTask] },
    deploys: { count: 1, events_capped: false, items: [deploy] },
    ...overrides,
  };
}

function serve(body: unknown) {
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(body))));
}

beforeEach(() => { const values = new Map<string, string>(); vi.stubGlobal("localStorage", { getItem: (key: string) => values.get(key) ?? null, setItem: (key: string, value: string) => values.set(key, value) }); });
afterEach(() => vi.unstubAllGlobals());

it("renders action and reference sections with newest and overdue ordering", async () => {
  serve(payload()); render(<HomeAttention />);
  const action = await screen.findByRole("region", { name: "지금 할 일" });
  const fyi = screen.getByRole("region", { name: "참고" });
  expect(screen.getByRole("heading", { name: "나를 기다리는 일" })).toBeTruthy();
  const links = within(action).getAllByRole("link");
  expect(links[0].getAttribute("href")).toBe("/ui/decisions/41");
  expect(links[1].getAttribute("href")).toBe("/ui/decisions");
  expect(within(action).getByText("기한 경과")).toBeTruthy();
  expect(within(fyi).getByRole("link").getAttribute("href")).toBe("/ui/tasks/77");
  expect(within(action).getByText("+1개 더 있음")).toBeTruthy();
});
it("shows a two-line inbox row with kind, identifier, summary, chips and relative time", async () => {
  serve(payload()); render(<HomeAttention />);
  const summary = await screen.findByText("Ship the console?");
  const row = summary.closest("a");
  if (!row) throw new Error("missing row");
  expect(within(row).getByText("#41")).toBeTruthy();
  expect(within(row).getByText("요청")).toBeTruthy();
  expect(within(row).getByText("desk")).toBeTruthy();
  expect(within(row).getByText("4시간").getAttribute("title")).toBe("2026-11-05 01:00 UTC");
  expect(row.querySelector('[data-state-shape="awaiting_decision"]')).toBeTruthy();
});
it("groups inbox items by local calendar date", async () => {
  serve(payload({ generated_at: "2026-11-08T05:00:00Z", decisions: { count: 1, overdue: 0, items: [{ ...request, overdue: false, created_at: "2026-11-08T01:00:00Z" }] }, tasks: { count: 1, awaiting_decision: 1, stalled: 0, items: [{ ...waitingTask, since: "2026-11-07T01:00:00Z" }] }, deploys: { count: 1, events_capped: false, items: [{ ...deploy, merged_at: "2026-10-01T01:00:00Z" }] } }));
  render(<HomeAttention />);
  expect(await screen.findByRole("heading", { name: "오늘" })).toBeTruthy();
  expect(screen.getByRole("heading", { name: "어제" })).toBeTruthy();
  expect(screen.getByRole("heading", { name: "이전" })).toBeTruthy();
});
it("places overdue decisions above chronological groups and labels decision and stalled rows", async () => {
  serve(payload({ generated_at: "2026-11-08T05:00:00Z", decisions: { count: 2, overdue: 1, items: [{ ...request, created_at: "2026-11-07T01:00:00Z" }, { ...laneEvent, created_at: "2026-11-08T01:00:00Z" }] }, tasks: { count: 1, awaiting_decision: 0, stalled: 1, items: [{ ...stalledTask, since: "2026-11-07T02:00:00Z" }] }, deploys: { count: 0, events_capped: false, items: [] } }));
  render(<HomeAttention />);
  const action = await screen.findByRole("region", { name: "지금 할 일" });
  expect(within(action).getAllByRole("heading", { level: 3 }).map(h => h.textContent)).toEqual(["기한 지남", "오늘", "어제"]);
  const decision = within(action).getByText("Which database?").closest("a");
  if (!decision) throw new Error("missing decision");
  expect(within(decision).getByText("알림")).toBeTruthy();
  expect(within(action).queryByText("레인 이벤트")).toBeNull();
  const stalled = within(action).getByText("Idle build").closest("a");
  if (!stalled) throw new Error("missing task");
  expect(within(stalled).getByText("멈춤")).toBeTruthy();
  expect(within(stalled).queryByText("검토 중")).toBeNull();
});
it("summarizes decisions at a sentence, colon, newline, or 90 graphemes", () => {
  expect(summarizeQuestion("**배포할까요?** 다음 문장입니다.")).toBe("배포할까요?");
  expect(summarizeQuestion("선택: 긴 설명")).toBe("선택");
  expect(summarizeQuestion("첫 줄\n둘째 줄")).toBe("첫 줄");
  expect(summarizeQuestion("가".repeat(91))).toBe("가".repeat(90) + "…");
});
it("marks a row read locally and restores it on remount", async () => {
  serve(payload()); const view = render(<HomeAttention />);
  const row = (await screen.findByText("Ship the console?")).closest("a");
  expect(row && within(row).getByText("읽지 않음")).toBeTruthy();
  if (!row) throw new Error("missing row");
  row.addEventListener("click", event => event.preventDefault());
  fireEvent.click(row);
  expect(within(row).getByText("읽음")).toBeTruthy();
  view.unmount(); render(<HomeAttention />);
  expect(within((await screen.findByText("Ship the console?")).closest("a")!).getByText("읽음")).toBeTruthy();
});
it("renders the centered empty state", async () => {
  serve(payload({ decisions: { count: 0, overdue: 0, items: [] }, tasks: { count: 0, awaiting_decision: 0, stalled: 0, items: [] }, deploys: { count: 0, events_capped: false, items: [] } }));
  render(<HomeAttention />);
  expect(await screen.findByText("대기 중인 일 없음")).toBeTruthy();
  expect(screen.queryByRole("region")).toBeNull();
});

async function expectErrorNotEmpty(body: unknown) {
  serve(body);
  render(<HomeAttention />);
  expect(await screen.findByRole("alert")).toBeTruthy();
  expect(screen.queryByText("대기 중인 일 없음")).toBeNull();
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
  await expectErrorNotEmpty(payload({ decisions: { count: 1, overdue: 0, items: [noId] } }));
});

it("shows the error state when the request fails", async () => {
  vi.stubGlobal("fetch", vi.fn(async () => new Response("{}", { status: 500 })));
  render(<HomeAttention />);
  expect(await screen.findByRole("alert")).toBeTruthy();
  expect(screen.queryByText("대기 중인 일 없음")).toBeNull();
});
it("shows a fresh Korean title for a deploy and can reveal the original", async () => {
  serve(payload({ deploys: { count: 1, events_capped: false, items: [{ ...deploy, title_ko: "배포 대기 작업" }] } }));
  render(<HomeAttention />);
  expect(await screen.findByText("배포 대기 작업")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "원문 보기" }));
  expect(screen.getByText(deploy.title)).toBeTruthy();
});
