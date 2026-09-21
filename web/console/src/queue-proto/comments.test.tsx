// #537 코멘트 tab (hk:doc 2410 §4 AC 3·4·6): chronological list + write form,
// the #536 sanitize boundary, failures kept apart from "no comments", the
// "not an approval or a dispatch" notice, and a write that sends only the
// body and the session CSRF token.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { CommentWriteError, HttpError, postTaskComment, readCsrfToken, type TaskComments as TaskCommentsData } from "../board/api";
import type { BoardComment, BoardTask } from "../board/types";
import { boardTaskToProto } from "./boardtask";
import { QueueProtoApp } from "./QueueProtoApp";
import { TaskComments, writeRefusal, type CommentsClient } from "./TaskComments";
import { KNOWN_STATES, type Dataset } from "./types";

function mkComment(id: number, body: string, over: Partial<BoardComment> = {}): BoardComment {
  return { id, task_id: 8001, body, author: "operator:admin@example.com", created_at: `2026-09-22T0${id % 10}:00:00Z`, ...over };
}

function mkClient(over: Partial<CommentsClient> = {}): CommentsClient & { list: ReturnType<typeof vi.fn>; create: ReturnType<typeof vi.fn> } {
  return {
    list: vi.fn<CommentsClient["list"]>(() => Promise.resolve({ comments: [], truncated: false })),
    create: vi.fn<CommentsClient["create"]>((taskId, body) => Promise.resolve(mkComment(99, body, { task_id: taskId }))),
    csrf: () => "csrf-token",
    ...over,
  } as CommentsClient & { list: ReturnType<typeof vi.fn>; create: ReturnType<typeof vi.fn> };
}

function listed(data: BoardComment[], truncated = false): () => Promise<TaskCommentsData> {
  return () => Promise.resolve({ comments: data, truncated });
}

function stateOf(container: HTMLElement): string | null {
  return container.querySelector("[data-comments-state]")?.getAttribute("data-comments-state") ?? null;
}

async function settled(container: HTMLElement) {
  await waitFor(() => expect(stateOf(container)).not.toBe("loading"));
}

const NOTICE = "코멘트는 데이터입니다 — 승인·발주가 아닙니다.";

beforeEach(() => {
  delete (window as { __pwned?: number }).__pwned;
});

afterEach(() => {
  vi.unstubAllGlobals();
  document.head.querySelectorAll('meta[name="hk-csrf"]').forEach((m) => m.remove());
});

describe("comment list", () => {
  it("renders comments in the order the server returned (creation order), with author and time", async () => {
    const comments = [mkComment(3, "first"), mkComment(7, "second"), mkComment(9, "third")];
    const client = mkClient({ list: vi.fn(listed(comments)) });
    const { container } = render(<TaskComments taskId={8001} client={client} />);
    await settled(container);
    expect(stateOf(container)).toBe("ready");
    const items = [...container.querySelectorAll<HTMLElement>(".qp-comment")];
    expect(items.map((li) => li.dataset.commentId)).toEqual(["3", "7", "9"]);
    await waitFor(() => expect(items[0].querySelector("[data-doc-renderer]")).not.toBeNull());
    expect(items.map((li) => li.querySelector(".qp-comment-body")?.textContent?.trim())).toEqual(["first", "second", "third"]);
    expect(items[0].querySelector(".qp-comment-author")?.textContent).toBe("operator:admin@example.com");
    expect(items[0].querySelector("time")?.getAttribute("datetime")).toBe(comments[0].created_at);
    expect(client.list).toHaveBeenCalledWith(8001);
  });

  it("an empty list says 코멘트 없음", async () => {
    const { container } = render(<TaskComments taskId={8001} client={mkClient()} />);
    await settled(container);
    expect(stateOf(container)).toBe("empty");
    expect(container.textContent).toContain("코멘트 없음");
  });

  // Mutant: a failed read shown as "no comments".
  const failures: [string, () => Promise<TaskCommentsData>, string, string][] = [
    ["server error", () => Promise.reject(new HttpError(500)), "error", "오류 — 코멘트를 불러오지 못했습니다"],
    ["network failure", () => Promise.reject(new TypeError("Failed to fetch")), "error", "오류 — 코멘트를 불러오지 못했습니다"],
    ["unauthenticated", () => Promise.reject(new HttpError(401)), "forbidden", "권한 없음"],
    ["forbidden", () => Promise.reject(new HttpError(403)), "forbidden", "권한 없음"],
    ["missing task", () => Promise.reject(new HttpError(404)), "notfound", "태스크 없음"],
  ];
  for (const [name, list, state, text] of failures) {
    it(`${name} is its own state, never 코멘트 없음`, async () => {
      const { container } = render(<TaskComments taskId={8001} client={mkClient({ list: vi.fn(list) })} />);
      await settled(container);
      expect(stateOf(container)).toBe(state);
      expect(container.textContent).toContain(text);
      expect(container.textContent).not.toContain("코멘트 없음");
      expect(container.querySelector(".qp-comment")).toBeNull();
    });
  }

  it("a failed read can be retried", async () => {
    const list = vi.fn<CommentsClient["list"]>().mockRejectedValueOnce(new HttpError(502)).mockResolvedValueOnce({ comments: [mkComment(1, "late")], truncated: false });
    const { container } = render(<TaskComments taskId={8001} client={mkClient({ list })} />);
    await settled(container);
    expect(stateOf(container)).toBe("error");
    fireEvent.click(screen.getByRole("button", { name: "다시 불러오기" }));
    await waitFor(() => expect(stateOf(container)).toBe("ready"));
    expect(list).toHaveBeenCalledTimes(2);
  });

  it("a truncated list says only part is shown", async () => {
    const { container } = render(<TaskComments taskId={8001} client={mkClient({ list: vi.fn(listed([mkComment(1, "a")], true)) })} />);
    await settled(container);
    expect(container.textContent).toContain("일부만 보임");
  });
});

