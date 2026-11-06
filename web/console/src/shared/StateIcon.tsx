// Task state shapes — plain geometry drawn for hk (circles, strokes, a check,
// a pause, arrows), not taken from any external icon set. Every state gets a
// distinct outline so the state still reads with color removed; the color
// comes from the --hk-state-* token through currentColor and never carries
// the meaning alone — each use pairs the shape with a Korean label (visible
// in group headers, accessible name on rows).

import type { ReactNode } from "react";
import { isKnownState, stateLabel, stateToken } from "./states";


// Stroke geometry on a 24-unit grid. Exported for the distinctness test.
// New state shapes are listed first; legacy shapes follow and are kept for
// event history rendering (server writes old names into event from/to).
export const STATE_SHAPES: Record<string, ReactNode> = {
  // ---- current states (twelve post-hierarchy) ----
  triage: <path d="M5 6h14M5 12h9M5 18h5" />,
  backlog: <circle cx="12" cy="12" r="8" strokeDasharray="3 3" />,
  unstarted: (
    <>
      <circle cx="12" cy="12" r="8" />
      <path d="M9 12h6M13 9l3 3-3 3" />
    </>
  ),
  researching: (
    <>
      <circle cx="10" cy="10" r="6" />
      <line x1="14.5" y1="14.5" x2="18" y2="18" />
    </>
  ),
  developing: (
    <>
      <circle cx="12" cy="12" r="8" />
      <path d="M12 4a8 8 0 1 1-5.66 13.66z" fill="currentColor" stroke="none" />
    </>
  ),
  reviewing: (
    <>
      <circle cx="12" cy="12" r="8" />
      <path d="M8 10h8M8 14h5" />
    </>
  ),
  merged: (
    <>
      <circle cx="7" cy="6" r="2" />
      <circle cx="7" cy="18" r="2" />
      <circle cx="17" cy="12" r="2" />
      <path d="M7 8v8M7 8c0 3 3 4 8 4" />
    </>
  ),
  deployed: (
    <>
      <circle cx="12" cy="12" r="8" />
      <path d="M12 7v10M9 10l3-3 3 3" />
    </>
  ),
  completed: (
    <>
      <circle cx="12" cy="12" r="8" />
      <path d="M7 12l3 3 7-7" />
    </>
  ),
  dropped: (
    <>
      <circle cx="12" cy="12" r="8" />
      <path d="M9 9l6 6M15 9l-6 6" />
    </>
  ),
  duplicate: (
    <>
      <rect x="5" y="8" width="9" height="9" rx="1" />
      <rect x="10" y="7" width="9" height="9" rx="1" />
    </>
  ),
  icebox: <path d="M12 4v16M4 12h16M6.34 6.34l11.32 11.32M17.66 6.34L6.34 17.66" />,
  // ---- virtual awaiting_decision group ----
  awaiting_decision: (
    <>
      <circle cx="12" cy="12" r="8" />
      <path d="M9.8 9.5a2.3 2.3 0 1 1 3.4 2c-.8.5-1.2 1-1.2 1.9" />
      <circle cx="12" cy="16.6" r=".6" fill="currentColor" />
    </>
  ),
  // ---- legacy shapes — kept for event history (server writes old names) ----
  reopened: <path d="M4 11a8 8 0 1 1 2 7M4 4v7h7" />,
  claimed: (
    <>
      <circle cx="12" cy="12" r="8" />
      <circle cx="12" cy="12" r="2.5" fill="currentColor" stroke="none" />
    </>
  ),
  in_progress: (
    <>
      <circle cx="12" cy="12" r="8" />
      <path d="M12 4a8 8 0 0 1 0 16z" fill="currentColor" stroke="none" />
    </>
  ),
  verifying: (
    <>
      <circle cx="12" cy="12" r="8" />
      <path d="M8 12.5l3 3 5-6" />
    </>
  ),
  join: (
    <>
      <path d="M5 6v4a6 6 0 0 0 6 6h8" />
      <path d="M15 12l4 4-4 4" />
    </>
  ),
  hold: (
    <>
      <circle cx="12" cy="12" r="8" />
      <path d="M10 9v6M14 9v6" />
    </>
  ),
  needs_decision: (
    <>
      <circle cx="12" cy="12" r="8" />
      <path d="M9.8 9.5a2.3 2.3 0 1 1 3.4 2c-.8.5-1.2 1-1.2 1.9" />
      <circle cx="12" cy="16.6" r=".6" fill="currentColor" />
    </>
  ),
  unknown: (
    <>
      <path d="M12 3.5l8.5 8.5-8.5 8.5-8.5-8.5z" />
      <path d="M12 8.5v4" />
      <circle cx="12" cy="15.5" r=".6" fill="currentColor" />
    </>
  ),
};

type StateIconProps = {
  state: string;
  /** Accessible name rendered as visually hidden text next to the shape.
   * Omit when a visible label already names the state (group headers). */
  srLabel?: boolean;
  className?: string;
};

export function StateIcon({ state, srLabel = false, className }: StateIconProps) {
  const key = isKnownState(state) ? state : "unknown";
  return (
    <>
      <svg
        className={`hk-icon hk-state-${stateToken(state)}${className ? ` ${className}` : ""}`}
        data-state-shape={key}
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.75"
        strokeLinecap="round"
        strokeLinejoin="round"
        aria-hidden="true"
        focusable="false"
      >
        {STATE_SHAPES[key]}
      </svg>
      {srLabel ? <span className="hk-sr">{stateLabel(state)}</span> : null}
    </>
  );
}
