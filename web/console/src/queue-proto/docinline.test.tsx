import { afterEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
import { DocInline, type FetchDoc } from "./DocInline";
import { HttpError } from "../board/api";
import type { BoardDoc } from "../board/types";

function mkDoc(body: string, over: Partial<BoardDoc> = {}): BoardDoc {
  return {
    key: "design/body",
    kind: "brief",
    sha256: "a".repeat(64),
    updated_at: "2026-09-21T10:00:00Z",
    bytes: body.length,
    format: "markdown",
    body,
    ...over,
  };
}

const HOSTILE = [
  "# Heading",
  "",
  "<script>window.__pwned = 1</script>",
  "",
  '<img src="https://evil.example/pixel.png" onerror="window.__pwned = 2">',
  "",
  '<iframe src="https://evil.example/frame"></iframe>',
  "",
  '<svg onload="window.__pwned = 3"><circle r="1"/></svg>',
  "",
  '<a href="javascript:window.__pwned=4">raw anchor</a>',
  "",
  "[js link](javascript:window.__pwned=5)",
  "",
  "[data link](data:text/html,<script>alert(1)</script>)",
  "",
  "[http link](http://example.com/)",
  "",
  "[proto relative](//evil.example/x)",
  "",
  "<javascript:alert(1)>",
  "",
  "[ref link][r]",
  "",
  "[r]: javascript:alert(1)",
  "",
  "![tracking pixel](https://evil.example/pixel.gif)",
  "",
  "[safe https](https://github.com/x/y/pull/7)",
  "",
  "[same origin](/ui/tasks/5)",
  "",
  "| a | b |",
  "|---|---|",
  "| 1 | 2 |",
].join("\n");

afterEach(() => {
  vi.unstubAllGlobals();
  delete (window as { __pwned?: number }).__pwned;
});

async function renderReady(body: string, bodyDoc = "design/body") {
  const fetchDoc = vi.fn<FetchDoc>(() => Promise.resolve(mkDoc(body)));
  const view = render(<DocInline bodyDoc={bodyDoc} fetchDoc={fetchDoc} />);
  await waitFor(() => expect(view.container.querySelector("[data-doc-renderer]")).not.toBeNull());
  return { ...view, fetchDoc };
}

describe("DocInline trust boundary (mutant a)", () => {
  it("never executes, embeds, or fetches anything from the document", async () => {
    // Any request other than the one document read is a failure: external
    // images, frames and scripts must not reach the network.
    const fetchSpy = vi.fn(() => Promise.reject(new Error("unexpected network")));
    vi.stubGlobal("fetch", fetchSpy);
    const { container, fetchDoc } = await renderReady(HOSTILE);
    const body = container.querySelector("[data-doc-renderer]")!;

    expect(body.querySelector("h1")?.textContent).toBe("Heading");
    expect(body.querySelector("table")).not.toBeNull();
    for (const tag of ["script", "img", "iframe", "svg", "object", "embed", "style", "form", "input[type=image]"]) {
      expect(container.querySelectorAll(tag), tag).toHaveLength(0);
    }
    for (const el of container.querySelectorAll("*")) {
      for (const attr of el.getAttributeNames()) {
        expect(attr.startsWith("on"), `${el.tagName} ${attr}`).toBe(false);
        expect(attr, `${el.tagName} carries style`).not.toBe("style");
      }
    }
    const hrefs = [...container.querySelectorAll("a")].map((a) => a.getAttribute("href"));
    for (const href of hrefs) {
      expect(href === null || href.startsWith("https://") || href.startsWith("/") || href.startsWith("#"), String(href)).toBe(true);
      expect(href ?? "").not.toMatch(/^\/\//);
    }
    expect(hrefs).toContain("https://github.com/x/y/pull/7");
    expect(hrefs).toContain("/ui/tasks/5");
    expect(hrefs.some((h) => h?.includes("javascript") || h?.startsWith("data:") || h?.startsWith("http:"))).toBe(false);
    // The dropped links keep their text, visibly without a target.
    expect(screen.getByText("js link").closest("a")).toBeNull();
    // Links that do navigate never hand the page to the target.
    for (const a of container.querySelectorAll("a[href^='https://']")) {
      expect(a.getAttribute("rel")).toBe("noopener noreferrer");
    }
    // The image is shown as its alt text, with at most a link — no <img>.
    expect(container.textContent).toContain("[이미지: tracking pixel]");
    expect((window as { __pwned?: number }).__pwned).toBeUndefined();
    expect(fetchDoc).toHaveBeenCalledTimes(1);
    expect(fetchDoc).toHaveBeenCalledWith("design/body");
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it("the section suffix scrolls to its heading and never reaches the fetch", async () => {
    const { container, fetchDoc } = await renderReady("# Top\n\n## 3. 태스크 가 — body\n\ntext", "design/body#3");
    expect(fetchDoc).toHaveBeenCalledWith("design/body");
    await waitFor(() => expect(container.querySelector(".qp-doc-section-target")?.textContent).toBe("3. 태스크 가 — body"));
    expect(container.textContent).not.toContain("문서에서 찾지 못함");
  });

  it("a section that is not in the document is said, not guessed", async () => {
    const { container } = await renderReady("# Top\n\ntext", "design/body#9");
    await waitFor(() => expect(container.textContent).toContain("문서에서 찾지 못함"));
    expect(container.querySelector(".qp-doc-section-target")).toBeNull();
  });

  it("shows when and which version of the document is rendered", async () => {
    const { container } = await renderReady("# Top");
    expect(container.textContent).toContain("2026-09-21T10:00:00Z");
    expect(container.textContent).toContain("a".repeat(12));
    expect(screen.getByRole("link", { name: "원문 보기" }).getAttribute("href")).toBe("/ui/doc/design/body");
  });
});

describe("DocInline five states are distinct", () => {
  const cases: [string, FetchDoc, string, RegExp][] = [
    ["missing (404)", () => Promise.reject(new HttpError(404)), "missing", /문서 없음/],
    ["forbidden (403)", () => Promise.reject(new HttpError(403)), "forbidden", /권한 없음/],
    ["forbidden (401)", () => Promise.reject(new HttpError(401)), "forbidden", /권한 없음/],
    ["unsupported (json)", () => Promise.resolve(mkDoc('{"a":1}', { format: "unsupported", reason: "json" })), "unsupported", /형식 미지원/],
    ["error (500)", () => Promise.reject(new HttpError(500)), "error", /오류/],
    ["error (network)", () => Promise.reject(new TypeError("fetch failed")), "error", /오류/],
  ];
  for (const [name, fetchDoc, state, text] of cases) {
    it(name, async () => {
      const { container } = render(<DocInline bodyDoc="design/body" fetchDoc={fetchDoc} />);
      await waitFor(() => expect(container.querySelector(".qp-doc")?.getAttribute("data-doc-state")).toBe(state));
      expect(container.textContent).toMatch(text);
      expect(container.querySelector("[data-doc-renderer]")).toBeNull();
      for (const other of cases.filter((c) => c[2] !== state)) {
        expect(container.textContent).not.toMatch(other[3]);
      }
    });
  }

  it("loading is its own state", () => {
    const { container } = render(<DocInline bodyDoc="design/body" fetchDoc={() => new Promise(() => {})} />);
    expect(container.querySelector(".qp-doc")?.getAttribute("data-doc-state")).toBe("loading");
    expect(container.querySelector(".qp-doc")?.getAttribute("aria-busy")).toBe("true");
    expect(container.textContent).toContain("불러오는 중");
  });

  it("unsupported shows the raw text escaped, never rendered", async () => {
    const raw = '{"x":"<script>window.__pwned=9</script>"}';
    const { container } = render(<DocInline bodyDoc="design/body" fetchDoc={() => Promise.resolve(mkDoc(raw, { format: "unsupported", reason: "json" }))} />);
    await waitFor(() => expect(container.querySelector("pre.qp-doc-raw")?.textContent).toBe(raw));
    expect(container.querySelector("script")).toBeNull();
    expect((window as { __pwned?: number }).__pwned).toBeUndefined();
  });

  it("a non-key body_doc is unsupported and issues no request", () => {
    const fetchDoc = vi.fn<FetchDoc>();
    const { container } = render(<DocInline bodyDoc="../etc/passwd" fetchDoc={fetchDoc} />);
    expect(container.querySelector(".qp-doc")?.getAttribute("data-doc-state")).toBe("unsupported");
    expect(fetchDoc).not.toHaveBeenCalled();
  });
});

describe("DocInline request ordering", () => {
  it("a late response for the previous document never paints over the current one", async () => {
    let resolveA: (doc: BoardDoc) => void = () => {};
    const fetchDoc: FetchDoc = (key) =>
      key === "doc/a" ? new Promise<BoardDoc>((resolve) => (resolveA = resolve)) : Promise.resolve(mkDoc("# document B", { key }));
    const { container, rerender } = render(<DocInline bodyDoc="doc/a" fetchDoc={fetchDoc} />);
    rerender(<DocInline bodyDoc="doc/b" fetchDoc={fetchDoc} />);
    await waitFor(() => expect(container.querySelector("[data-doc-renderer] h1")?.textContent).toBe("document B"));
    await act(async () => {
      resolveA(mkDoc("# document A", { key: "doc/a" }));
    });
    expect(container.querySelector("[data-doc-renderer] h1")?.textContent).toBe("document B");
    expect(container.textContent).not.toContain("document A");
  });
});
