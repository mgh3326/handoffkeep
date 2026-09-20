import { KNOWN_STATES, type FilterState, type Layout, type ProtoState } from "./types";

type ToolbarProps = {
  state: ProtoState;
  lanes: string[];
  kinds: string[];
  visibleCount: number;
  onChange: (next: ProtoState) => void;
};

export function Toolbar({ state, lanes, kinds, visibleCount, onChange }: ToolbarProps) {
  const setFilters = (filters: Partial<FilterState>) => onChange({ ...state, filters: { ...state.filters, ...filters } });
  const set = (part: Partial<ProtoState>) => onChange({ ...state, ...part });

  const toggleHiddenState = (s: string) => {
    const hidden = state.filters.hiddenStates.includes(s)
      ? state.filters.hiddenStates.filter((x) => x !== s)
      : [...state.filters.hiddenStates, s];
    setFilters({ hiddenStates: hidden });
  };

  const toggleHiddenColumn = (s: string) => {
    const hidden = state.hiddenColumns.includes(s) ? state.hiddenColumns.filter((x) => x !== s) : [...state.hiddenColumns, s];
    set({ hiddenColumns: hidden });
  };

  const chips: { label: string; clear: () => void }[] = [];
  if (state.filters.lane !== "") {
    chips.push({ label: `lane: ${state.filters.lane}`, clear: () => setFilters({ lane: "" }) });
  }
  if (state.filters.kind !== "") {
    chips.push({ label: `kind: ${state.filters.kind}`, clear: () => setFilters({ kind: "" }) });
  }
  for (const s of state.filters.hiddenStates) {
    chips.push({ label: `state hidden: ${s}`, clear: () => toggleHiddenState(s) });
  }
  if (state.filters.minPriority !== null) {
    chips.push({ label: `p≥${state.filters.minPriority}`, clear: () => setFilters({ minPriority: null }) });
  }
  if (state.grouping === "area") {
    chips.push({ label: "grouped: area→bundle (draft)", clear: () => set({ grouping: "none" }) });
  }

  return (
    <div className="qp-toolbar">
      <input
        id="qp-search"
        type="search"
        placeholder="search title or #id"
        aria-label="search"
        value={state.filters.query}
        onChange={(event) => setFilters({ query: event.target.value })}
      />
      <div id="qp-layout-toggle" className="qp-seg" role="group" aria-label="layout">
        {(["list", "board"] as Layout[]).map((layout) => (
          <button
            key={layout}
            type="button"
            className={state.layout === layout ? "on" : ""}
            aria-pressed={state.layout === layout}
            onClick={() => set({ layout })}
          >
            {layout}
          </button>
        ))}
      </div>
      <button
        type="button"
        id="qp-group-toggle"
        className={state.grouping === "area" ? "on" : ""}
        aria-pressed={state.grouping === "area"}
        onClick={() => set({ grouping: state.grouping === "area" ? "none" : "area" })}
      >
        group: {state.grouping === "area" ? "area→bundle" : "off"}
      </button>
      <button
        type="button"
        id="qp-density"
        aria-pressed={state.density === "comfortable"}
        onClick={() => set({ density: state.density === "compact" ? "comfortable" : "compact" })}
      >
        density: {state.density}
      </button>
      <select aria-label="lane filter" value={state.filters.lane} onChange={(event) => setFilters({ lane: event.target.value })}>
        <option value="">lane: all</option>
        {lanes.map((lane) => (
          <option key={lane} value={lane}>
            {lane}
          </option>
        ))}
      </select>
      <select aria-label="kind filter" value={state.filters.kind} onChange={(event) => setFilters({ kind: event.target.value })}>
        <option value="">kind: all</option>
        {kinds.map((kind) => (
          <option key={kind} value={kind}>
            {kind}
          </option>
        ))}
      </select>
      <details className="qp-more">
        <summary>states</summary>
        <fieldset className="qp-states">
          {KNOWN_STATES.map((s) => (
            <label key={s}>
              <input type="checkbox" checked={!state.filters.hiddenStates.includes(s)} onChange={() => toggleHiddenState(s)} /> {s}
            </label>
          ))}
        </fieldset>
      </details>
      {state.layout === "board" ? (
        <details className="qp-more">
          <summary>columns</summary>
          <fieldset className="qp-states">
            {KNOWN_STATES.map((s) => (
              <label key={s}>
                <input type="checkbox" checked={!state.hiddenColumns.includes(s)} onChange={() => toggleHiddenColumn(s)} /> {s}
              </label>
            ))}
          </fieldset>
        </details>
      ) : null}
      <details className="qp-more">
        <summary>priority</summary>
        <label>
          min p{" "}
          <input
            type="number"
            min={0}
            max={99}
            value={state.filters.minPriority ?? ""}
            onChange={(event) => setFilters({ minPriority: event.target.value === "" ? null : Number(event.target.value) })}
          />
        </label>
      </details>
      {chips.length > 0 ? (
        <div className="qp-chips" aria-label="applied filters">
          {chips.map((chip) => (
            <button key={chip.label} type="button" className="qp-chip" onClick={chip.clear}>
              {chip.label} ✕
            </button>
          ))}
        </div>
      ) : null}
      <span className="qp-count muted">
        {visibleCount} unique tasks{state.grouping === "area" ? " · disjoint groups (no double counting)" : ""}
      </span>
    </div>
  );
}