// Same hostile corpus class as the #536 body document boundary.
const HOSTILE = [
  "<script>window.__pwned = 1</script>",
  '<img src="https://tracker.example/p.png" onerror="window.__pwned = 1">',
  "[js link](javascript:window.__pwned=1)",
  '<a href="javascript:window.__pwned=1">raw anchor</a>',
  "[data link](data:text/html,<script>window.__pwned=1</script>)",
  "![tracking pixel](https://tracker.example/pixel.png)",
  "<iframe src=\"https://evil.example\"></iframe><svg onload=\"window.__pwned=1\"></svg>",
  "&lt;script&gt;entity&lt;/script&gt; and **bold**",
].join("\n\n");

describe("comment body — the #536 sanitize boundary (mutant: sanitize bypass)", () => {
  it("never executes, embeds, or fetches anything from a comment", async () => {
    const fetchSpy = vi.fn(() => Promise.reject(new Error("unexpected network")));
    vi.stubGlobal("fetch", fetchSpy);
    const { container } = render(<TaskComments taskId={8001} client={mkClient({ list: vi.fn(listed([mkComment(1, HOSTILE)])) })} />);
    await waitFor(() => expect(container.querySelector("[data-doc-renderer]")).not.toBeNull());
    const body = container.querySelector<HTMLElement>(".qp-comment-body")!;
    for (const tag of ["script", "img", "iframe", "svg", "object", "embed", "style"]) {
      expect(body.querySelectorAll(tag), tag).toHaveLength(0);
    }
    for (const el of body.querySelectorAll("*")) {
      for (const attr of el.getAttributeNames()) {
        expect(attr.startsWith("on"), `${el.tagName} ${attr}`).toBe(false);
      }
    }
    for (const a of body.querySelectorAll("a")) {
      const href = a.getAttribute("href") ?? "";
      expect(href.startsWith("https://") || href.startsWith("/") || href.startsWith("#"), href).toBe(true);
    }
    expect(within(body).getByText("js link").closest("a")).toBeNull();
    expect(body.textContent).toContain("[이미지: tracking pixel]");
    // An HTML entity stays text; markdown still works.
    expect(body.textContent).toContain("<script>entity</script>");
    expect(body.querySelector("strong")?.textContent).toBe("bold");
    expect((window as { __pwned?: number }).__pwned).toBeUndefined();
    expect(fetchSpy).not.toHaveBeenCalled();
  });
});

