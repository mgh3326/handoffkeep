import type { FilterState, Grouping, Layout, ProtoState } from "./types";

type ToolbarProps = {
  state: ProtoState;
  lanes: string[];
  kinds: string[];
  /** State enumeration for the filter/column lists — the dataset's own
   * (server-provided when live), never a hardcoded superset. */
  states: string[];
  visibleCount: number;
  /** Rows came from a partial load, so the count is a lower bound. */
  partial: boolean;
  /** Offer the synthetic area→bundle draft grouping. Local preview only —
   * the live queue has no classification source for it. */
  allowAreaGrouping: boolean;
  onChange: (next: ProtoState) => void;
};

const GROUPING_LABEL: Record<Grouping, string> = {
  state: "상태별",
  none: "그룹 없음",
  area: "area→bundle (draft)",
};

/** Number of filters that narrow the row set — the "필터 n" count. */
export function activeFilterCount(filters: FilterState): number {
  return (
    (filters.lane !== "" ? 1 : 0) +
    (filters.kind !== "" ? 1 : 0) +
    filters.hiddenStates.length +
    (filters.minPriority !== null ? 1 : 0)
  );
}

/**
 * Toolbar: search, list/board, and one "필터 n" disclosure holding the
 * secondary controls (lane, kind, states, priority, grouping, board columns).
 * Applied filters also show as removable chips so a narrowed list is never
 * mistaken for the whole queue.
 */
export function Toolbar({ state, lanes, kinds, states, visibleCount, partial, allowAreaGrouping, onChange }: ToolbarProps) {
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
    chips.push({ label: "grouped: area→bundle (draft)", clear: () => set({ grouping: "state" }) });
  }

  const groupings: Grouping[] = allowAreaGrouping ? ["state", "none", "area"] : ["state", "none"];
  const filterCount = activeFilterCount(state.filters);
  const groupingNote = state.layout === "list" ? ` · ${GROUPING_LABEL[state.grouping]}` : "";

  return (
    <div className="qp-toolbar">
      <input
        id="qp-search"
        type="search"
        placeholder="이 목록에서 거르기 — 제목 · #id"
        title="이미 불러온 목록 안에서만 거릅니다. 닫힌 태스크까지 찾으려면 위의 전체 검색을 쓰세요."
        aria-label="search"
        aria-description="이 목록에서 거르기 — 불러온 행만"
        value={state.filters.query}
        onChange={(event) => setFilters({ query: event.target.value })}
      />
      <div id="qp-layout-toggle" className="qp-seg hk-seg" role="group" aria-label="보기">
        {(["list", "board"] as Layout[]).map((layout) => (
          <button
            key={layout}
            type="button"
            className={`hk-btn${state.layout === layout ? " on" : ""}`}
            aria-pressed={state.layout === layout}
            onClick={() => set({ layout })}
          >
            {layout}
          </button>
        ))}
      </div>
      <details className="qp-more qp-filter">
        <summary className="hk-btn">필터 {filterCount}</summary>
        <div className="qp-filter-panel">
          <label className="qp-field">
            <span>lane</span>
            <select aria-label="lane filter" value={state.filters.lane} onChange={(event) => setFilters({ lane: event.target.value })}>
              <option value="">lane: all</option>
              {lanes.map((lane) => (
                <option key={lane} value={lane}>
                  {lane}
                </option>
              ))}
            </select>
          </label>
          <label className="qp-field">
            <span>kind</span>
            <select aria-label="kind filter" value={state.filters.kind} onChange={(event) => setFilters({ kind: event.target.value })}>
              <option value="">kind: all</option>
              {kinds.map((kind) => (
                <option key={kind} value={kind}>
                  {kind}
                </option>
              ))}
            </select>
          </label>
          <label className="qp-field">
            <span>최소 priority</span>
            <input
              type="number"
              min={0}
              max={99}
              value={state.filters.minPriority ?? ""}
              onChange={(event) => setFilters({ minPriority: event.target.value === "" ? null : Number(event.target.value) })}
            />
          </label>
          <label className="qp-field">
            <span>그룹</span>
            <select
              id="qp-grouping"
              aria-label="grouping"
              value={state.grouping}
              onChange={(event) => set({ grouping: event.target.value as Grouping })}
            >
              {groupings.map((g) => (
                <option key={g} value={g}>
                  {GROUPING_LABEL[g]}
                </option>
              ))}
            </select>
          </label>
          <fieldset className="qp-states">
            <legend>표시할 상태</legend>
            {states.map((s) => (
              <label key={s}>
                <input type="checkbox" checked={!state.filters.hiddenStates.includes(s)} onChange={() => toggleHiddenState(s)} /> {s}
              </label>
            ))}
          </fieldset>
          {state.layout === "board" ? (
            <fieldset className="qp-states qp-columns">
              <legend>보드 열</legend>
              {states.map((s) => (
                <label key={s}>
                  <input type="checkbox" checked={!state.hiddenColumns.includes(s)} onChange={() => toggleHiddenColumn(s)} /> {s}
                </label>
              ))}
            </fieldset>
          ) : null}
        </div>
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
      <span className="qp-count">
        {partial ? `확인된 ${visibleCount}건 · 일부만 조회됨` : `${visibleCount}건`}
        {groupingNote} · priority 큰 값 먼저
      </span>
    </div>
  );
}
