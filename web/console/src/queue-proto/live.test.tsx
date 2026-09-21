import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueueProtoApp } from "./QueueProtoApp";
import { boardTaskToProto, fetchLiveDataset, LiveQueue } from "./live";
import type { BoardDetail, BoardTask, BoardTasksResponse } from "../board/types";
import { KNOWN_STATES, type Dataset } from "./types";

function mkBoardTask(over: Partial<BoardTask>): BoardTask {
  return {
    id: 7001,
    lane: "live-lane-ops",
    title: "live task alpha",
    kind: "fix",
    state: "backlog",
    priority: 42,
    created_by: "live-op",
    created_at: "2026-09-20T10:00:00+09:00",
    updated_at: "2026-09-20T10:00:00+09:00",
    refs: {},
    ...over,
  };
}

function liveDataset(tasks: BoardTask[], states: string[] = [...KNOWN_STATES]): Dataset {
  return {
    key: "live",
    label: "live queue backlog",
    source: "live",
    generatedAt: "2026-09-21T09:00:00+09:00",
    completeness: "complete",
    completenessNote: "live test dataset",
    states,
    tasks: tasks.map(boardTaskToProto),
    enrichment: {},
  };
}

function mkDetail(over: Partial<BoardDetail>): BoardDetail {
  return {
    task: mkBoardTask({}),
    events: [],
    dwell: [],
    linear: null,
    participants: { task_ref: "#7001", coverage: "not_collected", segments: [] },
    ...over,
  };
}

function ddValue(drawer: HTMLElement, dtLabel: string): string {
  const dts = [...drawer.querySelectorAll("dt")];
  const dt = dts.find((d) => d.textContent?.trim() === dtLabel);
  return dt?.nextElementSibling?.textContent?.trim() ?? "<missing>";
}

function section(drawer: HTMLElement, heading: string): HTMLElement {
  const sec = [...drawer.querySelectorAll("section")].find((s) => s.querySelector("h4")?.textContent === heading);
  if (!sec) {
    throw new Error(`drawer section ${heading} missing`);
  }
  return sec;
}

function openRow(container: HTMLElement, id: number): void {
  fireEvent.keyDown(container.querySelector<HTMLElement>(`[data-task-id="${id}"]`)!, { key: "Enter" });
}

function page(tasks: BoardTask[], truncated: boolean, nextAfterId?: number, states: string[] = ["backlog"]): BoardTasksResponse {
  return { generated_at: "2026-09-21T09:00:00+09:00", states, tasks, truncated, next_after_id: nextAfterId };
}

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
}

describe("boardTaskToProto — field mapping", () => {
  it("maps direct fields and collapses claimed_by to claimant ?? null", () => {
    const proto = boardTaskToProto(mkBoardTask({ id: 9, claimed_by: "wk-1", refs: { pr: "https://github.com/x/y/pull/1" } }));
    expect(proto.id).toBe(9);
    expect(proto.claimant).toBe("wk-1");
    expect(proto.refs.pr).toBe("https://github.com/x/y/pull/1");
    expect(boardTaskToProto(mkBoardTask({})).claimant).toBeNull();
    expect(boardTaskToProto(mkBoardTask({ claimed_by: undefined })).claimant).toBeNull();
  });

  it("enters the five API-absent fields as null/[]/not_collected — never 0 or false", () => {
    const proto = boardTaskToProto(mkBoardTask({}));
    expect(proto.state_entered_at).toBeNull();
    expect(proto.due_at).toBeNull();
    expect(proto.blocker).toBeNull();
    expect(proto.dwell).toEqual([]);
    expect(proto.coverage).toEqual({ status: "not_collected", participants: null });
    expect(proto.events).toEqual([]);
    expect(proto.decision).toBeUndefined();
    // nothing fabricated: no zeroes, no empty-string stand-ins
    expect(JSON.stringify([proto.state_entered_at, proto.due_at, proto.blocker])).toBe("[null,null,null]");
  });

  it("carries updated_at and parent_lane through — absent → null, never 0 or empty string", () => {
    const proto = boardTaskToProto(mkBoardTask({ updated_at: "2026-09-21T03:00:00+09:00", parent_lane: "builder-lane" }));
    expect(proto.updated_at).toBe("2026-09-21T03:00:00+09:00");
    expect(proto.parent_lane).toBe("builder-lane");
    const bare = boardTaskToProto(mkBoardTask({}));
    expect(bare.parent_lane).toBeNull();
    // A server omission arrives as undefined — it must become null (→ unknown),
    // never an empty string or 0.
    const omitted = boardTaskToProto(mkBoardTask({ updated_at: undefined }));
    expect(omitted.updated_at).toBeNull();
    expect(JSON.stringify([bare.parent_lane, omitted.updated_at])).toBe("[null,null]");
  });
});

