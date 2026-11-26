// The full-window view of one drawn diagram. It shows the same SVG image the
// document shows, at its natural size; zoom and pan are a CSS transform on that
// image, so it stays vector at every scale. The controls are hk's own: buttons,
// keys, the wheel and pointers. Nothing inside the SVG runs.

import { useCallback, useEffect, useLayoutEffect, useRef, useState, type KeyboardEvent, type PointerEvent } from "react";
import { createPortal } from "react-dom";
import { dialogKeys } from "../actions/dialogs";
import { PAN_STEP, STEP, centred, fitScale, zoomAt, type Box, type Point, type Size, type View } from "./diagramView";

// Wheel distance to a zoom factor: one notch (about 100px) is about ×1.16.
const WHEEL_RATE = 0.0015;

type Props = { src: string; size: Size; onClose: () => void };

function boxOf(element: HTMLElement | null): Box {
  const rect = element?.getBoundingClientRect();
  if (rect && rect.width > 0 && rect.height > 0) return { width: rect.width, height: rect.height };
  return { width: window.innerWidth, height: window.innerHeight };
}

function pointIn(element: HTMLElement, clientX: number, clientY: number): Point {
  const rect = element.getBoundingClientRect();
  return { x: clientX - rect.left, y: clientY - rect.top };
}

function distance(a: Point, b: Point): number {
  return Math.hypot(a.x - b.x, a.y - b.y);
}

function midpoint(a: Point, b: Point): Point {
  return { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2 };
}

