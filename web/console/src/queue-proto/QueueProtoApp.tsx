import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { applyView, boardColumns, EMPTY_FILTERS, groupByArea, statusLine } from "./adapter";
import { flattenGrouped } from "./ListView";
import { buildDatasets } from "./fixtures";
import { DetailDrawer } from "./DetailDrawer";
import { ListView } from "./ListView";
import { BoardView } from "./BoardView";
import { Toolbar } from "./Toolbar";
import { MeasurePanel } from "./MeasurePanel";
import { loadPresentation, savePresentation, type SavedView } from "./storage";
import { runDiag } from "./diag";
import { runPerf } from "./perf";
import type { Dataset, ProtoState, ProtoView } from "./types";

const VIEW_LABELS: { view: ProtoView; label: string }[] = [
  { view: "operator", label: "Operator" },
  { view: "active", label: "Active" },
  { view: "backlog", label: "Backlog" },
  { view: "all", label: "All" },
];

export function defaultLayout(view: ProtoView): "list" | "board" {
  return view === "active" ? "board" : "list";
}

type AppProps = {
  datasets?: Record<string, Dataset>;
  initialSet?: string;
  storage?: Storage;
  diag?: boolean;
  perf?: boolean;
};

export function QueueProtoApp({ datasets, initialSet, storage, diag = false, perf = false }: AppProps) {
  const all = useMemo(() => datasets ?? buildDatasets(), [datasets]);
  const params = typeof window !== "undefined" ? new URLSearchParams(window.location.search) : new URLSearchParams();
  const setKey = initialSet ?? params.get("set") ?? (perf ? "perf5000" : "sample200");
  const dataset = all[setKey] ?? all.sample200;
  const diagMode = diag || params.get("diag") === "1";
  const perfMode = perf || params.get("perf") === "1";

  const loaded = useMemo(() => loadPresentation(storage), [storage]);
  const [state, setState] = useState<ProtoState>(() => {
    const s = { ...loaded.state };
    const pView = params.get("view");
    if (pView === "operator" || pView === "active" || pView === "backlog" || pView === "all") {
      s.view = pView;
      s.layout = defaultLayout(pView);
    }
    const pLayout = params.get("layout");
    if (pLayout === "list" || pLayout === "board") {
      s.layout = pLayout;
    }
    if (params.get("group") === "area") {
      s.grouping = "area";
    }
    return s;
  });
  const [views] = useState<Record<string, SavedView>>(loaded.views);
  const [openId, setOpenId] = useState<number | null>(null);
  const mainRef = useRef<HTMLDivElement>(null);
  const openerRef = useRef<HTMLElement | null>(null);
  const diagRef = useRef<HTMLPreElement>(null);
  const perfRef = useRef<HTMLPreElement>(null);

  const visible = useMemo(() => applyView(dataset.tasks, state), [dataset.tasks, state]);
  const groups = useMemo(
    () => (state.grouping === "area" ? groupByArea(visible, dataset.enrichment) : null),
    [state.grouping, visible, dataset.enrichment],
  );
  const columns = useMemo(
    () => (state.layout === "board" ? boardColumns(visible, state.view, state.hiddenColumns) : []),
    [state.layout, visible, state.view, state.hiddenColumns],
  );

  // Drawer prev/next follows the displayed order in the active layout.
  const orderedIds = useMemo(() => {
    if (state.layout === "board") {
      return columns.flatMap((col) => col.tasks.map((t) => t.id));
    }
    if (groups) {
      return flattenGrouped(groups, []).filter((r) => r.kind === "task").map((r) => (r.kind === "task" ? r.task.id : -1));
    }
    return visible.map((t) => t.id);
  }, [state.layout, columns, groups, visible]);

  const lanes = useMemo(() => [...new Set(dataset.tasks.map((t) => t.lane))].sort(), [dataset.tasks]);
  const kinds = useMemo(() => [...new Set(dataset.tasks.map((t) => t.kind))].sort(), [dataset.tasks]);

  // Persist presentation state only — never task bodies.
  useEffect(() => {
    savePresentation(state, views, storage);
  }, [state, views, storage]);

  // inert background + focus return to the originating row/card on close.
  useEffect(() => {
    const el = mainRef.current;
    if (!el) {
      return;
    }
    if (openId !== null) {
      el.setAttribute("inert", "");
    } else {
      el.removeAttribute("inert");
      openerRef.current?.focus();
    }
  }, [openId]);

  const open = useCallback((id: number, el: HTMLElement) => {
    openerRef.current = el;
    setOpenId(id);
  }, []);

  const close = useCallback(() => setOpenId(null), []);

  const setView = useCallback((view: ProtoView) => {
    setState((s) => ({ ...s, view, layout: defaultLayout(view) }));
  }, []);

  const applyNamedView = useCallback(
    (name: string) => {
      const saved = views[name];
      if (saved) {
        setState((s) => ({ ...s, ...saved, collapsedGroups: [] }));
      }
    },
    [views],
  );

  const toggleGroup = useCallback((key: string) => {
    setState((s) => ({
      ...s,
      collapsedGroups: s.collapsedGroups.includes(key) ? s.collapsedGroups.filter((k) => k !== key) : [...s.collapsedGroups, key],
    }));
  }, []);

  const showAll = useCallback(() => {
    setState((s) => ({ ...s, view: "all", filters: EMPTY_FILTERS }));
  }, []);

  const openTask = openId !== null ? dataset.tasks.find((t) => t.id === openId) ?? null : null;

  // diag mode: measure the real DOM into <pre id="diag"> during commit so
  // headless --dump-dom / CDP captures always see populated facts.
  useLayoutEffect(() => {
    if (!diagMode || !diagRef.current) {
      return;
    }
    const write = () => runDiag(diagRef.current!, dataset);
    write(); // synchronous: getBoundingClientRect forces real layout values
    window.addEventListener("resize", write);
    return () => window.removeEventListener("resize", write);
  }, [diagMode, dataset, state, openId]);

  // perf mode: warmup + 20 timed filter changes, JSON into <pre id="perf">.
  useLayoutEffect(() => {
    if (!perfMode || !perfRef.current) {
      return;
    }
    let cancelled = false;
    void runPerf((q) => setState((s) => ({ ...s, filters: { ...s.filters, query: q } })), dataset, (result) => {
      if (!cancelled && perfRef.current) {
        perfRef.current.textContent = JSON.stringify(result, null, 2);
      }
    });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [perfMode, dataset]);

  return (
    <div className="qp-root">
      <div id="qp-main" ref={mainRef}>
        <header className="qp-head">
          <span className="qp-ws">queue</span>
          <span className="qp-synth-badge">SYNTHETIC FIXTURE — not the production backlog</span>
          <nav className="qp-nav muted">timeline · decisions · fleet (prototype — links inert)</nav>
        </header>
        <div className="qp-viewbar" id="qp-viewbar">
          {VIEW_LABELS.map(({ view, label }) => (
            <button key={view} type="button" className={state.view === view ? "on" : ""} aria-pressed={state.view === view} onClick={() => setView(view)}>
              {label}
            </button>
          ))}
          <span className="qp-status muted" data-testid="status-line">
            {statusLine(dataset)}
          </span>
        </div>
        {loaded.versionMismatch ? (
          <p className="qp-reset-notice" role="alert">
            saved view reset — version mismatch
          </p>
        ) : null}
        <Toolbar state={state} lanes={lanes} kinds={kinds} views={views} onChange={setState} onApplyView={applyNamedView} />
        <p className="qp-count muted">
          {visible.length} unique tasks · {state.view} · {state.layout}
        </p>
        <div className="qp-content">
          {visible.length === 0 ? (
            <div className="qp-empty">
              <p>No tasks match the current view and filters.</p>
              <button type="button" onClick={showAll}>
                Show All view
              </button>
            </div>
          ) : state.layout === "list" ? (
            <ListView
              dataset={dataset}
              visible={visible}
              grouping={state.grouping}
              collapsedGroups={state.collapsedGroups}
              density={state.density}
              selectedId={openId}
              onOpen={open}
              onToggleGroup={toggleGroup}
            />
          ) : (
            <BoardView dataset={dataset} columns={columns} density={state.density} selectedId={openId} onOpen={open} />
          )}
        </div>
        <MeasurePanel />
      </div>
      {openTask ? <DetailDrawer dataset={dataset} task={openTask} orderedIds={orderedIds} onClose={close} onNav={setOpenId} /> : null}
      {diagMode ? <pre id="diag" ref={diagRef} /> : null}
      {perfMode ? <pre id="perf" ref={perfRef} /> : null}
    </div>
  );
}
