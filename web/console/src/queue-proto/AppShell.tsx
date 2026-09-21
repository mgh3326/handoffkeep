// Console shell pieces shared by the queue and the full-page task view:
// the global navigation block of the left sidebar and the theme choice.

import { useState, type ReactNode } from "react";
import type { Density } from "./types";

export type Theme = "dark" | "light";

export const THEME_KEY = "hk-console:theme";

/** Dark is the default and the only operator-approved theme; light applies
 * only after the viewer explicitly picks it. Storage is resolved inside the
 * try: in a sandboxed or storage-blocked browser even reading the
 * localStorage property throws, and that must never stop the queue from
 * mounting. */
export function loadTheme(storage?: Pick<Storage, "getItem">): Theme {
  try {
    return (storage ?? globalThis.localStorage)?.getItem(THEME_KEY) === "light" ? "light" : "dark";
  } catch {
    return "dark";
  }
}

export function applyTheme(theme: Theme, storage?: Pick<Storage, "setItem">): void {
  if (theme === "light") {
    document.documentElement.dataset.theme = "light";
  } else {
    delete document.documentElement.dataset.theme;
  }
  try {
    (storage ?? globalThis.localStorage)?.setItem(THEME_KEY, theme);
  } catch {
    // storage unavailable — the choice lasts for this page only
  }
}

export type NavKey = "queue" | "decisions" | "fleet" | "timeline" | "compose";

/** Global destinations. Home (#500) does not exist yet, so it is listed as
 * not-yet-available text instead of a link that leads nowhere. */
const NAV_LINKS: { key: NavKey; label: string; href: string }[] = [
  { key: "queue", label: "Queue", href: "/ui/queue" },
  { key: "decisions", label: "Decisions", href: "/ui/decisions" },
  { key: "fleet", label: "Fleet", href: "/ui/fleet" },
  { key: "timeline", label: "Timeline", href: "/ui/timeline" },
  { key: "compose", label: "Compose", href: "/ui/compose" },
];

export function GlobalNav({ current }: { current: NavKey }) {
  return (
    <>
      <div className="hk-brand">Fleet console</div>
      <nav className="hk-side-nav" aria-label="전역 탐색">
        <span className="hk-side-item hk-side-soon">
          <span>홈</span>
          <span className="hk-side-n">준비 중</span>
        </span>
        {NAV_LINKS.map((link) => (
          <a
            key={link.key}
            href={link.href}
            className={`hk-side-item${link.key === "compose" ? " hk-side-aux" : ""}`}
            aria-current={link.key === current ? "page" : undefined}
          >
            <span>{link.label}</span>
          </a>
        ))}
      </nav>
    </>
  );
}

const DENSITY_LABEL: Record<Density, string> = {
  compact: "행 40px · 1줄",
  comfortable: "행 56px · 2줄",
};

/** Row density choice — a per-viewer display preference persisted with the
 * rest of the presentation state. 40px · 1 line is the default. */
export function DensityChoice({ density, onChange }: { density: Density; onChange: (d: Density) => void }) {
  return (
    <div className="hk-seg hk-density" role="group" aria-label="행 밀도">
      {(["compact", "comfortable"] as Density[]).map((d) => (
        <button key={d} type="button" className="hk-btn" aria-pressed={density === d} onClick={() => onChange(d)}>
          {DENSITY_LABEL[d]}
        </button>
      ))}
    </div>
  );
}

export function ThemeChoice() {
  const [theme, setTheme] = useState<Theme>(() => loadTheme());
  const pick = (next: Theme) => {
    setTheme(next);
    applyTheme(next);
  };
  return (
    <div className="hk-seg hk-theme" role="group" aria-label="테마">
      {(["dark", "light"] as Theme[]).map((t) => (
        <button key={t} type="button" className="hk-btn" aria-pressed={theme === t} onClick={() => pick(t)}>
          {t === "dark" ? "다크" : "라이트"}
        </button>
      ))}
    </div>
  );
}

/** Full-page shell for routes that have no view rail (/ui/tasks/<id>). */
export function PageShell({ current, children }: { current: NavKey; children: ReactNode }) {
  return (
    <div className="hk-page">
      <aside className="hk-side" aria-label="콘솔 탐색">
        <GlobalNav current={current} />
        <div className="hk-side-foot">
          <ThemeChoice />
        </div>
      </aside>
      <main className="hk-page-main">{children}</main>
    </div>
  );
}
