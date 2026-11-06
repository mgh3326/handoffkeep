// The console shell: brand, theme choice and the four-row navigation around a
// page (홈 · 결정 · 작업 · 배포: the decision comes second because a pending
// decision outranks everything else on the board). One <nav> serves both layouts — shell.css turns it into a fixed
// bottom tab bar below 768px and a left sidebar from 768px up — so the DOM
// never carries two copies of the links. shell.css is imported by the shell
// entry (main.tsx) and linked by the server and deploys page templates, so the
// stylesheet has one fixed name for every page that renders this component.

import { useEffect, useRef, useState, type ReactNode } from "react";
import { loadAttention } from "../home/api";
import { usePoll as poll } from "../shared/usePoll";
import { Icon } from "../shared/Icon";
import { applyTheme, loadTheme } from "../shared/theme";
import { Chip } from "../shared/ui/Chip";

export type NavKey = "home" | "work" | "decisions" | "deploys";

/** Navigation entries are data: a new page is one row here. */
export const destinations: { key: NavKey; label: string; href: string }[] = [
  { key: "home", label: "홈", href: "/ui" },
  { key: "decisions", label: "결정", href: "/ui/decisions" },
  { key: "work", label: "작업", href: "/ui/work" },
  { key: "deploys", label: "배포", href: "/ui/deploys" },
];

/** The spoken form of the decision counter, also the live announcement. */
export function decisionSummary(count: number, overdue: number): string {
  return overdue > 0 ? `열린 결정 ${count}건, 기한 경과 ${overdue}건` : `열린 결정 ${count}건`;
}

function ThemeChoice() {
  const [theme, setTheme] = useState(loadTheme);
  useEffect(() => applyTheme(theme), [theme]);
  return (
    <button type="button" className="hk-shell-theme" onClick={() => setTheme(theme === "dark" ? "light" : "dark")}>
      <Icon name={theme === "dark" ? "sun" : "moon"} />
      {theme === "dark" ? "밝은 테마" : "어두운 테마"}
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
    const next = decisionSummary(data.decisions.count, data.decisions.overdue);
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
      <nav className="hk-shell-nav" aria-label="콘솔 내비게이션">
        {destinations.map((d) => (
          <a key={d.key} href={d.href} aria-current={d.key === current ? "page" : undefined}>
            {d.label}
            {d.key === "decisions" && data && data.decisions.count > 0 && (
              <Chip kind="count" group="decision" count={data.decisions.count} label={`열린 결정 ${data.decisions.count}건`} />
            )}
            {d.key === "decisions" && data && data.decisions.overdue > 0 && (
              <>
                <span className="hk-shell-nav-dot" aria-hidden="true" />
                <span className="hk-sr-only">기한 경과 {data.decisions.overdue}건</span>
              </>
            )}
          </a>
        ))}
      </nav>
      <main className="hk-shell-main">{children}</main>
      <div className="hk-sr-only" aria-live="polite"><span key={announcement.sequence}>{announcement.text}</span></div>
    </div>
  );
}
