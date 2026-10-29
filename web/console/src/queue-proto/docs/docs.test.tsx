// hk 2245 document browser. The page is read-only: prefixes, one list page
// per prefix, and the selected body through the shared markdown renderer.

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { DocsApp, docsHref, orderPrefixes, parseDocsQuery } from "./DocsApp";
import type { DocSummary } from "./types";

const here = dirname(fileURLToPath(import.meta.url));
const DOCS_CSS = readFileSync(join(here, "docs.css"), "utf8");

const PINNED: DocSummary[] = [
  { key: "policy/active", title: "Active policy", kind: "note", session: "", size: 2048, updated_at: "2026-10-01T00:00:00Z" },
  { key: "policy/cycle", title: "Cycle policy", kind: "note", session: "ops", size: 512, updated_at: "2026-09-30T00:00:00Z" },
];
const MAIN: DocSummary[] = [
  { key: "reports/alpha", title: "Report Alpha", kind: "report", session: "lane-a", size: 4096, updated_at: "2026-10-02T00:00:00Z" },
  { key: "reports/beta", title: "Report Beta", kind: "report", session: "lane-b", size: 8192, updated_at: "2026-10-03T00:00:00Z" },
  { key: "decision/no-title", title: "", kind: "note", session: "", size: 10, updated_at: "2026-10-04T00:00:00Z" },
];

function page(prefix: string, documents: DocSummary[], hasMore = false, next?: string) {
  return { prefix, limit: 100, documents, has_more: hasMore, next_before: next };
}

function json(body: unknown, status = 200): Promise<Response> {
  return Promise.resolve(new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } }));
}

