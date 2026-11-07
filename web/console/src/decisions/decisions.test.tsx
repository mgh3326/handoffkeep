import { readFileSync } from "node:fs";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import {
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { DecisionPage } from "./DecisionPage";
import { ResultBadge } from "./ResultBadge";
import { draftKey, saveDraft } from "./drafts";
const record = {
  id: 42,
  source: "task_request",
  source_ref: "dr-7-2",
  task_id: 7,
  lane: "example",
  question: "Ship it?",
  due_at: "2026-11-06T09:00:00Z",
  overdue: false,
  status: "open",
  created_at: "2026-11-05T01:00:00Z",
  answered_at: null,
  superseded_by: "",
  answer: "",
};
const request = {
  id: "dr-7-2",
  revision: 2,
  state: "open",
  question: "Ship it?",
  options: [
    { key: "A", label: "Wait" },
    { key: "B", label: "Ship", recommended: true },
    { key: "C", label: "Review" },
  ],
  allow_free: true,
  default_action: "Review",
  default_option: "C",
  default_trigger: "deadline",
  due_at: "2026-11-06T09:00:00Z",
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
  vi.useRealTimers();
});
async function start() {
  render(<DecisionPage id="42" />);
  await screen.findByRole("heading", { name: "Ship it?" });
}
async function sheet() {
  await start();
  fireEvent.click(screen.getByRole("radio", { name: /Ship/ }));
  fireEvent.click(screen.getByRole("button", { name: "답변 확인" }));
}
it("A1 renders the record, request and task link", async () => {
  await start();
  expect(screen.getByText(/결정 #42/)).toBeTruthy();
  expect(document.querySelector(".decision-ref")?.textContent).toMatch(/^dr-7-2 · revision 2/);
  expect(
    screen.getByRole("link", { name: "Ship console" }).getAttribute("href"),
  ).toBe("/ui/tasks/7");
});
it("A2 starts unselected and separates recommendation and default", async () => {
  await start();
  expect(
    screen
      .getAllByRole("radio")
      .every((x) => x.getAttribute("aria-checked") === "false"),
  ).toBe(true);
  expect(
    within(screen.getByRole("radio", { name: /Ship/ })).getByText("권장"),
  ).toBeTruthy();
  expect(
    within(screen.getByRole("radio", { name: /Review/ })).getByText(
      "미응답 시 기본값",
    ),
  ).toBeTruthy();
});
it("A3 shows the deadline and imminent mark", async () => {
  vi.setSystemTime(new Date("2026-11-06T08:00:00Z"));
  await start();
  expect(screen.getByText(/2026-11-06 18:00 KST.*임박/)).toBeTruthy();
});
it("A4 distinguishes overdue without claiming default application", async () => {
  fetcher.mockResolvedValue(
    response({ ...detail, request: { ...request, state: "overdue" } }),
  );
  await start();
  expect(screen.getByText(/기한 경과 · 기본값 미적용/)).toBeTruthy();
  expect(screen.queryByText(/기본값 적용됨/)).toBeNull();
});
it("A5 refuses to post when preflight revision changes", async () => {
  fetcher.mockResolvedValueOnce(response(detail)).mockResolvedValueOnce(
    response({
      ...detail,
      request: { ...request, id: "dr-7-3", revision: 3 },
    }),
  );
  await sheet();
  fireEvent.click(screen.getByRole("button", { name: "답변 보내기" }));
  expect(await screen.findByText(/revision이 바뀌었습니다/)).toBeTruthy();
  expect(
    fetcher.mock.calls.filter((c) => c[1]?.method === "POST"),
  ).toHaveLength(0);
  fireEvent.click(screen.getByRole("radio", { name: /Ship/ }));
  expect(screen.queryByText(/revision이 바뀌었습니다/)).toBeNull();
});
it("A6 confirmation displays provenance and allows editing", async () => {
  await sheet();
  expect(screen.getByText(/직접 답\(operator_direct\)/)).toBeTruthy();
  expect(screen.getByText("revision 2")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "수정" }));
  expect(screen.queryByRole("dialog")).toBeNull();
});
it("A7 repeated confirmation sends one request", async () => {
  let finish: (r: Response) => void = () => {};
  fetcher.mockImplementation((_path, init) =>
    init?.method === "POST"
      ? new Promise<Response>((resolve) => {
          finish = resolve;
        })
      : Promise.resolve(response(detail)),
  );
  await sheet();
  const b = screen.getByRole("button", { name: "답변 보내기" });
  fireEvent.click(b);
  fireEvent.click(b);
  await waitFor(() =>
    expect(
      fetcher.mock.calls.filter((c) => c[1]?.method === "POST"),
    ).toHaveLength(1),
  );
  expect(screen.getByText("전송 중")).toBeTruthy();
  finish(response({ result: "answered" }));
});
it("A8 network failure requires checking before retry with the same body", async () => {
  fetcher.mockImplementation((_path, init) =>
    init?.method === "POST"
      ? Promise.reject(new Error("offline"))
      : Promise.resolve(response(detail)),
  );
  await sheet();
  fireEvent.click(screen.getByRole("button", { name: "답변 보내기" }));
  expect(await screen.findByText("결과 미확인")).toBeTruthy();
  expect(screen.queryByText("다시 보내기")).toBeNull();
  fireEvent.click(screen.getByText("결과 확인"));
  fireEvent.click(await screen.findByText("다시 보내기"));
  await waitFor(() =>
    expect(
      fetcher.mock.calls.filter((c) => c[1]?.method === "POST"),
    ).toHaveLength(2),
  );
  const posts = fetcher.mock.calls.filter((c) => c[1]?.method === "POST");
  expect(posts[0][1].body).toBe(posts[1][1].body);
});
it("A9 pending relay remains open with its deadline", async () => {
  fetcher.mockResolvedValue(
    response({
      ...detail,
      attempts: [
        {
          id: 1,
          channel: "session",
          provenance: "relay",
          actor: "operator",
          result: "pending_confirmation",
          revision_request_id: "dr-7-2",
          question_revision: 2,
          received_at: "now",
        },
      ],
    }),
  );
  await start();
  expect(screen.getByText("접수 · 확인 대기")).toBeTruthy();
  const pending = screen.getByText(/중계 답 접수/);
  expect(pending.textContent).not.toContain("/");
  expect(screen.getByText(/기한:/)).toBeTruthy();
  expect(screen.queryByText("답변됨")).toBeNull();
});
it("A10 source elsewhere links to legacy", async () => {
  fetcher.mockResolvedValue(
    response({
      ...detail,
      request: null,
      answerable: false,
      disabled_reason: "source_elsewhere",
    }),
  );
  await start();
  expect(
    screen.getByRole("link", { name: "기존 페이지" }).getAttribute("href"),
  ).toBe("/ui/decisions/legacy");
});
it("A11 restores a request-specific draft", async () => {
  saveDraft("42", "dr-7-2", { option: "B", text: "memo", saved_at: "now" });
  await start();
  expect(
    screen.getByRole("radio", { name: /Ship/ }).getAttribute("aria-checked"),
  ).toBe("true");
  expect((screen.getByLabelText("메모") as HTMLTextAreaElement).value).toBe(
    "memo",
  );
  expect(draftKey("42", "dr-7-2")).toContain("42:dr-7-2");
});
it("A13 results have distinct labels", () => {
  const values = [
    "answered",
    "pending_confirmation",
    "stale_revision",
    "expired",
    "unconfirmed",
  ] as const;
  const { container } = render(
    <>
      {values.map((x) => (
        <ResultBadge key={x} result={x} />
      ))}
    </>,
  );
  expect(
    new Set(
      Array.from(container.querySelectorAll("span"), (x) => x.textContent),
    ).size,
  ).toBe(5);
});
it("A14 unknown record states are explicit", async () => {
  fetcher.mockResolvedValue(
    response({ ...detail, request: { ...request, state: "future" } }),
  );
  await start();
  expect(
    screen.getAllByText(/지원되지 않는 상태 future/).length,
  ).toBeGreaterThan(0);
});
it("A16 typing targets do not select via numeric keys", async () => {
  await start();
  fireEvent.keyDown(screen.getByLabelText("메모"), { key: "1" });
  expect(
    screen.getByRole("radio", { name: /Wait/ }).getAttribute("aria-checked"),
  ).toBe("false");
});
it("A17 field errors are associated with choices", async () => {
  await start();
  fireEvent.click(screen.getByRole("button", { name: "답변 확인" }));
  expect(screen.getByRole("radiogroup").getAttribute("aria-describedby")).toBe(
    "decision-error",
  );
});
it("A18 task state and answer are separate", async () => {
  fetcher.mockResolvedValue(
    response({
      ...detail,
      answerable: false,
      disabled_reason: "closed",
      request: {
        ...request,
        state: "answered",
        resolution: { kind: "answered", option: "B", text: "yes" },
      },
    }),
  );
  await start();
  expect(document.querySelector(".decision-task-state")?.textContent).toBe("작업 상태: 개발 중");
  expect(document.body.textContent).not.toContain("developing");
  expect(screen.getByText("답변: B yes")).toBeTruthy();
});

