import { useEffect, useRef, useState, type ReactNode } from "react";
import type { SearchFn } from "./search";
import { fetchTaskSearch, isTypingTarget, safeTaskId } from "./search";
import { readStored, STORAGE_KEYS, writeStored } from "./storage";
import "../design/shell.css";

export type Theme = "dark" | "light";
export const THEME_KEY = STORAGE_KEYS.theme;
export function loadTheme(storage?: Pick<Storage, "getItem">): Theme {
  return readStored(THEME_KEY, storage) === "light" ? "light" : "dark";
}
export function applyTheme(
  theme: Theme,
  storage?: Pick<Storage, "setItem">,
): void {
  document.documentElement.dataset.theme = theme;
  writeStored(THEME_KEY, theme, storage);
}
/** Applies the stored theme before the first render. Every entry calls it
 * first, so a light-theme page never flashes dark. Dark is the default. */
export function bootTheme(): void {
  if (loadTheme() === "light") {
    document.documentElement.dataset.theme = "light";
  }
}
export type NavKey =
  | "home"
  | "queue"
  | "deploys"
  | "docs"
  | "decisions"
  | "fleet"
  | "grades"
  | "timeline"
  | "compose";
export const destinations: {
  key: NavKey;
  label: string;
  href: string;
  shortcut?: string;
  /** Secondary item, styled smaller below the primary list. */
  aux?: boolean;
  /** Group heading rendered above this item. */
  heading?: string;
  /** A second entry for the same page: never marked as the current page. */
  alias?: boolean;
}[] = [
  { key: "home", label: "Inbox", href: "/ui/home", shortcut: "i" },
  { key: "queue", label: "My work / Board", href: "/ui/queue", shortcut: "b" },
  { key: "queue", label: "Tasks", href: "/ui/queue?view=all", shortcut: "t", alias: true },
  {
    key: "decisions",
    label: "Decisions",
    href: "/ui/decisions",
    shortcut: "d",
  },
  { key: "deploys", label: "Deploys", href: "/ui/deploys", shortcut: "e" },
  { key: "fleet", label: "Fleet hosts", href: "/ui/fleet", shortcut: "h" },
  { key: "docs", label: "Docs / Search", href: "/ui/docs", shortcut: "s" },
  { key: "grades", label: "Grades / catalog", href: "/ui/grades", aux: true, heading: "Workspace" },
  { key: "timeline", label: "Timeline / events", href: "/ui/timeline", aux: true },
  { key: "compose", label: "Compose", href: "/ui/compose", aux: true },
];
export const navigate = { to: (href: string) => window.location.assign(href) };
export function GlobalNav({ current }: { current: NavKey }) {
  return (
    <>
      <div className="hk-brand">
        handoffkeep <small>Workspace</small>
      </div>
      <nav className="hk-side-nav" aria-label="Workspace navigation">
        {destinations.map((d) => (
          <a
            key={d.label}
            href={d.href}
            className={`hk-side-item ${d.aux ? "hk-side-aux" : ""}`}
            aria-current={d.key === current && !d.alias ? "page" : undefined}
          >
            {d.heading ? <small>{d.heading}</small> : null}
            <span>{d.label}</span>
          </a>
        ))}
      </nav>
    </>
  );
}
export function ThemeChoice() {
  const [theme, setTheme] = useState(loadTheme);
  useEffect(() => applyTheme(theme), [theme]);
  return (
    <button
      className="hk-btn"
      onClick={() => setTheme(theme === "dark" ? "light" : "dark")}
    >
      {" "}
      {theme === "dark" ? "Light" : "Dark"} theme
    </button>
  );
}
type Recent = { label: string; href: string };
function recentItems(): Recent[] {
  try {
    const value: unknown = JSON.parse(
      readStored(STORAGE_KEYS.recent) ?? "[]",
    );
    if (!Array.isArray(value)) return [];
    return value
      .filter(
        (item): item is Recent =>
          typeof item === "object" &&
          item !== null &&
          "href" in item &&
          typeof item.href === "string" &&
          item.href.startsWith("/ui/") &&
          "label" in item &&
          typeof item.label === "string",
      )
      .slice(0, 5);
  } catch {
    return [];
  }
}
function myWorkHref() {
  try {
    const scope = new URLSearchParams(
      readStored(STORAGE_KEYS.scope) ?? "",
    );
    scope.set("mine", "1");
    return `/ui/queue?${scope}`;
  } catch {
    return "/ui/queue?mine=1";
  }
}
function Palette({
  close,
  search = fetchTaskSearch,
}: {
  close: () => void;
  search?: SearchFn;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [query, setQuery] = useState("");
  const [results, setResults] = useState<{ label: string; href: string }[]>([]);
  const [error, setError] = useState(false);
  const [active, setActive] = useState(0);
  const [more, setMore] = useState(false);
  const [recent] = useState(recentItems);
  useEffect(() => {
    dialog.current?.showModal();
    return () => dialog.current?.close();
  }, []);
  useEffect(() => {
    let cancelled = false;
    // A newer query (or closing the palette) aborts the request in flight.
    const controller = new AbortController();
    setActive(0);
    setError(false);
    setMore(false);
    if (!query.trim()) {
      setResults([]);
      return;
    }
    const timer = window.setTimeout(() => {
      search(query, controller.signal).then(
        (response) => {
          if (!cancelled) {
            setMore(response.hasMore);
            setResults(
              response.results.filter(r => safeTaskId(r.id) !== null).map((r) => ({
                label: `#${r.id} ${r.title}`,
                href: `/ui/tasks/${r.id}`,
              })),
            );
          }
        },
        () => {
          if (!cancelled) {
            setResults([]);
            setError(true);
          }
        },
      );
    }, 180);
    return () => {
      cancelled = true;
      window.clearTimeout(timer);
      controller.abort();
    };
  }, [query, search]);
  const items = [
    ...destinations.filter(
      (d) => !query || d.label.toLowerCase().includes(query.toLowerCase()),
    ),
    ...(!query ? recent : results),
  ];
  return (
    <dialog
      ref={dialog}
      className="hk-palette"
      aria-label="Command palette"
      onCancel={(e) => {
        e.preventDefault();
        close();
      }}
      onClick={(e) => {
        if (e.target === dialog.current) close();
      }}
      onKeyDown={(e) => {
        if (e.key === "ArrowDown" || e.key === "ArrowUp") {
          e.preventDefault();
          setActive((a) =>
            Math.max(
              0,
              Math.min(items.length - 1, a + (e.key === "ArrowDown" ? 1 : -1)),
            ),
          );
        }
        if (e.key === "Enter" && items[active]) {
          e.preventDefault();
          navigate.to(items[active].href);
        }
        if (e.key === "Tab") {
          const focusable =
            dialog.current?.querySelectorAll<HTMLElement>("input,button,a");
          const first = focusable?.[0],
            last = focusable?.[focusable.length - 1];
          if (e.shiftKey && document.activeElement === first) {
            e.preventDefault();
            last?.focus();
          }
          if (!e.shiftKey && document.activeElement === last) {
            e.preventDefault();
            first?.focus();
          }
        }
      }}
    >
      <div className="hk-palette-head">
        <input
          autoFocus
          aria-label="Search commands and tasks"
          placeholder="Search commands and tasks…"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
        />
        <button onClick={close}>Esc</button>
      </div>
      <p>Navigation · task search · browse documents via Docs</p>
      {more ? <p>More task matches available · refine your search.</p> : null}
      {error ? <p role="alert">Task search unavailable. Try again.</p> : null}
      <div className="hk-palette-results">
        {items.map((item, i) => (
          <a
            key={item.href + item.label}
            className={i === active ? "active" : ""}
            href={item.href}
            ref={(el) => {
              if (i === active) el?.scrollIntoView?.({ block: "nearest" });
            }}
          >
            {item.label}
            <span>↵</span>
          </a>
        ))}
      </div>
    </dialog>
  );
}
export function PageShell({
  current,
  children,
  search,
}: {
  current: NavKey;
  children: ReactNode;
  search?: SearchFn;
}) {
  useEffect(() => {
    const href = location.pathname + location.search;
    const label = `Recent · ${location.pathname}`;
    writeStored(
      STORAGE_KEYS.recent,
      JSON.stringify(
        [
          { href, label },
          ...recentItems().filter((item) => item.href !== href),
        ].slice(0, 5),
      ),
    );
  }, []);
  const [palette, setPalette] = useState(false);
  const [help, setHelp] = useState(false);
  const [menu, setMenu] = useState(false);
  const trigger = useRef<HTMLElement | null>(null);
  const open = () => {
    trigger.current =
      document.activeElement instanceof HTMLElement
        ? document.activeElement
        : null;
    setPalette(true);
  };
  const close = () => {
    setPalette(false);
    trigger.current?.focus();
  };
  useEffect(() => {
    let prefixAt = 0;
    const key = (e: KeyboardEvent) => {
      if (e.isComposing) return;
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        if (palette) close();
        else open();
        return;
      }
      if (palette || isTypingTarget(e.target) || e.metaKey || e.ctrlKey || e.altKey)
        return;
      if (e.key === "Escape") {
        setHelp(false);
        setMenu(false);
        return;
      }
      if (e.key === "?") {
        setHelp((h) => !h);
        return;
      }
      if (prefixAt && Date.now() - prefixAt < 900) {
        prefixAt = 0;
        const href =
          e.key === "m"
            ? myWorkHref()
            : destinations.find((d) => d.shortcut === e.key)?.href;
        if (href) {
          e.preventDefault();
          navigate.to(href);
          return;
        }
      }
      if (e.key === "g") {
        prefixAt = Date.now();
        return;
      }
      if (["j", "k", "ArrowDown", "ArrowUp"].includes(e.key)) {
        const rows = Array.from(
          document.querySelectorAll<HTMLElement>(
            "[data-console-row],.timeline-event,.decision, .hk-host",
          ),
        ).filter((row) => !row.closest("[hidden]"));
        if (!rows.length) return;
        e.preventDefault();
        const index = rows.findIndex(
          (r) =>
            r === document.activeElement || r.contains(document.activeElement),
        );
        const next =
          rows[
            Math.max(
              0,
              Math.min(
                rows.length - 1,
                index + (e.key === "j" || e.key === "ArrowDown" ? 1 : -1),
              ),
            )
          ];
        if (next) {
          next.tabIndex = 0;
          next.focus();
          next.scrollIntoView?.({ block: "nearest" });
        }
      }
      if (
        e.key === "Enter" &&
        e.target instanceof HTMLElement &&
        e.target.matches("article")
      )
        e.target.querySelector<HTMLElement>("a,summary")?.click();
    };
    window.addEventListener("keydown", key);
    return () => window.removeEventListener("keydown", key);
  }, [palette]);
  return (
    <div className={`hk-page${menu ? " hk-menu-open" : ""}`}>
      <aside className="hk-side" aria-label="Console navigation">
        <GlobalNav current={current} />
        <div className="hk-side-foot">
          Operator workspace
          <br />
          <small>Same-origin authenticated console</small>
        </div>
      </aside>
      <main className="hk-page-main">
        <header className="hk-topbar">
          <button
            className="hk-mobile-menu"
            aria-label="Toggle navigation"
            aria-expanded={menu}
            onClick={() => setMenu(!menu)}
          >
            ☰
          </button>
          <span>
            Workspace <span className="muted">/</span>{" "}
            {destinations.find((d) => d.key === current)?.label}
          </span>
          <button onClick={open}>
            Search <kbd>⌘K</kbd>
          </button>
          <ThemeChoice />
        </header>
        {children}
      </main>
      {palette ? <Palette close={close} search={search} /> : null}
      {help ? (
        <div className="hk-help" role="region" aria-label="Keyboard shortcuts">
          <button onClick={() => setHelp(false)}>Close</button>
          <h2>Keyboard shortcuts</h2>
          <p>
            j / k or arrows: move · Enter: open · Esc: close · ⌘K / Ctrl+K:
            search
          </p>
          <p>
            g then i: Inbox · b: Board · m: My work · t: Tasks · d: Decisions ·
            e: Deploys · h: Fleet · s: Docs
          </p>
          <p>
            My work requires an explicit lane or claimant scope. Navigation
            never submits an answer.
          </p>
        </div>
      ) : null}
    </div>
  );
}