describe("fetchLiveDataset", () => {
  beforeEach(() => {
    vi.unstubAllGlobals();
  });

  it("walks the after_id cursor to the end and keeps the server snapshot time", async () => {
    const fetchSpy = vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.includes("after_id=7")) {
        return Promise.resolve(jsonResponse(page([mkBoardTask({ id: 8 })], false)));
      }
      return Promise.resolve(jsonResponse(page([mkBoardTask({ id: 7 })], true, 7)));
    });
    vi.stubGlobal("fetch", fetchSpy);
    const ds = await fetchLiveDataset();
    expect(ds.tasks.map((t) => t.id)).toEqual([7, 8]);
    expect(ds.source).toBe("live");
    expect(ds.generatedAt).toBe("2026-09-21T09:00:00+09:00");
    expect(ds.completeness).toBe("complete");
    expect(fetchSpy).toHaveBeenCalledTimes(2);
    expect(String(fetchSpy.mock.calls[0][0])).toContain("/ui/api/board/tasks");
    expect(String(fetchSpy.mock.calls[1][0])).toContain("after_id=7");
  });

  it("marks a client-capped walk partial and propagates fetch errors", async () => {
    const fetchSpy = vi.fn(() => Promise.resolve(jsonResponse(page([mkBoardTask({ id: 1 })], true, 0))));
    vi.stubGlobal("fetch", fetchSpy);
    const ds = await fetchLiveDataset();
    expect(ds.completeness).toBe("partial");
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(new Response("nope", { status: 500 }))));
    await expect(fetchLiveDataset()).rejects.toThrow("request failed: 500");
  });

  it("rejects a non-advancing cursor instead of re-requesting the same page to the cap", async () => {
    // Server violation: page 2 reports the same next_after_id it was asked
    // after. Without the guard this loop re-fetches the identical page until
    // the 5000-task cap — 10 wasted requests on the only data path /ui/queue has.
    const fetchSpy = vi.fn(() => Promise.resolve(jsonResponse(page([mkBoardTask({ id: 7 })], true, 7))));
    vi.stubGlobal("fetch", fetchSpy);
    await expect(fetchLiveDataset()).rejects.toThrow("non-advancing board cursor");
    expect(fetchSpy).toHaveBeenCalledTimes(2); // stops at the first repeat
  });

  it("carries the server's states enumeration — KNOWN_STATES is fallback only", async () => {
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(jsonResponse(page([mkBoardTask({ id: 1 })], false, undefined, ["backlog", "parked", "in_review"])))));
    const ds = await fetchLiveDataset();
    expect(ds.states).toEqual(["backlog", "parked", "in_review"]);
    // server sends an empty list → fallback, not an empty filter enumeration
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(jsonResponse(page([mkBoardTask({ id: 1 })], false, undefined, [])))));
    expect((await fetchLiveDataset()).states).toEqual([...KNOWN_STATES]);
    // server omits the field entirely → same fallback
    const noStates = { generated_at: "2026-09-21T09:00:00+09:00", tasks: [mkBoardTask({ id: 1 })], truncated: false };
    vi.stubGlobal("fetch", vi.fn(() => Promise.resolve(jsonResponse(noStates))));
    expect((await fetchLiveDataset()).states).toEqual([...KNOWN_STATES]);
  });
});