it("A5 refetches after a stale revision result", async () => {
  fetcher.mockImplementation((_path, init) =>
    Promise.resolve(
      init?.method === "POST"
        ? response({ result: "stale_revision" }, 409)
        : response(detail),
    ),
  );
  await sheet();
  fireEvent.click(screen.getByRole("button", { name: "답변 보내기" }));
  expect(
    await screen.findByText(/이전 revision · 다시 확인 필요/),
  ).toBeTruthy();
  expect(fetcher.mock.calls.filter((c) => !c[1]?.method)).toHaveLength(3);
  expect(screen.queryByText("답변됨")).toBeNull();
});
it("A9 a 202 response remains pending", async () => {
  let reads = 0;
  fetcher.mockImplementation((_path, init) => {
    if (init?.method === "POST")
      return Promise.resolve(response({ result: "pending_confirmation" }, 202));
    reads++;
    return Promise.resolve(
      response(
        reads < 3
          ? detail
          : {
              ...detail,
              attempts: [
                {
                  id: 1,
                  channel: "session",
                  provenance: "relay",
                  actor: "reviewer",
                  result: "pending_confirmation",
                  revision_request_id: "dr-7-2",
                  question_revision: 2,
                  received_at: "2026-11-06T08:00:00Z",
                },
              ],
            },
      ),
    );
  });
  await sheet();
  fireEvent.click(screen.getByRole("button", { name: "답변 보내기" }));
  expect(await screen.findByText("접수 · 확인 대기")).toBeTruthy();
  expect(
    await screen.findByText(/중계 답 접수.*reviewer.*session/),
  ).toBeTruthy();
  expect(screen.getByText(/기한:/)).toBeTruthy();
  expect(
    (screen.getByRole("button", { name: "답변 확인" }) as HTMLButtonElement)
      .disabled,
  ).toBe(false);
  expect(screen.queryByText("답변됨")).toBeNull();
});
it("A16 Escape confirms a dirty memo and returns focus", async () => {
  vi.spyOn(window, "confirm").mockReturnValue(true);
  await start();
  fireEvent.change(screen.getByLabelText("메모"), {
    target: { value: "memo" },
  });
  const opener = screen.getByRole("button", { name: "답변 확인" });
  fireEvent.click(opener);
  fireEvent.keyDown(window, { key: "Escape" });
  expect(window.confirm).toHaveBeenCalled();
  expect(document.activeElement).toBe(opener);
});
it("A11 older revision draft remains read-only", async () => {
  saveDraft("42", "dr-7-1", { option: "A", text: "old", saved_at: "now" });
  fetcher.mockResolvedValue(
    response({
      ...detail,
      history: [{ ...request, id: "dr-7-1", revision: 1 }],
    }),
  );
  await start();
  expect(screen.getByText(/이전 revision 초안/)).toBeTruthy();
  expect(
    screen
      .getAllByRole("radio")
      .every((x) => x.getAttribute("aria-checked") === "false"),
  ).toBe(true);
});

