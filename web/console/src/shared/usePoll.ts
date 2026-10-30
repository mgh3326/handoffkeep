import { useEffect, useState } from "react";

/** The one polling loop for console reads. The next request is scheduled only
 * after the in-flight one settles, so requests never overlap; a failure keeps
 * the last good data and sets failed, so stale numbers never pass for "now".
 * `load` must be stable (module-level or memoized): a new identity restarts
 * the loop. */
export function usePoll<T>(load: () => Promise<T>, pollMs: number) {
  const [data, setData] = useState<T | null>(null);
  const [failed, setFailed] = useState(false);
  const [at, setAt] = useState<number | null>(null);
  useEffect(() => {
    let cancelled = false;
    let timer = 0;
    const run = async () => {
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
        if (!cancelled) {
          timer = window.setTimeout(() => void run(), pollMs);
        }
      }
    };
    void run();
    return () => {
      cancelled = true;
      window.clearTimeout(timer);
    };
  }, [load, pollMs]);
  return { data, failed, at };
}