describe("live render — absent fields show unknown, never 0/blank (two-way)", () => {
  beforeEach(() => {
    localStorage.clear();
    window.history.replaceState(null, "", "/ui/queue");
  });

  it("drawer renders every deficit field as unknown — dwell path included", () => {
    const { container } = render(
      <QueueProtoApp datasets={{ live: liveDataset([mkBoardTask({})]) }} initialSet="live" fetchDetail={() => new Promise(() => {})} />,
    );
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    openRow(container, 7001);
    const drawer = screen.getByRole("dialog") as HTMLElement;
    expect(ddValue(drawer, "claimant")).toBe("unknown");
    expect(ddValue(drawer, "current-state age")).toContain("unknown");
    expect(ddValue(drawer, "due")).toBe("unknown");
    expect(ddValue(drawer, "blocker")).toBe("unknown");
    // dwell/coverage: detail fetch pending → honest transient state, never "0s" or blank
    expect(section(drawer, "dwell").textContent).toContain("loading");
    expect(section(drawer, "dwell").textContent).not.toContain("0s");
    expect(section(drawer, "participation coverage").textContent).toContain("loading");
    expect(within(drawer).queryByTestId("participant-count")).toBeNull();
    // two-way: any null→0 mutant makes these exact assertions fail
    for (const label of ["claimant", "due", "blocker"]) {
      expect(ddValue(drawer, label)).not.toBe("0");
      expect(ddValue(drawer, label)).not.toBe("");
    }
    // while the detail is still loading the history section says so, not 0 rows
    expect(section(drawer, "history").textContent).toContain("loading");
  });

  it("with no detail fetcher at all, absent dwell/coverage render unknown — not collected", () => {
    const { container } = render(<QueueProtoApp datasets={{ live: liveDataset([mkBoardTask({})]) }} initialSet="live" />);
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    openRow(container, 7001);
    const drawer = screen.getByRole("dialog") as HTMLElement;
    // the dangerous path: empty dwell must never look like "0s dwell" or blank
    expect(section(drawer, "dwell").textContent).toContain("unknown — not collected");
    expect(section(drawer, "dwell").textContent).not.toContain("0s");
    expect(section(drawer, "participation coverage").textContent).toContain("unknown — not collected");
  });

  it("a real claimant renders — the mapping is not unconditionally unknown", () => {
    const { container } = render(
      <QueueProtoApp
        datasets={{ live: liveDataset([mkBoardTask({ id: 7002, claimed_by: "worker-7" })]) }}
        initialSet="live"
        fetchDetail={() => new Promise(() => {})}
      />,
    );
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    openRow(container, 7002);
    expect(ddValue(screen.getByRole("dialog") as HTMLElement, "claimant")).toBe("worker-7");
  });

  it("drawer renders created_by, updated_at and parent_lane values from the list row", () => {
    const { container } = render(
      <QueueProtoApp
        datasets={{
          live: liveDataset([mkBoardTask({ id: 7001, parent_lane: "builder-lane", updated_at: "2026-09-20T15:30:00+09:00" })]),
        }}
        initialSet="live"
      />,
    );
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    openRow(container, 7001);
    const drawer = screen.getByRole("dialog") as HTMLElement;
    expect(ddValue(drawer, "created by")).toBe("live-op");
    expect(ddValue(drawer, "updated")).toBe("2026-09-20T15:30:00+09:00");
    expect(ddValue(drawer, "parent lane")).toBe("builder-lane");
  });

  it("drawer renders unknown for absent parent_lane/updated_at/created_by — never 0 or blank", () => {
    const { container } = render(
      <QueueProtoApp
        datasets={{ live: liveDataset([mkBoardTask({ id: 7001, updated_at: undefined, created_by: "" })]) }}
        initialSet="live"
      />,
    );
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    openRow(container, 7001);
    const drawer = screen.getByRole("dialog") as HTMLElement;
    for (const label of ["parent lane", "updated", "created by"]) {
      expect(ddValue(drawer, label)).toBe("unknown");
      expect(ddValue(drawer, label)).not.toBe("0");
      expect(ddValue(drawer, label)).not.toBe("");
    }
  });

  it("the states filter enumerates the dataset's server states, not the hardcoded nine", () => {
    const { container } = render(
      <QueueProtoApp datasets={{ live: liveDataset([mkBoardTask({})], ["backlog", "parked"]) }} initialSet="live" />,
    );
    const fieldset = container.querySelector<HTMLElement>(".qp-states")!;
    const labels = [...fieldset.querySelectorAll("label")].map((l) => l.textContent?.trim());
    expect(labels).toEqual(["backlog", "parked"]);
    // a KNOWN_STATES member the server did not send must not appear
    expect(labels).not.toContain("merged");
  });

  it("board layout renders a column for a server-only state the hardcoded list lacks", () => {
    const { container } = render(
      <QueueProtoApp
        datasets={{ live: liveDataset([mkBoardTask({ id: 7001, state: "parked" })], ["backlog", "parked"]) }}
        initialSet="live"
      />,
    );
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    fireEvent.click(screen.getByRole("button", { name: "board" }));
    // "parked" is not in KNOWN_STATES — the task must still get a column
    const col = container.querySelector<HTMLElement>('.qp-col[data-state="parked"]');
    expect(col).toBeTruthy();
    expect(col!.textContent).toContain("live task alpha");
    // and the "all" view itself did not hide the task for lacking a known state
    expect(container.querySelector('.qp-col[data-state="parked"] .qp-card')).toBeTruthy();
  });

  it("list rows show unknown for absent claimant and state age, not 0", () => {
    const { container } = render(<QueueProtoApp datasets={{ live: liveDataset([mkBoardTask({})]) }} initialSet="live" />);
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    const row = container.querySelector<HTMLElement>('[data-task-id="7001"]')!;
    expect(row.textContent).toContain("인수자 미상");
    expect(row.textContent).toContain("진입 미수집");
    expect(row.querySelectorAll(".qp-unknown").length).toBeGreaterThanOrEqual(2); // claimant + state age
    expect(row.textContent).not.toMatch(/진입 0|\b0d\b|진입 \d+시간/);
  });
});

