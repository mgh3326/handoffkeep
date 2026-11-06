// The console's one icon set: 24-unit grid, 1.75 stroke, round caps, drawn in
// the same plain geometry as the StateIcon shapes (StateIcon renders through
// IconFrame below, so both share one <svg> wrapper). Names follow the Phosphor
// vocabulary named in hk doc design/2026-11-06/console-design-system §1. No
// emoji or text glyphs (×, ▸) stand in for an icon.
//
// An icon beside text is decorative and hidden from assistive technology. An
// icon that stands alone gets its accessible name from `label` (Icon) or from
// the button that holds it (IconButton).

import type { ButtonHTMLAttributes, ReactNode, SVGProps } from "react";

export const ICON_SHAPES = {
  "caret-down": <path d="M6 9.5l6 6 6-6" />,
  x: <path d="M6 6l12 12M18 6L6 18" />,
  "arrow-square-out": (
    <>
      <path d="M14 5h5v5M19 5l-8 8" />
      <path d="M17 14v4a1 1 0 0 1-1 1H6a1 1 0 0 1-1-1V8a1 1 0 0 1 1-1h4" />
    </>
  ),
  copy: (
    <>
      <rect x="9" y="9" width="10" height="10" rx="1.5" />
      <path d="M15 9V6.5A1.5 1.5 0 0 0 13.5 5h-7A1.5 1.5 0 0 0 5 6.5v7A1.5 1.5 0 0 0 6.5 15H9" />
    </>
  ),
  "magnifying-glass": (
    <>
      <circle cx="10" cy="10" r="6" />
      <line x1="14.5" y1="14.5" x2="19" y2="19" />
    </>
  ),
  funnel: <path d="M4 6h16l-6 7v5l-4 2v-7z" />,
  check: <path d="M6 12.5l4 4 8-9" />,
  warning: (
    <>
      <path d="M12 4l9 16H3z" />
      <path d="M12 10v4" />
      <circle cx="12" cy="17" r=".6" fill="currentColor" />
    </>
  ),
  sun: (
    <>
      <circle cx="12" cy="12" r="4" />
      <path d="M12 3v2M12 19v2M3 12h2M19 12h2M5.6 5.6L7 7M17 17l1.4 1.4M5.6 18.4L7 17M17 7l1.4-1.4" />
    </>
  ),
  moon: <path d="M20 14.5A8 8 0 1 1 9.5 4 6.5 6.5 0 0 0 20 14.5z" />,
} satisfies Record<string, ReactNode>;

export type IconName = keyof typeof ICON_SHAPES;

type FrameProps = Omit<SVGProps<SVGSVGElement>, "children" | "aria-label"> & {
  children: ReactNode;
  /** Accessible name of a standalone icon. Omit for a decorative one. */
  label?: string;
};

/** The shared <svg> wrapper for every shape in the console. */
export function IconFrame({ children, label, className, ...rest }: FrameProps) {
  return (
    <svg
      className={className ? `hk-icon ${className}` : "hk-icon"}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.75"
      strokeLinecap="round"
      strokeLinejoin="round"
      focusable="false"
      {...(label ? { role: "img", "aria-label": label } : { "aria-hidden": true })}
      {...rest}
    >
      {children}
    </svg>
  );
}

export function Icon({ name, label, className }: { name: IconName; label?: string; className?: string }) {
  return (
    <IconFrame label={label} className={className} data-icon={name}>
      {ICON_SHAPES[name]}
    </IconFrame>
  );
}

type IconButtonProps = Omit<ButtonHTMLAttributes<HTMLButtonElement>, "children" | "aria-label"> & {
  icon: IconName;
  /** Required: an icon-only button has no other name. */
  label: string;
};

export function IconButton({ icon, label, className, type = "button", ...rest }: IconButtonProps) {
  return (
    <button type={type} aria-label={label} className={className ? `hk-icon-button ${className}` : "hk-icon-button"} {...rest}>
      <Icon name={icon} />
    </button>
  );
}
