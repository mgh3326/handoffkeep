// The detail's three fixed tabs — 개요 / 코멘트 / 전이 (hk:doc 2301 ①, 2410
// §3 AC 5). WAI-ARIA APG tabs with manual activation: arrows/Home/End move
// focus between tabs, Enter/Space (or click) selects. Every panel stays
// mounted and is hidden when inactive, so switching tabs never refetches.

import { useId, useRef, useState, type KeyboardEvent, type ReactNode } from "react";

export type TabKey = "overview" | "comments" | "transitions";

const TABS: { key: TabKey; label: string }[] = [
  { key: "overview", label: "개요" },
  { key: "comments", label: "코멘트" },
  { key: "transitions", label: "전이" },
];

export function ActivityTabs({ panels }: { panels: Record<TabKey, ReactNode> }) {
  const [active, setActive] = useState<TabKey>("overview");
  const [focused, setFocused] = useState<TabKey>("overview");
  const refs = useRef<Record<TabKey, HTMLButtonElement | null>>({ overview: null, comments: null, transitions: null });
  const base = useId();

  const moveFocus = (key: TabKey) => {
    setFocused(key);
    refs.current[key]?.focus();
  };

  const onKeyDown = (event: KeyboardEvent<HTMLButtonElement>, index: number) => {
    let next: number | null = null;
    if (event.key === "ArrowRight") {
      next = (index + 1) % TABS.length;
    } else if (event.key === "ArrowLeft") {
      next = (index + TABS.length - 1) % TABS.length;
    } else if (event.key === "Home") {
      next = 0;
    } else if (event.key === "End") {
      next = TABS.length - 1;
    } else if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      event.stopPropagation();
      setActive(TABS[index].key);
      return;
    }
    if (next !== null) {
      // Handled here: the panel host's own arrow keys (task prev/next) must
      // not also fire while the tab list has focus.
      event.preventDefault();
      event.stopPropagation();
      moveFocus(TABS[next].key);
    }
  };

  return (
    <div className="qp-tabs">
      <div role="tablist" aria-label="task detail" className="qp-tablist">
        {TABS.map((tab, index) => (
          <button
            key={tab.key}
            ref={(el) => {
              refs.current[tab.key] = el;
            }}
            type="button"
            role="tab"
            id={`${base}-tab-${tab.key}`}
            aria-selected={active === tab.key}
            aria-controls={`${base}-panel-${tab.key}`}
            tabIndex={focused === tab.key ? 0 : -1}
            className={active === tab.key ? "qp-tab qp-tab-active" : "qp-tab"}
            onClick={() => {
              setActive(tab.key);
              setFocused(tab.key);
            }}
            onKeyDown={(event) => onKeyDown(event, index)}
          >
            {tab.label}
          </button>
        ))}
      </div>
      {TABS.map((tab) => (
        <div
          key={tab.key}
          role="tabpanel"
          id={`${base}-panel-${tab.key}`}
          aria-labelledby={`${base}-tab-${tab.key}`}
          hidden={active !== tab.key}
          tabIndex={0}
          className="qp-tabpanel"
          data-tab={tab.key}
        >
          {panels[tab.key]}
        </div>
      ))}
    </div>
  );
}