describe("write form", () => {
  it("shows the not-an-approval notice next to the form (mutant: notice removed)", async () => {
    const { container } = render(<TaskComments taskId={8001} client={mkClient()} />);
    await settled(container);
    const form = screen.getByRole("form", { name: "코멘트 작성" });
    const notice = within(form).getByText(NOTICE);
    expect(notice.closest("[hidden]")).toBeNull();
    expect(form.textContent).toContain("상태·우선순위·전이는 바뀌지 않고");
  });

  it("sends exactly the task id, the body and the page CSRF token — no author (mutant: author from the form)", async () => {
    const client = mkClient();
    const { container } = render(<TaskComments taskId={8001} client={client} />);
    await settled(container);
    expect(container.querySelectorAll("form input, form select")).toHaveLength(0);
    expect([...container.querySelectorAll("form textarea")].map((t) => t.getAttribute("name"))).toEqual(["body"]);
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "[decision] #8001: yes" } });
    fireEvent.click(screen.getByRole("button", { name: "코멘트 남기기" }));
    await waitFor(() => expect(container.querySelector("[data-write-state]")?.getAttribute("data-write-state")).toBe("saved"));
    expect(client.create.mock.calls).toEqual([[8001, "[decision] #8001: yes", "csrf-token"]]);
    // The saved comment is the server's record, appended to the list.
    expect([...container.querySelectorAll<HTMLElement>(".qp-comment")].map((li) => li.dataset.commentId)).toEqual(["99"]);
    expect((screen.getByRole("textbox") as HTMLTextAreaElement).value).toBe("");
  });

  it("without a CSRF token the form is disabled and nothing is sent (mutant: write without CSRF)", async () => {
    const client = mkClient({ csrf: () => null });
    const { container } = render(<TaskComments taskId={8001} client={client} />);
    await settled(container);
    expect(container.querySelector("fieldset")?.disabled).toBe(true);
    expect(container.textContent).toContain("쓰기 불가 — 이 페이지에 쓰기 토큰(CSRF)이 없습니다");
    fireEvent.submit(screen.getByRole("form", { name: "코멘트 작성" }));
    expect(client.create).not.toHaveBeenCalled();
  });

  it("an empty or oversized draft is refused locally with its own words", async () => {
    const client = mkClient();
    const { container } = render(<TaskComments taskId={8001} client={client} />);
    await settled(container);
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "   \n " } });
    fireEvent.click(screen.getByRole("button", { name: "코멘트 남기기" }));
    expect(container.querySelector(".qp-comment-result")?.textContent).toContain("빈 본문");
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "가".repeat(21846) } }); // 65,538 bytes
    expect(screen.getByTestId("comment-bytes").textContent).toContain("65,538 / 65,536");
    fireEvent.click(screen.getByRole("button", { name: "코멘트 남기기" }));
    expect(container.querySelector(".qp-comment-result")?.textContent).toContain("길이 초과");
    expect(client.create).not.toHaveBeenCalled();
  });

  it("every refused server outcome gets its own words, keeps the draft, and never reads as saved", async () => {
    const outcomes: [CommentWriteError, string][] = [
      [new CommentWriteError(400, "comment_empty"), "빈 본문"],
      [new CommentWriteError(413, "comment_too_long"), "길이 초과"],
      [new CommentWriteError(404, "not_found"), "태스크 없음"],
      [new CommentWriteError(403, "csrf_rejected"), "CSRF 거부"],
      [new CommentWriteError(403, "origin_rejected"), "출처 거부"],
      [new CommentWriteError(403, ""), "권한 없음"],
      [new CommentWriteError(401, ""), "인증 없음"],
      [new CommentWriteError(400, "comment_secret_like"), "비밀값"],
      [new CommentWriteError(400, "author_not_accepted"), "형식 거부"],
      [new CommentWriteError(500, "unavailable"), "HTTP 500"],
      [new CommentWriteError(0, "network"), "서버에 닿지 못했습니다"],
    ];
    const seen = new Set<string>();
    for (const [err, text] of outcomes) {
      const client = mkClient({ create: vi.fn(() => Promise.reject(err)), list: vi.fn(listed([mkComment(1, "kept")])) });
      const view = render(<TaskComments taskId={8001} client={client} />);
      await settled(view.container);
      fireEvent.change(within(view.container).getByRole("textbox"), { target: { value: "draft" } });
      fireEvent.click(within(view.container).getByRole("button", { name: "코멘트 남기기" }));
      const result = view.container.querySelector(".qp-comment-result")!;
      await waitFor(() => expect(result.getAttribute("data-write-state")).toBe("refused"));
      expect(result.textContent, err.code).toContain(text);
      expect(result.textContent).not.toContain("저장됨");
      expect((within(view.container).getByRole("textbox") as HTMLTextAreaElement).value).toBe("draft");
      expect(view.container.querySelectorAll(".qp-comment")).toHaveLength(1);
      seen.add(result.textContent ?? "");
      view.unmount();
    }
    expect(seen.size).toBe(outcomes.length);
    expect(writeRefusal(new Error("x"))).toContain("오류");
  });

  it("typing arrow keys in the form does not move the panel to another task", async () => {
    const tasks: BoardTask[] = [8001, 8002].map((id) => ({
      id,
      lane: "c-lane",
      title: `task ${id}`,
      kind: "implement",
      state: "claimed",
      priority: 10,
      created_by: "director",
      created_at: "2026-09-21T10:00:00Z",
      updated_at: "2026-09-21T11:00:00Z",
      refs: {},
    }));
    const dataset: Dataset = {
      key: "live",
      label: "live",
      source: "live",
      generatedAt: "2026-09-21T12:00:00Z",
      completeness: "complete",
      completenessNote: "test",
      states: [...KNOWN_STATES],
      tasks: tasks.map(boardTaskToProto),
      enrichment: {},
    };
    localStorage.clear();
    window.history.replaceState(null, "", "/ui/queue");
    const list = vi.fn<CommentsClient["list"]>((id) => Promise.resolve({ comments: [mkComment(id % 100, `for ${id}`, { task_id: id })], truncated: false }));
    const client = mkClient({ list });
    const view = render(<QueueProtoApp datasets={{ live: dataset }} initialSet="live" comments={client} />);
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    fireEvent.keyDown(view.container.querySelector<HTMLElement>('[data-task-id="8001"]')!, { key: "Enter" });
    const drawer = screen.getByRole("dialog");
    fireEvent.click(within(drawer).getByRole("tab", { name: "코멘트" }));
    await waitFor(() => expect(within(drawer).getByText("for 8001")).toBeTruthy());
    const textarea = within(drawer).getByRole("textbox");
    fireEvent.keyDown(textarea, { key: "ArrowDown" });
    expect(drawer.getAttribute("aria-label")).toContain("task 8001");
    // Moving to the next task starts that task's own list.
    fireEvent.click(within(drawer).getByRole("button", { name: "Next task" }));
    await waitFor(() => expect(within(screen.getByRole("dialog")).getByText("for 8002")).toBeTruthy());
    expect(within(screen.getByRole("dialog")).queryByText("for 8001")).toBeNull();
    expect(list.mock.calls.map((c) => c[0])).toEqual([8001, 8002]);
  });
});

