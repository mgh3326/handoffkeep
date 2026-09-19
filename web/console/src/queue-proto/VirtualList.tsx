import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";

type VirtualListProps<T> = {
  items: T[];
  rowHeight: number;
  overscan?: number;
  className?: string;
  renderRow: (item: T, index: number) => ReactNode;
  getKey: (item: T, index: number) => string | number;
};

/**
 * Fixed-height windowed list. No task is ever dropped from the data — only
 * DOM rows outside the viewport are skipped, and counts stay truthful.
 * When the container reports zero viewport height (jsdom/SSR) it renders
 * every row so tests and headless dumps see the full set.
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
  const start = renderAll || !measured ? 0 : Math.max(0, Math.floor(scrollTop / rowHeight) - overscan);
  const end = renderAll
    ? items.length
    : !measured
      ? Math.min(items.length, 60)
      : Math.min(items.length, Math.ceil((scrollTop + viewportH) / rowHeight) + overscan);
  const slice = items.slice(start, end);

  return (
    <div ref={ref} className={className} onScroll={onScroll} style={{ overflowY: "auto", position: "relative" }}>
      <div style={{ height: items.length * rowHeight, position: "relative" }}>
        {slice.map((item, i) => {
          const index = start + i;
          return (
            <div key={getKey(item, index)} style={{ position: "absolute", top: index * rowHeight, left: 0, right: 0, height: rowHeight }}>
              {renderRow(item, index)}
            </div>
          );
        })}
      </div>
    </div>
  );
}