function stubFetch(options: { failDocs?: boolean } = {}) {
  const fetchMock = vi.fn((input: RequestInfo | URL) => {
    const url = new URL(String(input), "http://localhost");
    if (url.pathname === "/ui/api/docs/prefixes") {
      return json({
        prefixes: [
          { prefix: "reports/", count: MAIN.length },
          { prefix: "policy/", count: PINNED.length },
        ],
      });
    }
    if (url.pathname === "/ui/api/docs") {
      if (options.failDocs) {
        return Promise.reject(new Error("boom"));
      }
      const prefix = url.searchParams.get("prefix") ?? "";
      if (prefix === "policy/") {
        return json(page("policy/", PINNED));
      }
      if (prefix === "reports/") {
        return json(page("reports/", MAIN.filter((doc) => doc.key.startsWith("reports/"))));
      }
      return json(page(prefix, MAIN));
    }
    if (url.pathname === "/ui/api/board/doc") {
      const key = url.searchParams.get("key") ?? "";
      const body = key === "reports/beta" ? "# Report Beta body\n\nrendered body text\n" : `# ${key}\n\nbody\n`;
      return json({
        key,
        kind: "report",
        sha256: "a".repeat(64),
        updated_at: "2026-10-03T00:00:00Z",
        bytes: body.length,
        format: "markdown",
        body,
      });
    }
    return Promise.reject(new Error(`unexpected fetch ${url.pathname}`));
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

beforeEach(() => {
  window.history.replaceState(null, "", "/ui/docs");
  Object.defineProperty(navigator, "clipboard", {
    configurable: true,
    value: { writeText: vi.fn(() => Promise.resolve()) },
  });
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("DocsApp layout (#2245)", () => {
  it("renders the prefix tree with whole-space counts and pins policy first", async () => {
    stubFetch();
    render(<DocsApp />);
    const rail = screen.getByRole("navigation", { name: "문서 분류" });
    await waitFor(() => expect(within(rail).getByText("policy/")).toBeTruthy());
    const names = [...rail.querySelectorAll(".dx-rail-name")].map((node) => node.textContent);
    expect(names).toEqual(["policy/", "reports/"]);
    expect(within(rail).getByText("전체")).toBeTruthy();
    // pinned documents are listed above the selected prefix's documents
    const pinnedGroup = document.querySelector('[data-group="pinned"]')!;
    expect(within(pinnedGroup as HTMLElement).getByText("Active policy")).toBeTruthy();
    await waitFor(() => expect(screen.getByText("Report Beta")).toBeTruthy());
    // pinned keys are not repeated in the prefix group
    expect(document.querySelectorAll('[data-doc-key="policy/active"]')).toHaveLength(1);
  });

  it("filters the loaded page by key or title", async () => {
    stubFetch();
    render(<DocsApp />);
    await waitFor(() => expect(screen.getByText("Report Alpha")).toBeTruthy());
    fireEvent.change(screen.getByLabelText("문서 검색"), { target: { value: "beta" } });
    expect(screen.queryByText("Report Alpha")).toBeNull();
    expect(screen.getByText("Report Beta")).toBeTruthy();
    expect(screen.queryByText("Active policy")).toBeNull();
    // a document without a heading is still named by its key
    fireEvent.change(screen.getByLabelText("문서 검색"), { target: { value: "decision/" } });
    expect(document.querySelector('[data-doc-key="decision/no-title"]')).not.toBeNull();
  });

  it("opens a document through the shared markdown renderer and copies a deep link", async () => {
    stubFetch();
    render(<DocsApp />);
    await waitFor(() => expect(screen.getByText("Report Beta")).toBeTruthy());
    fireEvent.click(screen.getByText("Report Beta"));
    const reader = screen.getByLabelText("문서 본문");
    await waitFor(() => expect(reader.querySelector("[data-doc-renderer]")).not.toBeNull());
    expect(within(reader).getByText("rendered body text")).toBeTruthy();
    expect(new URLSearchParams(window.location.search).get("key")).toBe("reports/beta");

    fireEvent.click(within(reader).getByRole("button", { name: "링크 복사" }));
    await waitFor(() => expect(navigator.clipboard.writeText).toHaveBeenCalledTimes(1));
    const copied = (navigator.clipboard.writeText as ReturnType<typeof vi.fn>).mock.calls[0][0] as string;
    const parsed = new URL(copied);
    expect(parsed.pathname).toBe("/ui/docs");
    expect(parsed.searchParams.get("key")).toBe("reports/beta");
  });

  it("a failed listing is an explicit error, never an empty page", async () => {
    stubFetch({ failDocs: true });
    render(<DocsApp />);
    await waitFor(() => expect(screen.getByRole("alert").textContent).toContain("문서 목록을 불러오지 못했습니다"));
  });

  it("phone layout is CSS: one column below 640px, no color literals", () => {
    expect(DOCS_CSS).toMatch(/@media \(max-width: 640px\)/);
    expect(DOCS_CSS.replace(/\/\*[\s\S]*?\*\//g, "")).not.toMatch(/#[0-9a-f]{3,8}\b|\b(?:rgba?|hsla?)\(/i);
    expect(DOCS_CSS).toMatch(/\.dx-root\s*\{[^}]*display: grid/);
  });
});

describe("docs URL helpers", () => {
  it("parses and renders prefix/key deep links", () => {
    expect(parseDocsQuery("")).toEqual({ prefix: "", key: null });
    expect(parseDocsQuery("?prefix=policy%2F&key=policy%2Factive")).toEqual({ prefix: "policy/", key: "policy/active" });
    expect(parseDocsQuery("?key=")).toEqual({ prefix: "", key: null });
    expect(docsHref("", null)).toBe("/ui/docs");
    const parsed = new URL(docsHref("reports/", "reports/beta"), "http://localhost");
    expect(parsed.searchParams.get("prefix")).toBe("reports/");
    expect(parsed.searchParams.get("key")).toBe("reports/beta");
  });

  it("orders policy first and leaves the rest alphabetical", () => {
    expect(
      orderPrefixes([
        { prefix: "reports/", count: 1 },
        { prefix: "policy/", count: 2 },
        { prefix: "advice/", count: 3 },
      ]).map((item) => item.prefix),
    ).toEqual(["policy/", "advice/", "reports/"]);
  });
});
