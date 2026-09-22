// #538 header search (2410 §5 AC4 · AC6): server-side task search in the app
// shell, rendered as text only, with keyboard pick, shortcuts that stay out of
// typing targets, and honest cap/failure states.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { HttpError } from "../board/api";
import type { BoardDetail } from "../board/types";
import { navigate, PageShell } from "./AppShell";
import { GlobalSearch } from "./GlobalSearch";
import { QueueProtoApp } from "./QueueProtoApp";
import { buildDatasets } from "./fixtures";
import type { CommentsClient } from "./TaskComments";
import { highlightParts, isTypingTarget, type SearchFn, type SearchHit, type SearchPage } from "./search";

const datasets = buildDatasets();

function hit(id: number, over: Partial<SearchHit> = {}): SearchHit {
  return { id, state: "backlog", lane: "ops", kind: "implement", title: `task ${id}`, snippet: `task ${id}`, ...over };
}

function page(results: SearchHit[], hasMore = false): SearchPage {
  return { query: "", results, hasMore, limit: 20 };
}

/** A search stub answering from a map; records every query it was asked. */
function stub(answers: Record<string, SearchPage | Error>): SearchFn & { calls: string[] } {
  const calls: string[] = [];
  const fn = (async (q: string) => {
    calls.push(q);
    const answer = answers[q];
    if (answer === undefined) {
      return page([]);
    }
    if (answer instanceof Error) {
      throw answer;
    }
    return { ...answer, query: q };
  }) as unknown as SearchFn & { calls: string[] };
  fn.calls = calls;
  return fn;
}

const box = () => screen.getByLabelText("전체 검색") as HTMLInputElement;

async function type(value: string) {
  await act(async () => {
    fireEvent.change(box(), { target: { value } });
  });
  await act(async () => {
    vi.advanceTimersByTime(200);
  });
  await act(async () => {
    await Promise.resolve();
  });
}

function detailFor(id: number, title: string, state = "merged"): BoardDetail {
  return {
    task: { id, lane: "ops", title, kind: "work", state, priority: 1, created_by: "t", created_at: "2026-09-20T00:00:00Z", updated_at: "2026-09-21T00:00:00Z", refs: {} },
    events: [],
    dwell: [],
    linear: null,
    participants: { task_ref: String(id), coverage: "collected", segments: [] },
  };
}

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  localStorage.clear();
  window.history.replaceState(null, "", "/ui/queue");
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
  delete (window as { __pwned?: unknown }).__pwned;
});

describe("snippet and title are text; only the store's markers highlight", () => {
  it("splits on the exact <b> / </b> markers and nothing else", () => {
    expect(highlightParts("a <b>hit</b> b")).toEqual([
      { text: "a ", hit: false },
      { text: "hit", hit: true },
      { text: " b", hit: false },
    ]);
    expect(highlightParts("<B>x</B> <b >y</b > &lt;b&gt;z")).toEqual([{ text: "<B>x</B> <b >y</b > &lt;b&gt;z", hit: false }]);
  });

  it("<script>, <img onerror> and entities in title and snippet render as literal text, execute nothing", async () => {
    const title = `<script>window.__pwned=1</script><img src=x onerror="window.__pwned=2"> &amp; &lt;b&gt;`;
    const snippet = `<b>deploy</b> <img src=y onerror="window.__pwned=3"><svg onload="window.__pwned=4"></svg> &quot;q&quot;`;
    const search = stub({ deploy: page([hit(71, { title, snippet }), hit(72, { title: `<b>x</b><script>1</script>`, snippet: `<b>x</b><script>1</script>` })]) });
    const { container } = render(<GlobalSearch onPick={() => {}} search={search} />);
    await type("deploy");
    const list = screen.getByRole("listbox");
    expect(list.querySelectorAll("script, img, svg:not(.hk-icon), iframe, [onerror], [onload]")).toHaveLength(0);
    expect(container.querySelectorAll("img, script")).toHaveLength(0);
    const first = within(list).getAllByRole("option")[0];
    expect(first.textContent).toContain(title);
    expect(first.textContent).toContain(`<img src=y onerror="window.__pwned=3">`);
    expect(first.textContent).toContain("&quot;q&quot;");
    const marks = [...first.querySelectorAll("mark")].map((m) => m.textContent);
    expect(marks).toEqual(["deploy"]);
    // title match: highlight rebuilt around "x", the tag after it stays text
    const second = within(list).getAllByRole("option")[1];
    expect([...second.querySelectorAll("mark")].map((m) => m.textContent)).toEqual(["x"]);
    expect(second.textContent).toContain("<script>1</script>");
    expect((window as { __pwned?: unknown }).__pwned).toBeUndefined();
  });
});

