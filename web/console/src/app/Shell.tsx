// The console shell: brand, theme choice and the four-row navigation around a
// page. One <nav> serves both layouts — shell.css turns it into a fixed
// bottom tab bar below 768px and a left sidebar from 768px up — so the DOM
// never carries two copies of the links. shell.css is imported by the shell
// entry (main.tsx) and linked by the server and deploys page templates, so the
// stylesheet has one fixed name for every page that renders this component.

import { useEffect, useRef, useState, type ReactNode } from "react";
import { loadAttention } from "../home/api";
import { usePoll as poll } from "../shared/usePoll";
import { applyTheme, loadTheme } from "../shared/theme";

export type NavKey = "home" | "work" | "decisions" | "deploys";

/** Navigation entries are data: a new page is one row here. */
export const destinations: { key: NavKey; label: string; href: string }[] = [
  { key: "home", label: "Home", href: "/ui" },
  { key: "work", label: "Work", href: "/ui/work" },
  { key: "decisions", label: "Decisions", href: "/ui/decisions" },
  { key: "deploys", label: "Deploys", href: "/ui/deploys" },
];

function ThemeChoice() {
  const [theme, setTheme] = useState(loadTheme);
  useEffect(() => applyTheme(theme), [theme]);
  return (
    <button type="button" className="hk-shell-theme" onClick={() => setTheme(theme === "dark" ? "light" : "dark")}>
      {theme === "dark" ? "Light" : "Dark"} theme
    </button>
  );
}

/** current is the destination the page belongs to; a page that is none of
 * them (a document, a task, a moved page) passes null. */
export function Shell({ current, children }: { current: NavKey | null; children: ReactNode }) {
  const { data } = poll(loadAttention, 60_000);
  const [announcement, setAnnouncement] = useState({ sequence: 0, text: "" });
  const lastBadge = useRef("");
  useEffect(() => {
    const announce = (event: Event) => {
      if (event instanceof CustomEvent && typeof event.detail === "string") setAnnouncement(x => ({ sequence: x.sequence + 1, text: event.detail }));
    };
    window.addEventListener("hk-console-announce", announce);
    return () => window.removeEventListener("hk-console-announce", announce);
  }, []);
  useEffect(() => {
    if (!data) return;
    const next = `open ${data.decisions.count} · overdue ${data.decisions.overdue}`;
    if (next !== lastBadge.current) { lastBadge.current = next; setAnnouncement(x => ({ sequence: x.sequence + 1, text: next })); }
  }, [data]);
  return (
    <div className="hk-shell">
      <header className="hk-shell-head">
        <a className="hk-shell-brand" href="/ui">
          handoffkeep
        </a>
        <ThemeChoice />
      </header>
      <nav className="hk-shell-nav" aria-label="Console navigation">
        {destinations.map((d) => (
          <a key={d.key} href={d.href} aria-current={d.key === current ? "page" : undefined}>
            {d.label}{d.key === "decisions" && data && <span className="hk-shell-nav-badge">open {data.decisions.count} · overdue {data.decisions.overdue}</span>}
          </a>
        ))}
      </nav>
      <main className="hk-shell-main">{children}</main>
      <div className="hk-sr-only" aria-live="polite"><span key={announcement.sequence}>{announcement.text}</span></div>
    </div>
  );
}
