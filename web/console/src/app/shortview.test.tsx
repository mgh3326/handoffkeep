import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import DocView from "./DocView";

const DOC_KEY = "tbls/oci/mysql/handoffkeep/doc";
const STATS_KEY = "tbls/oci/mysql/handoffkeep/stats";
const REVIEW_KEY = "tbls/oci/mysql/handoffkeep/review.json";
const SOURCE_KEY = "findings/2026-11-25/hk5584-table-io";

const HISTORY = JSON.stringify({
  v: 1,
  snapshots: [
    {
      at: "2026-11-24T08:42:02Z",
      tables: { alpha: [10, 10, 1000, 200], beta: [1, 1, 5000, 0], gamma: [1, 1, 300, 100] },
    },
    {
      at: "2026-11-25T03:15:35Z",
      tables: {
        alpha: [20, 20, 1000, 200],
        beta: [2, 2, 6000, 1000],
        gamma: [1, 1, 300, 100],
        delta: [1, 1, 9000, 0],
        epsilon: [1, 1, 10, 5],
      },
    },
  ],
});

const STATS_BODY = `# stats\n\n## History\n\n\`\`\`json table-stats-history\n${HISTORY}\n\`\`\`\n`;

const REVIEW_BODY = JSON.stringify({
  version: 1,
  read_cost: {
    window_start: "2026-11-25T04:57:28Z",
    window_end: "2026-11-25T05:27:40Z",
    source: SOURCE_KEY,
    rows: [
      { table: "beta", rows_read: 1698508 },
      { table: "zz-missing", rows_read: 42 },
    ],
  },
  cleanup: ["gamma", "zz-cleanup"],
  tables: {
    alpha: { state: "DECIDED", decision: "493", note: "done note" },
    beta: { state: "OPEN", note: "open note" },
    gamma: { state: "NOT_A_PROBLEM", finding: "findings/2026-11-25/hk1", note: "fine note" },
    delta: { state: "WAITING", until: "2026-12-10", note: "wait note" },
  },
});

type Answer = { status: number; body?: unknown };

function payload(key: string, body: string): Record<string, unknown> {
  return {
    key,
    kind: "report",
    sha256: "0".repeat(64),
    updated_at: "2026-11-25T06:00:00Z",
    format: "markdown",
    body,
  };
}