describe("global shortcut: / and ⌘K, never while typing", () => {
  it("/ and ⌘K / Ctrl+K from the page focus the search box", () => {
    render(<GlobalSearch onPick={() => {}} search={stub({})} />);
    for (const init of [{ key: "/" }, { key: "k", metaKey: true }, { key: "K", ctrlKey: true }]) {
      (document.activeElement as HTMLElement | null)?.blur();
      const event = new KeyboardEvent("keydown", { ...init, bubbles: true, cancelable: true });
      document.body.dispatchEvent(event);
      expect(document.activeElement, JSON.stringify(init)).toBe(box());
      expect(event.defaultPrevented).toBe(true);
    }
  });

  it("typing / or ⌘K in an input, a textarea (comment form), a select or contenteditable does nothing", () => {
    const { container } = render(
      <div>
        <GlobalSearch onPick={() => {}} search={stub({})} />
        <input aria-label="other" type="search" />
        <input aria-label="plain" />
        <textarea aria-label="comment" />
        <select aria-label="pick">
          <option>a</option>
        </select>
        <div contentEditable suppressContentEditableWarning>
          <span data-testid="ce">x</span>
        </div>
      </div>,
    );
    const targets = [
      screen.getByLabelText("other"),
      screen.getByLabelText("plain"),
      screen.getByLabelText("comment"),
      screen.getByLabelText("pick"),
      screen.getByTestId("ce"),
    ];
    for (const target of targets) {
      for (const init of [{ key: "/" }, { key: "k", metaKey: true }, { key: "k", ctrlKey: true }]) {
        target.focus();
        const before = document.activeElement;
        const event = new KeyboardEvent("keydown", { ...init, bubbles: true, cancelable: true });
        target.dispatchEvent(event);
        expect(event.defaultPrevented, `${target.tagName} ${JSON.stringify(init)}`).toBe(false);
        expect(document.activeElement).toBe(before);
        expect(document.activeElement).not.toBe(box());
      }
    }
    expect(container).toBeTruthy();
  });

  it("IME composition never triggers the shortcut", () => {
    render(<GlobalSearch onPick={() => {}} search={stub({})} />);
    const event = new KeyboardEvent("keydown", { key: "/", bubbles: true, cancelable: true, isComposing: true });
    document.body.dispatchEvent(event);
    expect(document.activeElement).not.toBe(box());
    expect(event.defaultPrevented).toBe(false);
  });

  it("the comment textarea inside an open task panel keeps / and ⌘K", async () => {
    window.history.replaceState(null, "", "/ui/queue?view=all&task=5005");
    const comments = { list: async () => ({ comments: [], truncated: false }), create: async () => Promise.reject(new Error("unused")), csrf: () => "csrf-token" } as unknown as CommentsClient;
    render(<QueueProtoApp datasets={datasets} initialSet="edge" search={stub({})} comments={comments} />);
    const drawer = screen.getByRole("dialog");
    fireEvent.click(within(drawer).getByRole("tab", { name: "코멘트" }));
    const textarea = await within(drawer).findByRole("textbox");
    expect(textarea.tagName).toBe("TEXTAREA");
    textarea.focus();
    for (const init of [{ key: "/" }, { key: "k", metaKey: true }]) {
      const event = new KeyboardEvent("keydown", { ...init, bubbles: true, cancelable: true });
      textarea.dispatchEvent(event);
      expect(event.defaultPrevented).toBe(false);
      expect(document.activeElement).toBe(textarea);
    }
  });

  it("checkboxes and buttons are not typing targets", () => {
    const cb = document.createElement("input");
    cb.type = "checkbox";
    expect(isTypingTarget(cb)).toBe(false);
    expect(isTypingTarget(document.createElement("button"))).toBe(false);
    expect(isTypingTarget(document.createElement("textarea"))).toBe(true);
  });
});

