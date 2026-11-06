import { afterEach, expect, it, vi } from "vitest";
import { postResolve } from "./api";

afterEach(() => {
  document.querySelector('meta[name="hk-csrf"]')?.remove();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});
it("A8 aborts an unresolved submission after fifteen seconds", async () => {
  vi.useFakeTimers();
  const meta = document.createElement("meta");
  meta.name = "hk-csrf";
  meta.content = "fixture";
  document.head.append(meta);
  vi.stubGlobal(
    "fetch",
    vi.fn(
      (_url: string, init: RequestInit) =>
        new Promise((_resolve, reject) =>
          init.signal?.addEventListener("abort", () =>
            reject(new Error("aborted")),
          ),
        ),
    ),
  );
  const pending = postResolve({
    request_id: "dr-7-2",
    option: "A",
    text: "memo",
    event_key: "console:42:dr-7-2:fixture",
  });
  const rejected = expect(pending).rejects.toThrow("aborted");
  await vi.advanceTimersByTimeAsync(15_000);
  await rejected;
});
