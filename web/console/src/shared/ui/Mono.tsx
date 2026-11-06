// Mono text: task ids, SHAs and digests. JetBrains Mono, tabular figures, and a
// long token wraps instead of widening the page (the .hk-mono class).

import type { ReactNode } from "react";

export function Mono({ children, className }: { children: ReactNode; className?: string }) {
  return <span className={className ? `hk-mono ${className}` : "hk-mono"}>{children}</span>;
}
