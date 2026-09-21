import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import type { BoardDetail } from "../board/types";
import { applyView, boardColumns, countByView, EMPTY_FILTERS, groupByArea } from "./adapter";
import { boardTaskToProto } from "./boardtask";
import { flattenGrouped } from "./ListView";
import { DetailDrawer, type DetailFetchState } from "./DetailDrawer";
import type { FetchDoc } from "./DocInline";
import { ListView } from "./ListView";
import { BoardView } from "./BoardView";
import { Toolbar } from "./Toolbar";
import { ViewRail } from "./ViewRail";
import { MeasurePanel } from "./MeasurePanel";
import { loadPresentation, savePresentation, type SavedView } from "./storage";
import { runDiag } from "./diag";
import { runPerf } from "./perf";
import type { Dataset, ProtoState, ProtoView } from "./types";

export function defaultLayout(view: ProtoView): "list" | "board" {
  return view === "active" ? "board" : "list";
}

function parseView(value: string | null): ProtoView | null {
  return value === "operator" || value === "active" || value === "backlog" || value === "all" ? value : null;
}

function applyNavParams(state: ProtoState, params: URLSearchParams): ProtoState {
  const next = { ...state };
  const v = parseView(params.get("view"));
  if (v) {
    next.view = v;
    next.layout = defaultLayout(v);
  }
  const l = params.get("layout");
  if (l === "list" || l === "board") {
    next.layout = l;
  }
  const g = params.get("group");
  if (g === "area" || g === "none") {
    next.grouping = g;
  }
  return next;
}

/** ?task=<id> deep link — the server shape-checks the value, so a value that
 * reaches the page is numeric or absent; a hand-crafted client-side value is
 * still discarded rather than trusted. */
function parseTaskParam(raw: string | null): number | null {
  if (raw === null || !/^[1-9][0-9]{0,14}$/.test(raw)) {
    return null;
  }
  const id = Number(raw);
  return Number.isSafeInteger(id) ? id : null;
}

/** The nav-relevant part of state — only view/layout/grouping/open-task enter
 * browser history, so Back steps through screens and open tasks, never
 * through filter keystrokes. */
function navKey(state: ProtoState, openId: number | null): string {
  return `${state.view}|${state.layout}|${state.grouping}|${openId ?? ""}`;
}

function navParams(state: ProtoState, openId: number | null): string {
  const params = new URLSearchParams(window.location.search);
  params.set("view", state.view);
  params.set("layout", state.layout);
  params.set("group", state.grouping);
  if (openId !== null) {
    params.set("task", String(openId));
  } else {
    params.delete("task");
  }
  return `?${params.toString()}`;
}

type AppProps = {
  /** Every dataset the rail can switch to. Required — the app never invents
   * rows; the fixture module stays unreachable from the production entry. */
  datasets: Record<string, Dataset>;
  initialSet?: string;
  storage?: Storage;
  diag?: boolean;
  perf?: boolean;
  /** Per-drawer detail loader (live mode). Absent → the drawer renders the
   * task's own fields, which is the fixture/test path. */
  fetchDetail?: (id: number) => Promise<BoardDetail>;
  /** Body document loader for the detail overview; defaults to the board BFF.
   * Called only for a task that carries body_doc. */
  fetchDoc?: FetchDoc;
};

