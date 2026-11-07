import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { HomeAttention, summarizeQuestion } from "./HomeAttention";

const request = {
  key: "decision:request:41",
  source: "request",
  id: 41,
  lane: "desk",
  question: "Ship the console?",
  created_at: "2026-11-05T01:00:00Z",
  due_at: "2026-11-05T02:00:00Z",
  overdue: true,
  read: false,
  woken: false,
};
const laneEvent = {
  key: "decision:lane_event:907",
  source: "lane_event",
  id: 907,
  lane: "ops",
  question: "Which database?",
  created_at: "2026-11-05T03:00:00Z",
  due_at: null,
  overdue: false,
  read: false,
  woken: false,
};
const waitingTask = {
  key: "task:12",
  id: 12,
  title: "Review the plan",
  lane: "desk",
  state: "developing",
  priority: 2,
  reason: "awaiting_decision",
  since: "2026-11-04T10:00:00Z",
  read: false,
  woken: false,
};
const stalledTask = { ...waitingTask, key: "task:13", id: 13, title: "Idle build", reason: "stalled", state: "reviewing" };
const deploy = {
  key: "deploy:handoffkeep:77",
  service: "handoffkeep",
  task_id: 77,
  title: "Add the attention route",
  pr: "https://github.com/mgh3326/handoffkeep/pull/1",
  merged_at: "2026-11-05T04:00:00Z",
  read: false,
  woken: false,
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

const stored = new Map<string, string>();

beforeEach(() => {
  stored.clear();
  vi.stubGlobal("localStorage", {
    getItem: (key: string) => stored.get(key) ?? null,
    setItem: (key: string, value: string) => {
      stored.set(key, value);
    },
    removeItem: (key: string) => {
      stored.delete(key);
    },
  });
});
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
  expect(row.getAttribute("data-inbox-key")).toBe("decision:request:41");
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
it("renders the server's read state and marks a woken snooze", async () => {
  serve(payload({
    decisions: { count: 1, overdue: 0, items: [{ ...request, read: true, overdue: false }] },
    tasks: { count: 1, awaiting_decision: 1, stalled: 0, items: [{ ...waitingTask, woken: true }] },
    deploys: { count: 0, events_capped: false, items: [] },
  }));
  render(<HomeAttention />);
  const read = (await screen.findByText("Ship the console?")).closest("a");
  if (!read) throw new Error("missing read row");
  expect(within(read).getByText("읽음")).toBeTruthy();
  expect(read.closest("li")?.className).toContain("ha-item--seen");
  const woken = screen.getByText("Review the plan").closest("a");
  if (!woken) throw new Error("missing woken row");
  expect(within(woken).getByText("다시 알림")).toBeTruthy();
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

it("shows the error state when an item has no inbox key", async () => {
  const { key: _key, ...noKey } = request;
  await expectErrorNotEmpty(payload({ decisions: { count: 1, overdue: 0, items: [{ ...noKey, overdue: false }] } }));
});

it("shows the error state when the request fails", async () => {
  vi.stubGlobal("fetch", vi.fn(async () => new Response("{}", { status: 500 })));
  render(<HomeAttention />);
  expect(await screen.findByRole("alert")).toBeTruthy();
  expect(screen.queryByText("대기 중인 일 없음")).toBeNull();
});

it("migrates the browser-local seen list into the server once, then forgets it", async () => {
  const meta = document.createElement("meta");
  meta.name = "hk-csrf";
  meta.content = "token";
  document.head.append(meta);
  stored.set("hk-console:home-seen", JSON.stringify(["task:12", "task:13"]));
  const fetcher = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (url === "/ui/api/inbox/state") {
      return new Response(JSON.stringify({ action: "read", items: [], failed: [], skipped: 0 }));
    }
    void init;
    return new Response(JSON.stringify(payload()));
  });
  vi.stubGlobal("fetch", fetcher);
  const view = render(<HomeAttention />);
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2));
  const call = fetcher.mock.calls.find(([input]) => String(input) === "/ui/api/inbox/state");
  if (!call) throw new Error("no migration request");
  const body = JSON.parse(String((call[1] as RequestInit).body));
  expect(body.action).toBe("read");
  expect(body.items).toEqual(["task:12", "task:13"]);
  expect(stored.get("hk-console:home-seen")).toBeUndefined();
  expect(stored.get("hk-console:home-seen-migrated")).toBe("1");
  view.unmount();
  render(<HomeAttention />);
  await screen.findByText("Ship the console?");
  expect(fetcher.mock.calls.filter(([input]) => String(input) === "/ui/api/inbox/state")).toHaveLength(1);
  document.querySelector('meta[name="hk-csrf"]')?.remove();
});

it("leaves the seen key alone when the migration write fails, so the next load retries", async () => {
  const meta = document.createElement("meta");
  meta.name = "hk-csrf";
  meta.content = "token";
  document.head.append(meta);
  stored.set("hk-console:home-seen", JSON.stringify(["task:12"]));
  const fetcher = vi.fn(async (input: RequestInfo | URL) => {
    if (String(input) === "/ui/api/inbox/state") return new Response("{}", { status: 500 });
    return new Response(JSON.stringify(payload()));
  });
  vi.stubGlobal("fetch", fetcher);
  render(<HomeAttention />);
  await waitFor(() => expect(fetcher.mock.calls.some(([input]) => String(input) === "/ui/api/inbox/state")).toBe(true));
  await screen.findByText("Ship the console?");
  expect(stored.get("hk-console:home-seen")).toBe(JSON.stringify(["task:12"]));
  expect(stored.get("hk-console:home-seen-migrated")).toBeUndefined();
  document.querySelector('meta[name="hk-csrf"]')?.remove();
});

it("shows a fresh Korean title for a deploy and can reveal the original", async () => {
  serve(payload({ deploys: { count: 1, events_capped: false, items: [{ ...deploy, title_ko: "배포 대기 작업" }] } }));
  render(<HomeAttention />);
  expect(await screen.findByText("배포 대기 작업")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "원문 보기" }));
  expect(screen.getByText(deploy.title)).toBeTruthy();
});
it("preserves inbox selection and read state for a translated decision", async () => {
  serve(payload({ decisions: { count: 1, overdue: 0, items: [{ ...request, read: true, overdue: false, question_ko: "배포할까요?" }] }, tasks: { count: 0, awaiting_decision: 0, stalled: 0, items: [] }, deploys: { count: 0, events_capped: false, items: [] } }));
  render(<HomeAttention />);
  const row = (await screen.findByText("배포할까요?")).closest("li");
  if (!row) throw new Error("missing translated row");
  expect(within(row).getByText("읽음")).toBeTruthy();
  const select = within(row).getByRole("button", { name: "배포할까요? 선택", hidden: true });
  fireEvent.click(select);
  expect(select.getAttribute("aria-pressed")).toBe("true");
  fireEvent.click(within(row).getByRole("button", { name: "원문 보기" }));
  expect(within(row).getByText("Ship the console?")).toBeTruthy();
});