describe("lazy detail fetch — no N+1 on the list path", () => {
  beforeEach(() => {
    localStorage.clear();
    window.history.replaceState(null, "", "/ui/queue");
  });

  it("fetchDetail is not called until a drawer opens, and is cached per id", async () => {
    const fetchDetail = vi.fn((id: number) => Promise.resolve(mkDetail({ task: mkBoardTask({ id }) })));
    const ds = liveDataset([mkBoardTask({ id: 7001 }), mkBoardTask({ id: 7002 }), mkBoardTask({ id: 7003 })]);
    const { container } = render(<QueueProtoApp datasets={{ live: ds }} initialSet="live" fetchDetail={fetchDetail} />);
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    // list renders every row without a single detail call
    expect(container.querySelectorAll(".qp-row").length).toBe(3);
    expect(fetchDetail).not.toHaveBeenCalled();
    openRow(container, 7001);
    await waitFor(() => expect(fetchDetail).toHaveBeenCalledTimes(1));
    expect(fetchDetail).toHaveBeenCalledWith(7001);
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    openRow(container, 7001);
    await waitFor(() => expect(section(screen.getByRole("dialog") as HTMLElement, "history")).toBeTruthy());
    expect(fetchDetail).toHaveBeenCalledTimes(1); // cached
  });

  it("loaded detail replaces unknown with real dwell, coverage and events", async () => {
    const detail = mkDetail({
      task: mkBoardTask({ id: 7001 }),
      dwell: [
        { state: "backlog", seconds: 120, open: false },
        { state: "claimed", seconds: 30, open: true },
      ],
      events: [{ id: 1, from: "backlog", to: "claimed", by: "wk-1", at: "2026-09-21T08:00:00+09:00", note: "claimed" }],
      participants: { task_ref: "#7001", coverage: "collected", segments: [{ role: "impl", model_id: "m", reps: 1, rounds: 1, blockers_found: 0, completed: 1, input_tokens: null, output_tokens: null }] },
    });
    const fetchDetail = vi.fn(() => Promise.resolve(detail));
    const { container } = render(
      <QueueProtoApp datasets={{ live: liveDataset([mkBoardTask({})]) }} initialSet="live" fetchDetail={fetchDetail} />,
    );
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    openRow(container, 7001);
    const drawer = screen.getByRole("dialog") as HTMLElement;
    await waitFor(() => expect(section(drawer, "dwell").textContent).toContain("backlog: 120s"));
    expect(section(drawer, "dwell").textContent).toContain("claimed: 30s (in progress)");
    expect(within(drawer).getByTestId("participant-count").textContent).toBe("1");
    expect(section(drawer, "history").textContent).toContain("backlog → claimed by wk-1");
    expect(section(drawer, "dwell").textContent).not.toContain("unknown — not collected");
  });

  it("a failed detail fetch marks history unavailable, never fabricated", async () => {
    const fetchDetail = vi.fn(() => Promise.reject(new Error("boom")));
    const { container } = render(
      <QueueProtoApp datasets={{ live: liveDataset([mkBoardTask({})]) }} initialSet="live" fetchDetail={fetchDetail} />,
    );
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    openRow(container, 7001);
    const drawer = screen.getByRole("dialog") as HTMLElement;
    await waitFor(() => expect(section(drawer, "history").textContent).toContain("unavailable"));
    expect(section(drawer, "history").textContent).not.toContain("0 events");
    // error state marks dwell/coverage unavailable too — not silently "not collected"
    expect(section(drawer, "dwell").textContent).toContain("unavailable");
    expect(section(drawer, "participation coverage").textContent).toContain("unavailable");
  });

  it("closing mid-fetch still caches the result — reopening never sticks on loading", async () => {
    let resolveDetail!: (d: BoardDetail) => void;
    const fetchDetail = vi.fn(() => new Promise<BoardDetail>((res) => (resolveDetail = res)));
    const { container } = render(
      <QueueProtoApp datasets={{ live: liveDataset([mkBoardTask({})]) }} initialSet="live" fetchDetail={fetchDetail} />,
    );
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    openRow(container, 7001);
    expect(fetchDetail).toHaveBeenCalledTimes(1);
    // close while the request is still in flight
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    await act(async () => {
      resolveDetail(
        mkDetail({ task: mkBoardTask({ id: 7001 }), dwell: [{ state: "backlog", seconds: 42, open: false }] }),
      );
    });
    // reopen: the completed fetch must be cached, not stuck on "loading…"
    openRow(container, 7001);
    const drawer = screen.getByRole("dialog") as HTMLElement;
    await waitFor(() => expect(section(drawer, "dwell").textContent).toContain("backlog: 42s"));
    expect(fetchDetail).toHaveBeenCalledTimes(1);
  });

  it("a failed detail fetch retries on reopen", async () => {
    const fetchDetail = vi
      .fn<() => Promise<BoardDetail>>()
      .mockRejectedValueOnce(new Error("boom"))
      .mockResolvedValue(mkDetail({ task: mkBoardTask({ id: 7001 }), dwell: [{ state: "claimed", seconds: 7, open: true }] }));
    const { container } = render(
      <QueueProtoApp datasets={{ live: liveDataset([mkBoardTask({})]) }} initialSet="live" fetchDetail={fetchDetail} />,
    );
    fireEvent.click(screen.getByRole("link", { name: "All" }));
    openRow(container, 7001);
    const drawer = screen.getByRole("dialog") as HTMLElement;
    await waitFor(() => expect(section(drawer, "history").textContent).toContain("unavailable"));
    fireEvent.keyDown(drawer, { key: "Escape" });
    openRow(container, 7001);
    await waitFor(() => expect(fetchDetail).toHaveBeenCalledTimes(2));
    await waitFor(() =>
      expect(section(screen.getByRole("dialog") as HTMLElement, "dwell").textContent).toContain("claimed: 7s"),
    );
  });
});

