// A warning that stays one line until the operator opens it: a native
// <details>, collapsed by default, so a long list never pushes the page down.
// The summary names what is hidden ("N개 … · 펼치기"); the warning icon and the
// words carry the meaning, never color alone.

import { useState, type ReactNode } from "react";
import { Icon } from "../Icon";
import "./CollapsedWarning.css";

export function CollapsedWarning({ title, children, testId }: { title: string; children: ReactNode; testId?: string }) {
  const [open, setOpen] = useState(false);
  return (
    <details className="hk-collapsed-warning" data-testid={testId} onToggle={(e) => setOpen(e.currentTarget.open)}>
      <summary>
        <Icon name="warning" />
        <span>
          {title} · {open ? "접기" : "펼치기"}
        </span>
        <Icon name="caret-down" className="hk-collapsed-warning-caret" />
      </summary>
      <div className="hk-collapsed-warning-body">{children}</div>
    </details>
  );
}
