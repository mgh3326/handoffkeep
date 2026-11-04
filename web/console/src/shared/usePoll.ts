import { useEffect, useState } from "react";

/** The one polling loop for console reads. The next request is scheduled only
 * after the in-flight one settles, so requests never overlap; a failure keeps
 * the last good data and sets failed, so stale numbers never pass for "now".
 * While the tab is hidden, ticks are skipped (the loop parks); when it becomes
 * visible again, one read runs at once and the loop resumes. The first read
 * runs on mount even in a hidden tab.
 * `load` must be stable (module-level or memoized): a new identity restarts
 * the loop. */
export function usePoll<T>(load: () => Promise<T>, pollMs: number) {
  const [data, setData] = useState<T | null>(null);
  const [failed, setFailed] = useState(false);
  const [at, setAt] = useState<number | null>(null);
  useEffect(() => {
    let cancelled = false;
    let timer = 0;
    let inFlight = false;
    const tick = () => {
      if (document.hidden) return; // parked until visibilitychange
      void run();
    };
    const run = async () => {
      inFlight = true;
      try {
        const next = await load();
        if (!cancelled) {
          setData(next);
          setFailed(false);
          setAt(Date.now());
        }
      } catch {
        if (!cancelled) {
          setFailed(true);
        }
      } finally {
        inFlight = false;
        if (!cancelled) {
          timer = window.setTimeout(tick, pollMs);
        }
      }
    };
    const onVisibility = () => {
      // An in-flight read reschedules itself when it settles.
      if (document.hidden || inFlight) return;
      window.clearTimeout(timer);
      void run();
    };
    document.addEventListener("visibilitychange", onVisibility);
    void run();
    return () => {
      cancelled = true;
      window.clearTimeout(timer);
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [load, pollMs]);
  return { data, failed, at };
}