describe("LiveQueue mount", () => {
  beforeEach(() => {
    localStorage.clear();
    window.history.replaceState(null, "", "/ui/queue");
  });

  it("renders the live dataset with its data status — no synthetic marker, no internal endpoint", async () => {
    render(<LiveQueue loadDataset={() => Promise.resolve(liveDataset([mkBoardTask({})]))} />);
    expect(screen.getByText(/불러오는 중/)).toBeTruthy();
    const status = await screen.findByTestId("data-status");
    expect(status.textContent).toMatch(/\d\d:\d\d 확인 자료/);
    // the list API never carries these — the header says so on the default screen
    expect(status.querySelector('[data-status="not-collected"]')!.textContent).toContain("상태 진입 시각 · 기한 · 막힘");
    expect(screen.queryByText(/SYNTHETIC FIXTURE/)).toBeNull();
    // internal endpoints are developer detail, not user-facing status
    expect(document.querySelector(".qp-root")!.textContent).not.toContain("/ui/api");
    expect(screen.queryByTestId("status-line")).toBeNull();
  });

  it("a failed load renders an error — never a fallback dataset", async () => {
    render(<LiveQueue loadDataset={() => Promise.reject(new Error("request failed: 500"))} />);
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("queue unavailable");
    expect(alert.textContent).toContain("request failed: 500");
    expect(screen.queryByText(/SYNTHETIC FIXTURE/)).toBeNull();
    expect(document.querySelector(".qp-root")).toBeNull(); // app never mounts
  });
});

