import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";

type VirtualListProps<T> = {
  items: T[];
  /** Height of every item in px, or a per-item height (group headers and
   * rows differ). */
  rowHeight: number | ((item: T, index: number) => number);
  overscan?: number;
  className?: string;
  renderRow: (item: T, index: number) => ReactNode;
  getKey: (item: T, index: number) => string | number;
};

/** First index whose item bottom lies below `y` (offsets[i] = top of i). */
function indexAt(offsets: number[], y: number): number {
  let lo = 0;
  let hi = offsets.length - 1;
  while (lo < hi) {
    const mid = (lo + hi + 1) >> 1;
    if (offsets[mid] <= y) {
      lo = mid;
    } else {
      hi = mid - 1;
    }
  }
  return lo;
}

/**
 * Windowed list with known item heights. No task is ever dropped from the
 * data — only DOM rows outside the viewport are skipped, and counts stay
 * truthful. When the container reports zero viewport height (jsdom/SSR) it
 * renders every row so tests and headless dumps see the full set.
 */
export function VirtualList<T>({ items, rowHeight, overscan = 8, className, renderRow, getKey }: VirtualListProps<T>) {
  const ref = useRef<HTMLDivElement>(null);
  const [scrollTop, setScrollTop] = useState(0);
  const [viewportH, setViewportH] = useState(0);
  const [measured, setMeasured] = useState(false);
  // Once a real viewport height has been seen, windowing stays on for good —
  // transient zero-height reports (e.g. overlay/inert toggles) can't flip the
  // list back to rendering every row. jsdom never sees a positive height, so
  // tests keep the render-all path.
  const [everLaidOut, setEverLaidOut] = useState(false);

  // offsets[i] = top of item i; offsets[items.length] = total height.
  const offsets = useMemo(() => {
    const out = new Array<number>(items.length + 1);
    out[0] = 0;
    for (let i = 0; i < items.length; i++) {
      out[i + 1] = out[i] + (typeof rowHeight === "number" ? rowHeight : rowHeight(items[i], i));
    }
    return out;
  }, [items, rowHeight]);

  useEffect(() => {
    const el = ref.current;
    if (!el) {
      return;
    }
    const measure = () => {
      const h = el.clientHeight;
      setViewportH(h);
      if (h > 0) {
        setEverLaidOut(true);
      }
    };
    measure();
    setMeasured(true);
    if (typeof ResizeObserver === "undefined") {
      return;
    }
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  const onScroll = useCallback(() => {
    if (ref.current) {
      setScrollTop(ref.current.scrollTop);
    }
  }, []);

  // Before the first measurement only a small slice is painted. Afterwards:
  // real viewport → windowed slice; never-laid-out (jsdom/headless dump) →
  // all rows so tests and DOM dumps see the full set.
  const renderAll = measured && !everLaidOut;
  let start = 0;
  let end = items.length;
  if (!renderAll) {
    if (!measured) {
      end = Math.min(items.length, 60);
    } else if (items.length > 0) {
      start = Math.max(0, indexAt(offsets, scrollTop) - overscan);
      end = Math.min(items.length, indexAt(offsets, scrollTop + viewportH) + 1 + overscan);
    }
  }
  const slice = items.slice(start, end);

  return (
    <div ref={ref} className={className} onScroll={onScroll} style={{ overflowY: "auto", position: "relative" }}>
      <div style={{ height: offsets[items.length], position: "relative" }}>
        {slice.map((item, i) => {
          const index = start + i;
          return (
            <div
              key={getKey(item, index)}
              style={{ position: "absolute", top: offsets[index], left: 0, right: 0, height: offsets[index + 1] - offsets[index] }}
            >
              {renderRow(item, index)}
            </div>
          );
        })}
      </div>
    </div>
  );
}
