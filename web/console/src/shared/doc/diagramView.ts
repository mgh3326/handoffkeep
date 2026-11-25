// The view math of the diagram viewer, with no DOM: the natural size of a
// drawn SVG, the fit scale, and a zoom that keeps one point still. The viewer
// applies the result as a CSS transform on the SVG's <img>.

export type Size = { width: number; height: number };
export type Point = { x: number; y: number };
/** The stage the image is shown in, in CSS px. */
export type Box = Size;
/** The image's top-left corner on the stage, and its scale. */
export type View = { scale: number; x: number; y: number };

/** The zoom range the buttons, wheel and pinch respect (10%–800%). A fit
 * scale outside it widens the range rather than snapping the view. */
export const MIN_SCALE = 0.1;
export const MAX_SCALE = 8;
/** One step of the + and − buttons and keys. */
export const STEP = 1.25;
/** One arrow-key press, in CSS px. */
export const PAN_STEP = 40;

// Used when the SVG states no size at all. Mermaid always sets a viewBox, so
// this only guards a malformed drawing.
const FALLBACK: Size = { width: 800, height: 600 };

/** The size the SVG is drawn at, in CSS px: its viewBox, else its numeric
 * width and height. The `style` max-width Mermaid adds is ignored, so the
 * viewer shows the drawing at its own size, not the column's. */
export function naturalSize(svg: string): Size {
  const root = /<svg\b[^>]*>/i.exec(svg)?.[0] ?? "";
  const viewBox = attribute(root, "viewBox");
  if (viewBox !== null) {
    const [, , width, height] = viewBox.trim().split(/[\s,]+/).map(Number);
    if (width > 0 && height > 0) return { width, height };
  }
  const width = Number(attribute(root, "width")?.replace(/px$/, ""));
  const height = Number(attribute(root, "height")?.replace(/px$/, ""));
  if (width > 0 && height > 0) return { width, height };
  return FALLBACK;
}

function attribute(tag: string, name: string): string | null {
  const match = new RegExp(`\\s${name}\\s*=\\s*(?:"([^"]*)"|'([^']*)')`, "i").exec(tag);
  return match === null ? null : (match[1] ?? match[2]);
}

/** The scale that shows the whole image inside the stage. */
export function fitScale(size: Size, box: Box): number {
  return Math.min(box.width / size.width, box.height / size.height);
}

/** The view at `scale` with the image centred on the stage. */
export function centred(size: Size, box: Box, scale: number): View {
  return { scale, x: (box.width - size.width * scale) / 2, y: (box.height - size.height * scale) / 2 };
}

/** The bounds for a scale: the zoom range, widened to include the fit scale. */
export function clampScale(scale: number, fit: number): number {
  const min = Math.min(MIN_SCALE, fit);
  const max = Math.max(MAX_SCALE, fit);
  return Math.min(max, Math.max(min, scale));
}

/** Scales the view by `factor` (clamped) and keeps the content under `point`
 * where it was on the stage. */
export function zoomAt(view: View, factor: number, point: Point, fit: number): View {
  const scale = clampScale(view.scale * factor, fit);
  const ratio = scale / view.scale;
  return {
    scale,
    x: point.x - (point.x - view.x) * ratio,
    y: point.y - (point.y - view.y) * ratio,
  };
}