describe("LiveQueue polling — 15s refresh parity with the old board", () => {
  beforeEach(() => {
    localStorage.clear();
    window.history.replaceState(null, "", "/ui/queue");
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  // The mutant the contract pins: not "a timer was registered" but "the list
  // actually refreshes N seconds later".
  it("re-fetches and re-renders the list 15 seconds after the first load", async () => {
    const loadDataset = vi
      .fn<() => Promise<Dataset>>()
      .mockResolvedValueOnce(liveDataset([mkBoardTask({ title: "task alpha" })]))
      .mockResolvedValue(liveDataset([mkBoardTask({ title: "task beta" })]));
    render(<LiveQueue loadDataset={loadDataset} />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(loadDataset).toHaveBeenCalledTimes(1);
    expect(screen.getByText(/task alpha/)).toBeTruthy();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(14_999);
    });
    expect(loadDataset).toHaveBeenCalledTimes(1);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    expect(loadDataset).toHaveBeenCalledTimes(2);
    expect(screen.getByText(/task beta/)).toBeTruthy();
  });

  it("never overlaps — the next poll is scheduled only after the in-flight load settles", async () => {
    let resolveFirst!: (d: Dataset) => void;
    const loadDataset = vi
      .fn<() => Promise<Dataset>>()
      .mockImplementationOnce(() => new Promise<Dataset>((res) => (resolveFirst = res)))
      .mockResolvedValue(liveDataset([mkBoardTask({})]));
    render(<LiveQueue loadDataset={loadDataset} />);
    // A slow first load outlives several poll windows — still exactly one call.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(loadDataset).toHaveBeenCalledTimes(1);
    await act(async () => {
      resolveFirst(liveDataset([mkBoardTask({})]));
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(15_000);
    });
    expect(loadDataset).toHaveBeenCalledTimes(2);
  });

  it("a failed refresh keeps the last dataset with a warning, and recovery clears it", async () => {
    const loadDataset = vi
      .fn<() => Promise<Dataset>>()
      .mockResolvedValueOnce(liveDataset([mkBoardTask({ title: "task alpha" })]))
      .mockRejectedValueOnce(new Error("boom"))
      .mockResolvedValue(liveDataset([mkBoardTask({ title: "task alpha" })]));
    render(<LiveQueue loadDataset={loadDataset} />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(screen.getByText(/task alpha/)).toBeTruthy();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(15_000);
    });
    // last data stays on screen, flagged — the page is not torn down
    expect(screen.getByText(/갱신 실패/)).toBeTruthy();
    expect(screen.getByText(/갱신 실패/).closest('[role="status"]')).toBeTruthy();
    expect(screen.getByText(/task alpha/)).toBeTruthy();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(15_000);
    });
    expect(loadDataset).toHaveBeenCalledTimes(3);
    expect(screen.queryByText(/갱신 실패/)).toBeNull();
  });

  it("a first-load failure shows the error and keeps polling until the API returns", async () => {
    const loadDataset = vi
      .fn<() => Promise<Dataset>>()
      .mockRejectedValueOnce(new Error("request failed: 500"))
      .mockResolvedValue(liveDataset([mkBoardTask({ title: "task alpha" })]));
    render(<LiveQueue loadDataset={loadDataset} />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(screen.getByRole("alert").textContent).toContain("queue unavailable");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(15_000);
    });
    expect(loadDataset).toHaveBeenCalledTimes(2);
    expect(screen.getByText(/task alpha/)).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("unmount stops the chain — no leaked timer keeps fetching", async () => {
    const loadDataset = vi.fn<() => Promise<Dataset>>(() => Promise.resolve(liveDataset([mkBoardTask({})])));
    const { unmount } = render(<LiveQueue loadDataset={loadDataset} />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(loadDataset).toHaveBeenCalledTimes(1);
    unmount();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(loadDataset).toHaveBeenCalledTimes(1);
  });
});