export function QueueProtoApp({ datasets, initialSet, storage, diag = false, perf = false, fetchDetail, fetchDoc }: AppProps) {
  const all = datasets;
  const params = typeof window !== "undefined" ? new URLSearchParams(window.location.search) : new URLSearchParams();
  const setKey = initialSet ?? params.get("set") ?? (perf ? "perf5000" : "sample200");
  const dataset = all[setKey] ?? Object.values(all)[0];
  const diagMode = diag || params.get("diag") === "1";
  const perfMode = perf || params.get("perf") === "1";

  const loaded = useMemo(() => loadPresentation(storage), [storage]);
  const [state, setState] = useState<ProtoState>(() => ({
    ...applyNavParams(loaded.state, params),
    // Narrow viewports (incl. 200% zoom) start with the rail collapsed; the
    // header toggle always stays reachable.
    sidebarCollapsed: typeof window !== "undefined" ? window.innerWidth < 900 : false,
  }));
  const [views] = useState<Record<string, SavedView>>(loaded.views);
  const [openId, setOpenId] = useState<number | null>(() => parseTaskParam(params.get("task")));
  const [details, setDetails] = useState<Record<number, DetailFetchState>>({});
  const rootRef = useRef<HTMLDivElement>(null);
  const headRef = useRef<HTMLElement>(null);
  const mainRef = useRef<HTMLDivElement>(null);
  const openerRef = useRef<HTMLElement | null>(null);
  const diagRef = useRef<HTMLPreElement>(null);
  const perfRef = useRef<HTMLPreElement>(null);
  const navRef = useRef(navKey(state, openId));

  const visible = useMemo(() => applyView(dataset.tasks, state, dataset.states), [dataset.tasks, dataset.states, state]);
  const viewCounts = useMemo(() => countByView(dataset.tasks, state.filters, dataset.states), [dataset.tasks, dataset.states, state.filters]);
  const groups = useMemo(
    () => (state.grouping === "area" ? groupByArea(visible, dataset.enrichment, dataset.generatedAt) : null),
    [state.grouping, visible, dataset.enrichment, dataset.generatedAt],
  );
  const columns = useMemo(
    () => (state.layout === "board" ? boardColumns(visible, state.view, state.hiddenColumns, dataset.states) : []),
    [state.layout, visible, state.view, state.hiddenColumns, dataset.states],
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

  // Browser history: a nav-level change (view/layout/grouping/open task)
  // pushes one entry; Back restores it via popstate. Filter edits never push
  // entries. The entry URL is normalized on mount so every history entry
  // carries the full nav params.
  useEffect(() => {
    window.history.replaceState(null, "", navParams(state, openId));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    const key = navKey(state, openId);
    if (key !== navRef.current) {
      navRef.current = key;
      window.history.pushState(null, "", navParams(state, openId));
    }
  }, [state, openId]);

  useEffect(() => {
    const onPop = () => {
      const p = new URLSearchParams(window.location.search);
      const taskId = parseTaskParam(p.get("task"));
      setState((s) => {
        const next = applyNavParams(s, p);
        next.grouping = p.get("group") === "area" ? "area" : "none";
        navRef.current = navKey(next, taskId);
        return next;
      });
      setOpenId(taskId);
    };
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);

  // Non-modal panel: the list is never inert. On close, focus returns to the
  // originating row/card — but only when focus was inside the panel; focus
  // already back in the list (peek navigation) is left alone. A deep-linked
  // or unmounted opener has no row to return to, so focus lands on a stable
  // queue control instead of document.body.
  useEffect(() => {
    if (openId === null && !mainRef.current?.contains(document.activeElement)) {
      const opener = openerRef.current;
      (opener?.isConnected ? opener : document.getElementById("qp-rail-toggle"))?.focus();
    }
  }, [openId]);

  // Detail drawer lazy fetch: at most one call per open task id, only while
  // the drawer is open — the list never issues per-task requests. `details`
  // is deliberately not a dep: the effect runs only when openId/fetchDetail
  // changes, at which point the rendered `details` snapshot is current.
  // Detail fetches are never cancelled: closing or navigating the drawer lets
  // the request finish and cache, so reopening hits the cache instead of a
  // zombie "loading" entry. An "error" entry retries on the next open. A 404
  // is a resolved answer — the id does not exist — and is kept distinct from
  // a transient failure both for the panel's "not found" state and so it is
  // not retried on every render.
  useEffect(() => {
    if (openId === null || fetchDetail === undefined) {
      return;
    }
    const existing = details[openId];
    if (existing !== undefined && existing.status !== "error") {
      return;
    }
    setDetails((d) => ({ ...d, [openId]: { status: "loading" } }));
    fetchDetail(openId).then(
      (data) => {
        setDetails((d) => ({ ...d, [openId]: { status: "loaded", data } }));
      },
      (err) => {
        const status = (err as { status?: number } | null)?.status;
        setDetails((d) => ({ ...d, [openId]: status === 404 ? { status: "notfound" } : { status: "error" } }));
      },
    );
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [openId, fetchDetail]);

  const open = useCallback((id: number, el: HTMLElement) => {
    openerRef.current = el;
    setOpenId(id);
  }, []);

  const close = useCallback(() => setOpenId(null), []);

  // At ≤900px the rail is a fixed overlay; pinning its top edge to the
  // measured header height keeps #qp-rail-toggle — the only close control —
  // outside the overlay's hit area no matter how tall the header wraps.
  useLayoutEffect(() => {
    const root = rootRef.current;
    const head = headRef.current;
    if (!root || !head) {
      return;
    }
    const measure = () => root.style.setProperty("--qp-head-h", `${head.getBoundingClientRect().height}px`);
    measure();
    if (typeof ResizeObserver === "undefined") {
      return;
    }
    const observer = new ResizeObserver(measure);
    observer.observe(head);
    return () => observer.disconnect();
  }, []);

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

  const toggleRail = useCallback(() => {
    setState((s) => ({ ...s, sidebarCollapsed: !s.sidebarCollapsed }));
  }, []);

  const showAll = useCallback(() => {
    setState((s) => ({ ...s, view: "all", filters: EMPTY_FILTERS }));
  }, []);

  // The open task resolves from the list dataset first; a deep link to a task
  // outside the current filter — or outside a truncated dataset — falls back
  // to the single-task detail fetch. Absent from the list must never mean
  // "not found".
  const openDetail = openId !== null ? details[openId] : undefined;
  const openTask =
    openId !== null
      ? (dataset.tasks.find((t) => t.id === openId) ??
        (openDetail?.status === "loaded" ? boardTaskToProto(openDetail.data.task) : null))
      : null;
  const openNotFound =
    openId !== null && openTask === null && (fetchDetail === undefined || openDetail?.status === "notfound");

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
    <div className={`qp-root${state.sidebarCollapsed ? " rail-collapsed" : ""}${openId !== null ? " qp-peek-open" : ""}`} ref={rootRef}>
      <div id="qp-main" ref={mainRef}>
        <ViewRail dataset={dataset} view={state.view} counts={viewCounts} views={views} onSelectView={setView} onApplyView={applyNamedView} />
        <div className="qp-body">
          <header className="qp-head" ref={headRef}>
            <button
              type="button"
              id="qp-rail-toggle"
              aria-expanded={!state.sidebarCollapsed}
              aria-controls="qp-rail"
              onClick={toggleRail}
            >
              ☰ views
            </button>
            <span className="qp-ws">queue</span>
            {dataset.source === "synthetic" ? (
              <span className="qp-synth-badge">SYNTHETIC FIXTURE — not the production backlog</span>
            ) : (
              <span className="qp-live-badge">LIVE — /ui/api/board</span>
            )}
            <nav className="qp-nav muted">
              {dataset.source === "synthetic" ? "timeline · decisions · fleet (prototype — links inert)" : "timeline · decisions · fleet"}
            </nav>
          </header>
          {loaded.versionMismatch ? (
            <p className="qp-reset-notice" role="alert">
              saved view reset — version mismatch
            </p>
          ) : null}
          <Toolbar state={state} lanes={lanes} kinds={kinds} states={dataset.states} visibleCount={visible.length} onChange={setState} />
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
      </div>
      {openId !== null ? (
        <DetailDrawer
          dataset={dataset}
          taskId={openId}
          task={openTask}
          detail={fetchDetail ? (openDetail ?? { status: "loading" }) : undefined}
          notFound={openNotFound}
          orderedIds={orderedIds}
          onClose={close}
          onNav={setOpenId}
          fetchDoc={fetchDoc}
        />
      ) : null}
      {diagMode ? <pre id="diag" ref={diagRef} /> : null}
      {perfMode ? <pre id="perf" ref={perfRef} /> : null}
    </div>
  );
}