// Answers /ui/api/board/doc?key=… by the requested key, so each sibling can
// be present, missing or broken on its own. Matches the docview-schema tests.
function answers(byKey: Record<string, Answer>) {
  const fetchMock = vi.fn(async (url: string) => {
    const key = new URL(url, "http://console").searchParams.get("key") ?? "";
    const hit = byKey[key] ?? { status: 500 };
    return new Response(JSON.stringify(hit.body ?? {}), { status: hit.status });
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

function fullViewLocation() {
  window.history.pushState({}, "", `/ui/doc/${DOC_KEY}?full=1`);
}

function sectionLocation() {
  window.history.pushState({}, "", `/ui/doc/${DOC_KEY}#user-content-file-beta.md`);
}

function shortLocation() {
  window.history.pushState({}, "", `/ui/doc/${DOC_KEY}`);
}

afterEach(() => {
  vi.unstubAllGlobals();
  window.history.pushState({}, "", "/");
});

describe("short view", () => {
  it("shows three groups from stats and review", async () => {
    shortLocation();
    answers({ [DOC_KEY]: { status: 200, body: payload(DOC_KEY, "# full") }, [STATS_KEY]: { status: 200, body: payload(STATS_KEY, STATS_BODY) }, [REVIEW_KEY]: { status: 200, body: payload(REVIEW_KEY, REVIEW_BODY) } });
    render(<DocView docKey={DOC_KEY} />);
    expect(await screen.findByRole("heading", { name: "저장 용량 상위" })).toBeTruthy();
    expect(screen.getByRole("heading", { name: "읽기 비용 상위" })).toBeTruthy();
    expect(screen.getByRole("heading", { name: "정리 검토" })).toBeTruthy();
    // Largest storage: delta top 5, data plus index bytes, change since previous.
    expect(screen.getByText("9,000 bytes (신규)")).toBeTruthy();
    expect(screen.getByText("7,000 bytes (+2,000)")).toBeTruthy();
    // Highest read cost: rows read, window dates and the source as a link.
    expect(screen.getByText("1,698,508 rows")).toBeTruthy();
    expect(screen.getByRole("link", { name: SOURCE_KEY })).toHaveProperty("href", expect.stringContaining(`/ui/doc/${SOURCE_KEY}`));
    // The full document body is not fetched for the short view.
    expect(screen.queryByRole("heading", { name: "full" })).toBeNull();
  });

  it("labels every state and names its field, and a missing table is OPEN", async () => {
    shortLocation();
    answers({ [STATS_KEY]: { status: 200, body: payload(STATS_KEY, STATS_BODY) }, [REVIEW_KEY]: { status: 200, body: payload(REVIEW_KEY, REVIEW_BODY) } });
    render(<DocView docKey={DOC_KEY} />);
    expect((await screen.findAllByText("결정됨")).length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText("문제 아님").length).toBeGreaterThanOrEqual(2);
    expect(screen.getAllByText("대기").length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText("결정 493")).toBeTruthy();
    expect(screen.getByText("2026-12-10")).toBeTruthy();
    const findings = screen.getAllByRole("link", { name: "findings/2026-11-25/hk1" });
    expect(findings.length).toBeGreaterThanOrEqual(1);
    for (const finding of findings) {
      expect(finding).toHaveProperty("href", expect.stringContaining("/ui/doc/findings/2026-11-25/hk1"));
    }
    // zz-missing and zz-cleanup have no entry in tables: shown as 검토 필요.
    expect(screen.getAllByText("검토 필요").length).toBeGreaterThanOrEqual(2);
  });

  it("refuses a bad review.json with one line and still shows storage", async () => {
    shortLocation();
    answers({
      [STATS_KEY]: { status: 200, body: payload(STATS_KEY, STATS_BODY) },
      [REVIEW_KEY]: { status: 200, body: payload(REVIEW_KEY, JSON.stringify({ version: 2 })) },
    });
    render(<DocView docKey={DOC_KEY} />);
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toMatch(/version이 낯설어요: 2/);
    expect(screen.getByRole("heading", { name: "저장 용량 상위" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "읽기 비용 상위" })).toBeNull();
    expect(screen.queryByRole("heading", { name: "정리 검토" })).toBeNull();
  });

  it("names a missing review.json and still shows storage; names a missing stats too", async () => {
    shortLocation();
    answers({
      [STATS_KEY]: { status: 200, body: payload(STATS_KEY, STATS_BODY) },
      [REVIEW_KEY]: { status: 404 },
    });
    const { unmount } = render(<DocView docKey={DOC_KEY} />);
    expect(await screen.findByRole("note")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "저장 용량 상위" })).toBeTruthy();
    unmount();

    shortLocation();
    answers({
      [STATS_KEY]: { status: 404 },
      [REVIEW_KEY]: { status: 200, body: payload(REVIEW_KEY, REVIEW_BODY) },
    });
    render(<DocView docKey={DOC_KEY} />);
    expect(await screen.findByText(/통계 파일이 없습니다/)).toBeTruthy();
    expect(screen.getByRole("heading", { name: "읽기 비용 상위" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "저장 용량 상위" })).toBeNull();
  });

  it("a row and the full-document button open the full view at the table section", async () => {
    shortLocation();
    answers({ [STATS_KEY]: { status: 200, body: payload(STATS_KEY, STATS_BODY) }, [REVIEW_KEY]: { status: 200, body: payload(REVIEW_KEY, REVIEW_BODY) } });
    render(<DocView docKey={DOC_KEY} />);
    await screen.findByRole("heading", { name: "저장 용량 상위" });
    const rows = screen.getAllByRole("link", { name: "beta" });
    expect(rows.length).toBeGreaterThanOrEqual(2);
    for (const row of rows) {
      expect(row).toHaveProperty(
        "href",
        expect.stringContaining(`/ui/doc/${DOC_KEY}?full=1#user-content-file-beta.md`),
      );
    }
    expect(screen.getByRole("link", { name: "전체 문서" })).toHaveProperty(
      "href",
      expect.stringContaining(`/ui/doc/${DOC_KEY}?full=1`),
    );
  });

  it("a tbls doc key with a section opens the full view as today", async () => {
    sectionLocation();
    const fetchMock = answers({ [DOC_KEY]: { status: 200, body: payload(DOC_KEY, "# full\n\nbody") } });
    render(<DocView docKey={DOC_KEY} />);
    expect(await screen.findByRole("heading", { name: "full" })).toBeTruthy();
    expect(fetchMock).toHaveBeenCalledWith(`/ui/api/board/doc?key=${encodeURIComponent(DOC_KEY)}`);
    expect(screen.queryByRole("heading", { name: "저장 용량 상위" })).toBeNull();
  });

  it("a tbls doc key with ?full=1 opens the full view as today", async () => {
    fullViewLocation();
    answers({ [DOC_KEY]: { status: 200, body: payload(DOC_KEY, "# full\n\nbody") } });
    render(<DocView docKey={DOC_KEY} />);
    expect(await screen.findByRole("heading", { name: "full" })).toBeTruthy();
  });

  it("any other key opens the full view as today", async () => {
    const key = "decision/2026-11-25/example";
    window.history.pushState({}, "", `/ui/doc/${key}`);
    answers({ [key]: { status: 200, body: payload(key, "# Heading\n\nbody **text**") } });
    render(<DocView docKey={key} />);
    expect(await screen.findByRole("heading", { name: "Heading" })).toBeTruthy();
    expect(screen.queryByText("전체 문서")).toBeNull();
  });
});