it("A4 closed record states have distinct labels and disabled actions", async () => {
  for (const [state, reason, word] of [
    ["default_applied", "closed", "기본값 적용됨"],
    ["withdrawn", "closed", "철회됨"],
    ["superseded", "superseded", "대체됨"],
  ] as const) {
    fetcher.mockResolvedValue(
      response({
        ...detail,
        answerable: false,
        disabled_reason: reason,
        request: { ...request, state },
      }),
    );
    const view = render(<DecisionPage id="42" />);
    await screen.findByText(new RegExp(word));
    expect(
      (screen.getByRole("button", { name: "답변 확인" }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
    view.unmount();
  }
});
it("A10 missing CSRF disables the action with a reload link", async () => {
  document.querySelector('meta[name="hk-csrf"]')?.remove();
  await start();
  expect(
    (screen.getByRole("button", { name: "답변 확인" }) as HTMLButtonElement)
      .disabled,
  ).toBe(true);
  expect(screen.getByRole("link", { name: "새로고침" })).toBeTruthy();
});
it("A16 composing keystrokes do not select options", async () => {
  await start();
  fireEvent.keyDown(window, { key: "1", isComposing: true });
  expect(
    screen.getByRole("radio", { name: /Wait/ }).getAttribute("aria-checked"),
  ).toBe("false");
});
it("A5 a conflict result waits for the detail refetch", async () => {
  let getCount = 0;
  let finish: (value: Response) => void = () => {};
  fetcher.mockImplementation((_path, init) => {
    if (init?.method === "POST")
      return Promise.resolve(response({ result: "stale_revision" }, 409));
    getCount++;
    return getCount === 3
      ? new Promise<Response>((resolve) => {
          finish = resolve;
        })
      : Promise.resolve(response(detail));
  });
  await sheet();
  fireEvent.click(screen.getByRole("button", { name: "답변 보내기" }));
  await waitFor(() => expect(getCount).toBe(3));
  expect(screen.queryByText(/이전 revision · 다시 확인 필요/)).toBeNull();
  finish(response(detail));
  expect(
    await screen.findByText(/이전 revision · 다시 확인 필요/),
  ).toBeTruthy();
});
it("A3 applied default needs a receipt and does not render an empty answer", async () => {
  fetcher.mockResolvedValue(
    response({
      ...detail,
      answerable: false,
      disabled_reason: "closed",
      request: {
        ...request,
        state: "default_applied",
        resolution: { kind: "default_applied", receipt: "receipt-42" },
      },
    }),
  );
  await start();
  expect(screen.getByText("기본값 적용됨 receipt-42")).toBeTruthy();
  expect(screen.queryByText(/^답변:/)).toBeNull();
});
it("A10 uncleaned requests explain the cleanup context", async () => {
  fetcher.mockResolvedValue(
    response({ ...detail, request: { ...request, state: "uncleaned" } }),
  );
  await start();
  expect(screen.getByText(/종료된 태스크에 남은 미정리 요청/)).toBeTruthy();
  expect(
    screen.getByRole("link", { name: "이력 보기" }).getAttribute("href"),
  ).toBe("#decision-history");
});
it("A8 a refused answer restores its inputs without an unconfirmed retry", async () => {
  fetcher.mockImplementation((_path, init) =>
    Promise.resolve(
      init?.method === "POST"
        ? response({ error: "invalid_request" }, 400)
        : response(detail),
    ),
  );
  const view = render(<DecisionPage id="42" />);
  await screen.findByRole("heading", { name: "Ship it?" });
  fireEvent.click(screen.getByRole("radio", { name: /Ship/ }));
  fireEvent.change(screen.getByLabelText("메모"), {
    target: { value: "keep this" },
  });
  fireEvent.click(screen.getByRole("button", { name: "답변 확인" }));
  fireEvent.click(screen.getByRole("button", { name: "답변 보내기" }));
  expect((await screen.findByRole("alert")).textContent).toContain(
    "invalid_request",
  );
  const memo = screen.getByLabelText("메모") as HTMLTextAreaElement;
  const wait = screen.getByRole("radio", { name: /Wait/ }) as HTMLButtonElement;
  expect(memo.disabled).toBe(false);
  expect(wait.disabled).toBe(false);
  fireEvent.keyDown(window, { key: "1" });
  expect(wait.getAttribute("aria-checked")).toBe("true");
  fireEvent.change(memo, { target: { value: "corrected" } });
  expect(screen.getByRole("alert").textContent).toContain("invalid_request");
  view.unmount();
  render(<DecisionPage id="42" />);
  await screen.findByRole("heading", { name: "Ship it?" });
  expect(screen.queryByText("결과 미확인")).toBeNull();
  expect(
    screen.getByRole("radio", { name: /Wait/ }).getAttribute("aria-checked"),
  ).toBe("true");
  expect((screen.getByLabelText("메모") as HTMLTextAreaElement).value).toBe(
    "corrected",
  );
});

// ---- hk 3286 (D4): the redesigned detail page ------------------------------

it("D4 options are large radio choices; the recommended one wears a chip", async () => {
  await start();
  const options = screen.getAllByRole("radio");
  expect(options).toHaveLength(3);
  const ship = screen.getByRole("radio", { name: /Ship/ });
  const chip = within(ship).getByText("권장");
  // A chip with an icon and words, not a color: the label is printed and the
  // other options carry no such chip.
  expect(chip.className).toContain("hk-chip");
  expect(chip.querySelector("svg")).toBeTruthy();
  expect(within(screen.getByRole("radio", { name: /Wait/ })).queryByText("권장")).toBeNull();
  const css = readFileSync("src/decisions/decisions.css", "utf8");
  const at = css.indexOf(".decision-options button {");
  const block = css.slice(at, css.indexOf("}", at));
  expect(block).toContain("width: 100%");
  expect(block).toContain("min-height: var(--hk-touch-height)");
  expect(block).toContain("padding: var(--hk-space-4)");
});

it("D4 the picked option shows a check, not only a border color", async () => {
  await start();
  const wait = screen.getByRole("radio", { name: /Wait/ });
  expect(wait.querySelector("svg[data-icon='check']")).toBeNull();
  fireEvent.click(wait);
  expect(wait.getAttribute("aria-checked")).toBe("true");
  expect(wait.querySelector("svg[data-icon='check']")).toBeTruthy();
});

it("D4 answer and sheet buttons use the D1 button classes", async () => {
  await start();
  const answer = screen.getByRole("button", { name: "답변 확인" });
  expect(answer.className).toContain("hk-button--primary");
  fireEvent.click(screen.getByRole("radio", { name: /Ship/ }));
  fireEvent.click(answer);
  expect(screen.getByRole("button", { name: "답변 보내기" }).className).toContain("hk-button--primary");
  for (const name of ["수정", "취소"]) {
    const button = screen.getByRole("button", { name });
    expect(button.className).toContain("hk-button");
    expect(button.className).not.toContain("hk-button--primary");
  }
});

it("D4 the state is a chip with words, not a bullet glyph", async () => {
  fetcher.mockResolvedValue(response({ ...detail, request: { ...request, state: "overdue" } }));
  await start();
  const chip = document.querySelector(".decision-state .hk-chip");
  expect(chip?.textContent).toBe("기한 경과 · 기본값 미적용");
  expect(chip?.getAttribute("data-group")).toBe("failed");
  expect(document.body.textContent).not.toContain("●");
  expect(document.body.textContent).not.toContain("←");
});

it("D4 the page deadline uses the list's relative time, and the policy is Korean", async () => {
  vi.setSystemTime(new Date("2026-11-06T04:00:00Z"));
  await start();
  // 09:00Z due, 04:00Z now: the list would say 5시간, not 300분.
  expect(screen.getByText(/2026-11-06 18:00 KST · 5시간 남음/)).toBeTruthy();
  expect(document.body.textContent).not.toContain("분 남음");
  expect(document.querySelector(".decision-ref")?.textContent).toBe("dr-7-2 · revision 2 · 직접 답만");
});

it("D4 a passed deadline reads as elapsed in the same units", async () => {
  vi.setSystemTime(new Date("2026-11-06T12:00:00Z"));
  await start();
  expect(screen.getByText(/2026-11-06 18:00 KST · 3시간 경과/)).toBeTruthy();
});
it("shows fresh Korean question and options and lets the operator reveal originals", async () => {
  fetcher.mockImplementation(async () => response({ ...detail, request: { ...request, default_action: "Wait", question_ko: "배포할까요?", options_ko: [{key:"A",label:"대기"},{key:"B",label:"배포",recommended:true},{key:"C",label:"검토"}] } }));
  render(<DecisionPage id="42" />);
  expect(await screen.findByText("배포할까요?")).toBeTruthy();
  expect(screen.getByText(/답이 없으면 대기/)).toBeTruthy();
  expect(screen.getByRole("radio", { name: /배포/ })).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "원문 보기" }));
  expect(screen.getByText("Ship it?")).toBeTruthy();
  expect(screen.getByRole("radio", { name: /Ship/ })).toBeTruthy();
});