export default function DiagramViewer({ src, size, onClose }: Props) {
  const root = useRef<HTMLDivElement>(null);
  const stage = useRef<HTMLDivElement>(null);
  const closeButton = useRef<HTMLButtonElement>(null);
  // The fit scale bounds the zoom range (see clampScale). The handlers read it
  // from a ref so they do not re-subscribe when it changes.
  const fit = useRef(1);
  const pointers = useRef(new Map<number, Point>());
  const pressedBackdrop = useRef(false);
  const [view, setView] = useState<View>({ scale: 1, x: 0, y: 0 });

  const fitAll = useCallback(() => {
    const box = boxOf(stage.current);
    fit.current = fitScale(size, box);
    setView(centred(size, box, fit.current));
  }, [size]);

  // 100%: one SVG unit is one CSS px, the image centred.
  const actual = useCallback(() => {
    setView(centred(size, boxOf(stage.current), 1));
  }, [size]);

  const centre = useCallback((): Point => {
    const box = boxOf(stage.current);
    return { x: box.width / 2, y: box.height / 2 };
  }, []);

  const zoom = useCallback((factor: number, point: Point) => {
    setView(current => zoomAt(current, factor, point, fit.current));
  }, []);

  const pan = useCallback((dx: number, dy: number) => {
    setView(current => ({ ...current, x: current.x + dx, y: current.y + dy }));
  }, []);

  // The viewer opens at Fit, with the close button focused. useLayoutEffect
  // so the first paint is already fitted.
  useLayoutEffect(() => {
    fitAll();
    closeButton.current?.focus();
  }, [fitAll]);

  // A native listener, not React's onWheel: React attaches wheel listeners as
  // passive, and the page must not scroll behind the viewer.
  useEffect(() => {
    const element = root.current;
    if (element === null) return;
    const onWheel = (event: WheelEvent) => {
      event.preventDefault();
      const lines = event.deltaMode === 1 ? 16 : 1;
      const point = stage.current === null ? centre() : pointIn(stage.current, event.clientX, event.clientY);
      zoom(Math.exp(-event.deltaY * lines * WHEEL_RATE), point);
    };
    element.addEventListener("wheel", onWheel, { passive: false });
    return () => element.removeEventListener("wheel", onWheel);
  }, [centre, zoom]);

  const onPointerDown = (event: PointerEvent<HTMLDivElement>) => {
    if (event.button !== 0 || stage.current === null) return;
    // Touch and pen report button 0 too; a middle or right click is not a drag.
    event.currentTarget.setPointerCapture?.(event.pointerId);
    // The stage is not focusable, so a press would otherwise drop focus to the
    // body and take the keys with it. Focus the dialog root (tabIndex -1), which
    // keeps the keys and the Tab trap on this viewer.
    root.current?.focus({ preventScroll: true });
    pointers.current.set(event.pointerId, pointIn(stage.current, event.clientX, event.clientY));
  };

  const onPointerMove = (event: PointerEvent<HTMLDivElement>) => {
    const previous = pointers.current.get(event.pointerId);
    if (previous === undefined || stage.current === null) return;
    const next = pointIn(stage.current, event.clientX, event.clientY);
    pointers.current.set(event.pointerId, next);
    const other = [...pointers.current].find(([id]) => id !== event.pointerId)?.[1];
    if (other === undefined) {
      pan(next.x - previous.x, next.y - previous.y);
      return;
    }
    // Two pointers: pinch zooms about the midpoint where the gesture was, and
    // the midpoint's movement pans.
    const before = distance(previous, other);
    const after = distance(next, other);
    const midBefore = midpoint(previous, other);
    const midAfter = midpoint(next, other);
    setView(current => {
      const zoomed = before > 0 && after > 0 ? zoomAt(current, after / before, midBefore, fit.current) : current;
      return { ...zoomed, x: zoomed.x + midAfter.x - midBefore.x, y: zoomed.y + midAfter.y - midBefore.y };
    });
  };

  const onPointerEnd = (event: PointerEvent<HTMLDivElement>) => {
    pointers.current.delete(event.pointerId);
    if (event.currentTarget.hasPointerCapture?.(event.pointerId)) {
      event.currentTarget.releasePointerCapture(event.pointerId);
    }
  };

  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    // Shift+Tab from the dialog root itself (after a press on the stage) would
    // leave the dialog backwards; send it to the last control, as the trap does.
    if (event.key === "Tab" && event.shiftKey && event.target === root.current) {
      event.preventDefault();
      closeButton.current?.focus();
      return;
    }
    // Escape closes and Tab stays inside the dialog, as in every hk dialog.
    dialogKeys(event, onClose);
    if (event.defaultPrevented) return;
    switch (event.key) {
      case "+":
      case "=":
        zoom(STEP, centre());
        break;
      case "-":
      case "_":
      case "−":
        zoom(1 / STEP, centre());
        break;
      case "0":
        fitAll();
        break;
      case "ArrowLeft":
        pan(-PAN_STEP, 0);
        break;
      case "ArrowRight":
        pan(PAN_STEP, 0);
        break;
      case "ArrowUp":
        pan(0, -PAN_STEP);
        break;
      case "ArrowDown":
        pan(0, PAN_STEP);
        break;
      default:
        return;
    }
    // Handled here, so a page shortcut behind the viewer does not also fire.
    event.preventDefault();
    event.stopPropagation();
  };

  const percent = Math.round(view.scale * 100);

  return createPortal(
    <div
      ref={root}
      className="hk-diagram-viewer"
      tabIndex={-1}
      role="dialog"
      aria-modal="true"
      aria-label="Diagram viewer"
      onKeyDown={onKeyDown}
      onPointerDown={event => { pressedBackdrop.current = event.target === event.currentTarget; }}
      onClick={event => { if (pressedBackdrop.current && event.target === event.currentTarget) onClose(); }}
    >
      <div className="hk-diagram-viewer-bar">
        <button type="button" className="hk-button" aria-label="Zoom out" onClick={() => zoom(1 / STEP, centre())}>−</button>
        <button type="button" className="hk-button" aria-label="Zoom in" onClick={() => zoom(STEP, centre())}>+</button>
        <button type="button" className="hk-button" onClick={fitAll}>Fit</button>
        <button type="button" className="hk-button" onClick={actual}>100%</button>
        <output className="hk-diagram-viewer-zoom" aria-live="polite">{percent}%</output>
        <button ref={closeButton} type="button" className="hk-button" onClick={onClose}>Close</button>
      </div>
      <div
        ref={stage}
        className="hk-diagram-viewer-stage"
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerEnd}
        onPointerCancel={onPointerEnd}
      >
        <img
          className="hk-diagram-viewer-image"
          src={src}
          alt="Diagram"
          draggable={false}
          style={{ width: size.width, height: size.height, transform: `translate(${view.x}px, ${view.y}px) scale(${view.scale})` }}
        />
      </div>
    </div>,
    document.body,
  );
}
