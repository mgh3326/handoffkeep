import { afterEach, describe, expect, it, vi } from "vitest";
import { render, waitFor } from "@testing-library/react";
import { ReapPanel } from "../ReapPanel";
import { heldByReason, isReapResponse, type ReapResponse } from "../reap";
import fixture from "../test/reap-api.fixture.json";

// reap-api.fixture.json is the /ui/api/reap body internal/ui/reap.go built
// (projectReap) from the GET /v1/session-reap response the panewire hub
// served for its #603 fixture: one worker candidate, one builder promoted by
// a merged task, twelve held rows. It is generated, never hand-edited.
const payload = fixture as unknown as ReapResponse;

function stubFetch(body: unknown, status = 200) {
  const fetchMock = vi.fn(async () => new Response(JSON.stringify(body), { status }));
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

describe("ReapPanel (#603 정리 후보, report only)", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("lists candidates with their basis and folds held rows by reason", async () => {
    const fetchMock = stubFetch(payload);
    const { container } = render(<ReapPanel />);
    await waitFor(() => expect(container.textContent).toContain("t601-verify"));
    expect(fetchMock).toHaveBeenCalledWith("/ui/api/reap");
    const rows = [...container.querySelectorAll(".fleet-table tbody tr")];
    expect(rows).toHaveLength(2);
    const builder = rows.find((tr) => tr.textContent?.includes("b599-merged"));
    expect(builder?.querySelector("a")?.getAttribute("href")).toBe("/ui/queue?task=599");
    expect(builder?.textContent).toContain("merged");
    expect(rows.find((tr) => tr.textContent?.includes("t601-verify"))?.textContent).toContain("job.completed");
    // Held rows are never in the candidate table.
    expect(container.querySelector(".fleet-table")?.textContent).not.toContain("consult-arch");
    expect(container.textContent).toContain("보류 12건");
    expect(container.textContent).toContain("보호 표시 1");
    expect(container.textContent).toContain("사람 세션 6");
    expect(container.textContent).toContain("보고 전용");
  });

  it("offers no close control", async () => {
    stubFetch(payload);
    const { container } = render(<ReapPanel />);
    await waitFor(() => expect(container.textContent).toContain("t601-verify"));
    expect(container.querySelectorAll("button, form, input")).toHaveLength(0);
    expect(container.textContent).not.toMatch(/닫기|종료|close|kill/i);
  });

  it("an old hub is named unsupported, never 'no candidates'", async () => {
    stubFetch({ ...payload, status: "unsupported", candidates: [], held: [], nodes: [] });
    const { container } = render(<ReapPanel />);
    await waitFor(() => expect(container.textContent).toContain("허브 미지원"));
    expect(container.textContent).not.toContain("정리 후보 없음");
  });

  it("a non-reap body or HTTP failure is a failed lookup", async () => {
    stubFetch({ generated_at: "x", jobs: {}, nodes: {} });
    const { container } = render(<ReapPanel />);
    await waitFor(() => expect(container.textContent).toContain("정리 후보 조회 불가"));
    expect(container.textContent).not.toContain("정리 후보 없음");
    vi.unstubAllGlobals();
    stubFetch({}, 500);
    const second = render(<ReapPanel />);
    await waitFor(() => expect(second.container.textContent).toContain("정리 후보 조회 불가"));
  });

  it("badges every node that is not connected, even when stale is false", async () => {
    const disconnected = { ...payload, nodes: payload.nodes.map((node) => ({ ...node, state: "disconnected", stale: false })) };
    stubFetch(disconnected);
    const { container } = render(<ReapPanel />);
    await waitFor(() => expect(container.textContent).toContain("t601-verify"));
    expect([...container.querySelectorAll(".badge.stale")].map((badge) => badge.textContent)).toContain("disconnected");
  });

  it("helpers", () => {
    expect(isReapResponse(payload)).toBe(true);
    expect(isReapResponse({ status: "ok" })).toBe(false);
    expect(heldByReason(payload.held)[0][1]).toBeGreaterThanOrEqual(1);
  });
});
