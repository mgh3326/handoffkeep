import { statusLine } from "./adapter";
import { DensityChoice, GlobalNav, ThemeChoice } from "./AppShell";
import type { SavedView } from "./storage";
import type { Dataset, Density, ProtoView } from "./types";

export const VIEW_LABELS: { view: ProtoView; label: string }[] = [
  { view: "operator", label: "Operator" },
  { view: "live", label: "Live" },
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
  density: Density;
  onDensity: (density: Density) => void;
};

/**
 * Left sidebar: global destinations, then the semantic view navigator and
 * saved views. View links carry real hrefs and aria-current="page" on the
 * selected view; these are navigational links, not tabs. Counts equal the
 * filtered row set of each view. For a synthetic preview the footer keeps the
 * dataset scope/completeness line and uses `N+` wording whenever the fixture
 * is not a complete set.
 */
export function ViewRail({ dataset, view, counts, views, onSelectView, onApplyView, density, onDensity }: ViewRailProps) {
  const partial = dataset.completeness !== "complete";
  return (
    <aside className="qp-rail hk-side" id="qp-rail" aria-label="queue navigation">
      <GlobalNav current="queue" />
      <nav className="qp-rail-nav" aria-label="queue views">
        <h2 className="qp-rail-h">뷰</h2>
        {VIEW_LABELS.map(({ view: v, label }) => (
          <a
            key={v}
            href={viewHref(v)}
            className={`qp-rail-link hk-side-item${view === v ? " on" : ""}`}
            aria-current={view === v ? "page" : undefined}
            onClick={(event) => {
              // Modified (Cmd/Ctrl/Shift/Alt) and non-primary clicks keep
              // their native behaviour — open-in-new-tab/window, etc. Only an
              // unmodified primary click is intercepted for in-place state.
              if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey || event.button !== 0) {
                return;
              }
              event.preventDefault();
              onSelectView(v);
            }}
          >
            <span className="qp-rail-label">{label}</span>
            <span className="qp-nav-count hk-side-n" aria-hidden="true">
              {counts[v]}
            </span>
          </a>
        ))}
      </nav>
      <nav className="qp-rail-nav" aria-label="saved views">
        <h2 className="qp-rail-h">저장한 뷰</h2>
        {Object.keys(views).map((name) => (
          <button key={name} type="button" className="qp-rail-link hk-side-item qp-rail-saved" onClick={() => onApplyView(name)}>
            <span className="qp-rail-label">{name}</span>
          </button>
        ))}
      </nav>
      <div className="qp-rail-foot hk-side-foot">
        {dataset.source === "synthetic" ? (
          // The local preview keeps its provenance line: synthetic rows must
          // never pass for the real backlog. The live queue states its data
          // status in the page header instead of naming internal endpoints.
          <>
            <p data-testid="status-line">{statusLine(dataset)}</p>
            <p>
              {dataset.tasks.length}
              {partial ? "+" : ""} fixture tasks
              {partial ? ` — ${dataset.completeness} (partial load; total may exceed the shown count)` : ""}
            </p>
          </>
        ) : null}
        <div className="hk-prefs" role="group" aria-label="표시 설정">
          <span className="qp-rail-h">표시</span>
          <DensityChoice density={density} onChange={onDensity} />
          <ThemeChoice />
        </div>
      </div>
    </aside>
  );
}
