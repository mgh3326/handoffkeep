import { statusLine } from "./adapter";
import type { SavedView } from "./storage";
import type { Dataset, ProtoView } from "./types";

export const VIEW_LABELS: { view: ProtoView; label: string }[] = [
  { view: "operator", label: "Operator" },
  { view: "active", label: "Active" },
  { view: "backlog", label: "Backlog" },
  { view: "all", label: "All" },
];

/** Builds the href a view link would navigate to in a full page load — the
 * click handler intercepts it for in-place state changes, so the href exists
 * for link semantics (copy/open-in-new-tab), not for navigation. */
function viewHref(view: ProtoView): string {
  const params = typeof window !== "undefined" ? new URLSearchParams(window.location.search) : new URLSearchParams();
  params.set("view", view);
  params.delete("layout");
  return `?${params.toString()}`;
}

type ViewRailProps = {
  dataset: Dataset;
  view: ProtoView;
  counts: Record<ProtoView, number>;
  views: Record<string, SavedView>;
  onSelectView: (view: ProtoView) => void;
  onApplyView: (name: string) => void;
};

/**
 * Left rail: semantic view navigator. Links carry real hrefs and
 * aria-current="page" on the selected view; these are navigational links,
 * not tabs. Counts equal the filtered row set of each view. The footer keeps
 * the dataset scope/completeness line and uses `N+` wording whenever the
 * fixture is not a complete set.
 */
export function ViewRail({ dataset, view, counts, views, onSelectView, onApplyView }: ViewRailProps) {
  const partial = dataset.completeness !== "complete";
  return (
    <aside className="qp-rail" id="qp-rail" aria-label="queue navigation">
      <nav className="qp-rail-nav" aria-label="queue views">
        <h2 className="qp-rail-h">views</h2>
        {VIEW_LABELS.map(({ view: v, label }) => (
          <a
            key={v}
            href={viewHref(v)}
            className={`qp-rail-link${view === v ? " on" : ""}`}
            aria-current={view === v ? "page" : undefined}
            onClick={(event) => {
              event.preventDefault();
              onSelectView(v);
            }}
          >
            <span className="qp-rail-label">{label}</span>
            <span className="qp-nav-count" aria-hidden="true">
              {counts[v]}
            </span>
          </a>
        ))}
      </nav>
      <nav className="qp-rail-nav" aria-label="saved views">
        <h2 className="qp-rail-h">saved views</h2>
        {Object.keys(views).map((name) => (
          <button key={name} type="button" className="qp-rail-link qp-rail-saved" onClick={() => onApplyView(name)}>
            <span className="qp-rail-label">{name}</span>
          </button>
        ))}
      </nav>
      <div className="qp-rail-foot muted">
        <p data-testid="status-line">{statusLine(dataset)}</p>
        <p>
          {dataset.tasks.length}
          {partial ? "+" : ""} fixture tasks{partial ? ` — ${dataset.completeness} (partial load; total may exceed the shown count)` : ""}
        </p>
      </div>
    </aside>
  );
}
