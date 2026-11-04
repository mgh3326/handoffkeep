import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { usePoll } from "./usePoll";

let hidden = false;

function setHidden(next: boolean) {
  hidden = next;
  document.dispatchEvent(new Event("visibilitychange"));
}

// Let the resolved load promise and its finally block run.
async function settle() {
  await act(async () => {
    await Promise.resolve();
    await Promise.resolve();
  });
}

describe("usePoll", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    hidden = false;
    vi.spyOn(document, "hidden", "get").mockImplementation(() => hidden);
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it("polls while visible", async () => {
    const load = vi.fn(async () => 1);
    renderHook(() => usePoll(load, 1000));
    await settle();
    expect(load).toHaveBeenCalledTimes(1);
    await act(async () => {
      vi.advanceTimersByTime(1000);
    });
    await settle();
    expect(load).toHaveBeenCalledTimes(2);
  });

  it("skips ticks while hidden and reads once at once when visible again", async () => {
    const load = vi.fn(async () => 1);
    const { result } = renderHook(() => usePoll(load, 1000));
    await settle();
    expect(load).toHaveBeenCalledTimes(1);
    act(() => setHidden(true));
    await act(async () => {
      vi.advanceTimersByTime(5000);
    });
    await settle();
    expect(load).toHaveBeenCalledTimes(1);
    act(() => setHidden(false));
    await settle();
    expect(load).toHaveBeenCalledTimes(2);
    expect(result.current.data).toBe(1);
    // The loop resumes on its normal cadence, with no second immediate read.
    await act(async () => {
      vi.advanceTimersByTime(999);
    });
    await settle();
    expect(load).toHaveBeenCalledTimes(2);
    await act(async () => {
      vi.advanceTimersByTime(1);
    });
    await settle();
    expect(load).toHaveBeenCalledTimes(3);
  });

  it("does not overlap a read in flight when the tab becomes visible", async () => {
    let release = () => {};
    const load = vi.fn(
      () =>
        new Promise<number>((resolve) => {
          release = () => resolve(1);
        }),
    );
    renderHook(() => usePoll(load, 1000));
    act(() => setHidden(true));
    act(() => setHidden(false));
    expect(load).toHaveBeenCalledTimes(1);
    release();
    await settle();
    expect(load).toHaveBeenCalledTimes(1);
    await act(async () => {
      vi.advanceTimersByTime(1000);
    });
    expect(load).toHaveBeenCalledTimes(2);
  });

  it("stops listening after unmount", async () => {
    const load = vi.fn(async () => 1);
    const { unmount } = renderHook(() => usePoll(load, 1000));
    await settle();
    unmount();
    act(() => setHidden(true));
    act(() => setHidden(false));
    await settle();
    expect(load).toHaveBeenCalledTimes(1);
  });
});