describe("results: exact id first, Enter once, ↑↓, Esc", () => {
  it("#494 lists the exact task first and one Enter picks it — even pressed before the answer lands", async () => {
    const answer = page([hit(494, { exact: true, title: "exact task" }), hit(1494, { title: "mentions 494" }), hit(4940)]);
    const onPick = vi.fn();
    render(<GlobalSearch onPick={onPick} search={stub({ "#494": answer, "494": answer })} />);
    await type("#494");
    const options = screen.getAllByRole("option");
    expect(options.map((o) => o.getAttribute("data-task-id"))).toEqual(["494", "1494", "4940"]);
    expect(options[0].getAttribute("aria-selected")).toBe("true");
    fireEvent.keyDown(box(), { key: "Enter" });
    expect(onPick).toHaveBeenCalledTimes(1);
    expect(onPick.mock.calls[0][0]).toBe(494);

    // Enter straight after typing (debounce not fired yet): same single pick.
    onPick.mockClear();
    await act(async () => {
      fireEvent.change(box(), { target: { value: "494" } });
      fireEvent.keyDown(box(), { key: "Enter" });
    });
    await act(async () => {
      await Promise.resolve();
    });
    expect(onPick).toHaveBeenCalledTimes(1);
    expect(onPick.mock.calls[0][0]).toBe(494);
  });

  it("↑↓ move the selection, Enter picks it, Esc closes the list then clears", async () => {
    const onPick = vi.fn();
    render(<GlobalSearch onPick={onPick} search={stub({ work: page([hit(1), hit(2), hit(3)]) })} />);
    await type("work");
    fireEvent.keyDown(box(), { key: "ArrowDown" });
    fireEvent.keyDown(box(), { key: "ArrowDown" });
    expect(screen.getAllByRole("option")[2].getAttribute("aria-selected")).toBe("true");
    expect(box().getAttribute("aria-activedescendant")).toBe(screen.getAllByRole("option")[2].id);
    fireEvent.keyDown(box(), { key: "ArrowUp" });
    fireEvent.keyDown(box(), { key: "Enter" });
    expect(onPick.mock.calls[0][0]).toBe(2);
    fireEvent.focus(box());
    expect(screen.getByRole("listbox")).toBeTruthy();
    fireEvent.keyDown(box(), { key: "Escape" });
    expect(screen.queryByRole("listbox")).toBeNull();
    expect(box().value).toBe("work");
    fireEvent.keyDown(box(), { key: "Escape" });
    expect(box().value).toBe("");
  });

  it("a slow answer for an older query never replaces the current one", async () => {
    let releaseOld: (p: SearchPage) => void = () => {};
    const search: SearchFn = (q) =>
      q === "49" ? new Promise<SearchPage>((resolve) => (releaseOld = resolve)) : Promise.resolve({ ...page([hit(494, { exact: true })]), query: q });
    const onPick = vi.fn();
    render(<GlobalSearch onPick={onPick} search={search} />);
    await type("49");
    await type("#494");
    await act(async () => {
      releaseOld(page([hit(49, { exact: true }), hit(149)]));
      await Promise.resolve();
    });
    expect(screen.queryAllByRole("option").map((o) => o.getAttribute("data-task-id"))).toEqual(["494"]);
    fireEvent.keyDown(box(), { key: "Enter" });
    expect(onPick.mock.calls.map((c) => c[0])).toEqual([494]);
  });
});

describe("closed tasks are results", () => {
  it("merged and dropped results are listed with their state", async () => {
    const search = stub({ release: page([hit(10, { state: "merged", title: "merged one" }), hit(11, { state: "dropped", title: "dropped one" }), hit(12)]) });
    render(<GlobalSearch onPick={() => {}} search={search} />);
    await type("release");
    const options = screen.getAllByRole("option");
    expect(options.map((o) => o.getAttribute("data-task-id"))).toEqual(["10", "11", "12"]);
    expect(options[0].textContent).toContain("merged one");
    expect(options[0].querySelector('[data-state-shape="merged"]')).toBeTruthy();
    expect(options[1].querySelector('[data-state-shape="dropped"]')).toBeTruthy();
    expect(screen.getByText(/닫힌 태스크\(merged · dropped\) 포함/)).toBeTruthy();
  });
});