describe("live comment client", () => {
  it("reads the CSRF token from the page meta tag", () => {
    expect(readCsrfToken()).toBeNull();
    const meta = document.createElement("meta");
    meta.name = "hk-csrf";
    meta.content = "tok-1";
    document.head.appendChild(meta);
    expect(readCsrfToken()).toBe("tok-1");
  });

  it("POSTs a form with only body and csrf to the console write route", async () => {
    const fetchSpy = vi.fn((_url: string, _init: RequestInit) =>
      Promise.resolve(new Response(JSON.stringify(mkComment(5, "hi")), { status: 201, headers: { "Content-Type": "application/json" } })),
    );
    vi.stubGlobal("fetch", fetchSpy);
    const saved = await postTaskComment(8001, "hi", "tok-1");
    expect(saved.id).toBe(5);
    const [url, init] = fetchSpy.mock.calls[0];
    expect(url).toBe("/ui/tasks/8001/comments");
    expect(init.method).toBe("POST");
    expect(init.credentials).toBe("same-origin");
    const sent = new URLSearchParams(String(init.body));
    expect([...sent.keys()].sort()).toEqual(["body", "csrf"]);
    expect(sent.get("body")).toBe("hi");
    expect(sent.get("csrf")).toBe("tok-1");
  });

  it("carries the server's error code, or none from a bare auth gate", async () => {
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(new Response(JSON.stringify({ error: "comment_too_long" }), { status: 413 }))));
    await expect(postTaskComment(1, "x", "t")).rejects.toMatchObject({ status: 413, code: "comment_too_long" });
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(new Response("", { status: 401 }))));
    await expect(postTaskComment(1, "x", "t")).rejects.toMatchObject({ status: 401, code: "" });
    vi.stubGlobal("fetch", vi.fn(() => Promise.reject(new TypeError("Failed to fetch"))));
    await expect(postTaskComment(1, "x", "t")).rejects.toMatchObject({ status: 0, code: "network" });
    await act(async () => {});
  });
});
