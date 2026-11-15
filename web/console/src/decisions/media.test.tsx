import { readFileSync } from "node:fs";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { DecisionPage } from "./DecisionPage";

// hk 4788: a decision option carries its pictures and videos, and the page
// compares the options side by side with the requester's recommendation, what
// it could not check, and one fix.
const image = "a".repeat(64);
const video = "b".repeat(64);
const record = {
  id: 42,
  source: "task_request",
  source_ref: "dr-7-1",
  task_id: 7,
  lane: "example",
  question: "Which card?",
  due_at: null,
  overdue: false,
  status: "open",
  created_at: "2026-11-05T01:00:00Z",
  answered_at: null,
  superseded_by: "",
  answer: "",
};
const request = {
  id: "dr-7-1",
  revision: 1,
  state: "open",
  question: "Which card?",
  reason: "2B reads clearer at speed",
  recommendation_reason:
    "2B keeps the left line where 2A already had it.\nNothing new on 4B.",
  unverified: ["head relation of the Stage 2/3 package", "checker figures"],
  one_fix: "move the sign 20 m earlier",
  options: [
    {
      key: "A",
      label: "2A left at the signal",
      attachments: [
        { sha256: image, caption: "approach", mime: "image/png", original_name: "a.png" },
      ],
    },
    {
      key: "B",
      label: "2B left at the exit",
      recommended: true,
      attachments: [
        { sha256: video, caption: "key manoeuvre", mime: "video/mp4", original_name: "b.mp4" },
      ],
    },
  ],
  allow_free: true,
  default_action: "자동 적용 없음",
  human_only: true,
  answer_policy: "operator_direct",
};
const detail = {
  generated_at: "2026-11-06T08:00:00Z",
  record,
  task: { id: 7, title: "Ship console", state: "developing", lane: "example" },
  request,
  history: [],
  attempts: [],
  answerable: true,
  disabled_reason: "",
};
const response = (v: unknown, status = 200) =>
  new Response(JSON.stringify(v), { status });
let fetcher: ReturnType<typeof vi.fn>;
beforeEach(() => {
  localStorage.clear();
  history.replaceState(null, "", "/ui/decisions/42");
  const meta = document.createElement("meta");
  meta.name = "hk-csrf";
  meta.content = "fixture";
  document.head.append(meta);
  fetcher = vi.fn(async () => response(detail));
  vi.stubGlobal("fetch", fetcher);
});
afterEach(() => {
  document.querySelector('meta[name="hk-csrf"]')?.remove();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});
async function start() {
  render(<DecisionPage id="42" />);
  await screen.findByRole("heading", { name: "Which card?" });
}
const css = () => readFileSync("src/decisions/decisions.css", "utf8");
function block(selector: string) {
  const text = css();
  const at = text.indexOf(selector);
  expect(at).toBeGreaterThanOrEqual(0);
  return text.slice(at, text.indexOf("}", at));
}

it("B1 each option is a card with its pictures and videos in place", async () => {
  await start();
  const cards = document.querySelectorAll(".decision-option-card");
  expect(cards).toHaveLength(2);
  const picture = screen.getByAltText("approach") as HTMLImageElement;
  expect(picture.getAttribute("src")).toBe(`/ui/files/${image}?inline=1`);
  expect(
    within(screen.getByRole("radio", { name: /2A left at the signal/ })).queryByRole("img"),
  ).toBeNull();
  const clip = document.querySelector(".decision-option-card[data-option-key='B'] video");
  expect(clip?.getAttribute("src")).toBe(`/ui/files/${video}?inline=1`);
  expect(clip?.hasAttribute("controls")).toBe(true);
  expect(screen.getByText("approach")).toBeTruthy();
  expect(screen.getByText("key manoeuvre")).toBeTruthy();
});

it("B2 options sit side by side on a wide screen and one per row on a phone", () => {
  expect(block(".decision-options {")).toContain(
    "grid-template-columns: minmax(0, 1fr)",
  );
  const text = css();
  const wide = text.slice(text.indexOf("@media (min-width: 1024px)"));
  expect(wide).toContain("repeat(auto-fit, minmax(min(100%, 220px), 1fr))");
  const narrow = text.slice(text.indexOf("@media (max-width: 767.98px)"));
  const stickyAt = narrow.indexOf(".decision-option-key {");
  expect(stickyAt).toBeGreaterThanOrEqual(0);
  expect(narrow.slice(stickyAt, narrow.indexOf("}", stickyAt))).toContain(
    "position: sticky",
  );
});

it("B3 the recommendation, what is unverified and one fix follow the cards", async () => {
  await start();
  const cards = document.querySelectorAll(".decision-option-card");
  const recommendation = document.querySelector(".decision-recommendation");
  const unverified = document.querySelector(".decision-unverified");
  const oneFix = document.querySelector(".decision-one-fix");
  expect(recommendation).toBeTruthy();
  expect(unverified).toBeTruthy();
  expect(oneFix).toBeTruthy();
  // Document order: the cards, then the recommendation, then the limits.
  expect(
    cards[cards.length - 1].compareDocumentPosition(recommendation!) &
      Node.DOCUMENT_POSITION_FOLLOWING,
  ).toBeTruthy();
  expect(
    recommendation!.compareDocumentPosition(unverified!) &
      Node.DOCUMENT_POSITION_FOLLOWING,
  ).toBeTruthy();
  expect(screen.getByText(/2B keeps the left line/)).toBeTruthy();
  expect(screen.getByText("검증 안 된 것")).toBeTruthy();
  expect(screen.getByText("head relation of the Stage 2/3 package")).toBeTruthy();
  expect(screen.getByText("move the sign 20 m earlier")).toBeTruthy();
});

it("B4 the one-fix comment goes out with the answer", async () => {
  fetcher.mockImplementation((_path, init) =>
    Promise.resolve(
      init?.method === "POST" ? response({ result: "answered" }) : response(detail),
    ),
  );
  await start();
  fireEvent.click(screen.getByRole("radio", { name: /2A left at the signal/ }));
  fireEvent.change(screen.getByLabelText(/메모/), {
    target: { value: "sign too late" },
  });
  expect(screen.getByLabelText(/메모/).closest("label")?.textContent).toContain(
    "한 가지 고칠 점",
  );
  fireEvent.click(screen.getByRole("button", { name: "답변 확인" }));
  fireEvent.click(screen.getByRole("button", { name: "답변 보내기" }));
  await waitFor(() =>
    expect(
      fetcher.mock.calls.filter((call) => call[1]?.method === "POST"),
    ).toHaveLength(1),
  );
  const post = fetcher.mock.calls.find((call) => call[1]?.method === "POST")!;
  expect(JSON.parse(String(post[1].body))).toMatchObject({
    option: "A",
    text: "sign too late",
  });
});

it("B5 a file the page cannot show stays a link", async () => {
  fetcher.mockResolvedValue(
    response({
      ...detail,
      request: {
        ...request,
        options: [
          {
            key: "A",
            label: "2A left at the signal",
            attachments: [{ sha256: image, mime: "application/pdf", original_name: "plan.pdf" }],
          },
        ],
      },
    }),
  );
  await start();
  expect(document.querySelector(".decision-option-card img")).toBeNull();
  expect(
    screen.getByRole("link", { name: "plan.pdf" }).getAttribute("href"),
  ).toBe(`/ui/files/${image}?inline=1`);
});