describe("cap, failure, no permission and no results are four different answers", () => {
  it("a capped page says 더 있음, never 전부", async () => {
    render(<GlobalSearch onPick={() => {}} search={stub({ a: page([hit(1), hit(2)], true), b: page([hit(3)], false) })} />);
    await type("a");
    const more = document.querySelector('[data-search-status="more"]');
    expect(more).not.toBeNull();
    expect(more?.textContent ?? "").toMatch(/더 있음/);
    expect(document.querySelector('[data-search-status="complete"]')).toBeNull();
    expect(screen.getByRole("listbox").textContent).not.toMatch(/전부/);
    expect(more?.textContent ?? "").not.toMatch(/전부/);
    await type("b");
    expect(document.querySelector('[data-search-status="complete"]')?.textContent ?? "").toMatch(/전부/);
    expect(document.querySelector('[data-search-status="more"]')).toBeNull();
  });

  it("failure, 401/403, 400 and zero results render distinct states", async () => {
    render(
      <GlobalSearch
        onPick={() => {}}
        search={stub({
          boom: new HttpError(500),
          net: new TypeError("Failed to fetch"),
          deny: new HttpError(403),
          anon: new HttpError(401),
          bad: new HttpError(400),
          none: page([]),
        })}
      />,
    );
    const status = () => document.querySelector("[data-search-status]");
    await type("boom");
    expect(status()?.getAttribute("data-search-status")).toBe("failed");
    expect(status()?.textContent ?? "").toMatch(/조회 실패/);
    expect(status()?.textContent ?? "").not.toMatch(/^결과 없음/);
    await type("net");
    expect(status()?.getAttribute("data-search-status")).toBe("failed");
    await type("deny");
    expect(status()?.getAttribute("data-search-status")).toBe("forbidden");
    expect(status()?.textContent ?? "").toMatch(/권한 없음/);
    await type("anon");
    expect(status()?.getAttribute("data-search-status")).toBe("forbidden");
    await type("bad");
    expect(status()?.getAttribute("data-search-status")).toBe("invalid");
    await type("none");
    expect(status()?.getAttribute("data-search-status")).toBe("empty");
    expect(status()?.textContent ?? "").toMatch(/결과 없음/);
  });

  it("Enter on a failed lookup picks nothing", async () => {
    const onPick = vi.fn();
    render(<GlobalSearch onPick={onPick} search={stub({ boom: new HttpError(500) })} />);
    await type("boom");
    fireEvent.keyDown(box(), { key: "Enter" });
    await act(async () => {
      await Promise.resolve();
    });
    expect(onPick).not.toHaveBeenCalled();
  });
});

describe("app shell placement", () => {
  it("queue: one header search, distinct from the list filter; a pick opens the panel via ?task=", async () => {
    const search = stub({ "#777001": page([hit(777001, { state: "merged", exact: true, title: "closed elsewhere" })]) });
    const { container } = render(
      <QueueProtoApp datasets={datasets} initialSet="edge" search={search} fetchDetail={async (id) => detailFor(id, "closed elsewhere detail")} />,
    );
    expect(container.querySelectorAll("#qp-global-search")).toHaveLength(1);
    expect(container.querySelector(".qp-head #qp-global-search")).toBeTruthy();
    const filter = screen.getByLabelText("search") as HTMLInputElement;
    expect(filter.placeholder).toMatch(/이 목록에서 거르기/);
    expect(box().placeholder).toMatch(/전체 검색/);
    await type("#777001");
    fireEvent.keyDown(box(), { key: "Enter" });
    await screen.findByText("closed elsewhere detail");
    expect(screen.getByRole("dialog").getAttribute("aria-label")).toContain("task 777001");
    expect(new URLSearchParams(window.location.search).get("task")).toBe("777001");
    // the list filter was not touched by the header search
    expect(filter.value).toBe("");
  });

  it("queue: Esc in the search closes only the search list, not the open panel", async () => {
    window.history.replaceState(null, "", "/ui/queue?view=all&task=5005");
    render(<QueueProtoApp datasets={datasets} initialSet="edge" search={stub({ x: page([hit(1)]) })} />);
    box().focus();
    await type("x");
    fireEvent.keyDown(box(), { key: "Escape" });
    expect(screen.queryByRole("listbox")).toBeNull();
    expect(screen.getByRole("dialog")).toBeTruthy();
  });

  it("task page: the shell carries the same single search and a pick leaves for /ui/tasks/<id>", async () => {
    const to = vi.spyOn(navigate, "to").mockImplementation(() => {});
    window.history.replaceState(null, "", "/ui/tasks/5");
    const { container } = render(
      <PageShell current="queue" search={stub({ "#42": page([hit(42, { exact: true })]) })}>
        <p>page body</p>
      </PageShell>,
    );
    expect(container.querySelectorAll("#qp-global-search")).toHaveLength(1);
    await type("#42");
    fireEvent.keyDown(box(), { key: "Enter" });
    expect(to).toHaveBeenCalledWith("/ui/tasks/42");
  });

  it("an id that is not a positive safe integer never reaches a URL", async () => {
    const to = vi.spyOn(navigate, "to").mockImplementation(() => {});
    render(
      <PageShell current="queue" search={stub({ q: page([{ ...hit(1), id: "1/../../x" as unknown as number }, { ...hit(2), id: -3 }]) })}>
        <p>body</p>
      </PageShell>,
    );
    await type("q");
    fireEvent.keyDown(box(), { key: "Enter" });
    fireEvent.keyDown(box(), { key: "ArrowDown" });
    fireEvent.keyDown(box(), { key: "Enter" });
    expect(to).not.toHaveBeenCalled();
  });
});
