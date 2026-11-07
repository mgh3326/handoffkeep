import { useCallback, useEffect, useMemo, useState } from "react";
import { fetchBoardTasks, fetchProjectIndex, type ProjectOwner } from "../shared/boardApi";
import { usePoll } from "../shared/usePoll";
import { readStored, STORAGE_KEYS, writeStored } from "../shared/storage";
import { CHIP_GROUP_ORDER, stateLabel, statusGroup } from "../shared/states";
import { Icon } from "../shared/Icon";
import { StateIcon } from "../shared/StateIcon";
import { Chip } from "../shared/ui/Chip";
import { Mono } from "../shared/ui/Mono";
import { isTypingTarget } from "../shared/search";
import { formatMinuteKST } from "../shared/time";
import { formatAge } from "../decisions/format";
import { inView, views, type View } from "./views";
import { TaskRecord } from "./TaskRecord";
import { TaskActions } from "../shared/actions/TaskActions";
import { TranslationIndicator, translationAction, useShowOriginal } from "../shared/translation";
import { TriageUi, useInboxTriage, type TriageRow } from "../shared/actions/InboxTriage";
import { fetchInboxStates, type InboxStateView } from "../shared/inboxApi";
import { QueryBar } from "../shared/query/QueryBar";
import { formatQuery, parseQuery, sortTasks, taskMatchesQuery, type QueryContext, type QueryFilters, type QuerySort } from "../shared/query/query";
import { SavedSearchChips, SaveSearch } from "../shared/SavedSearchList";
import { readSavedSearches, writeSavedSearches, type SavedSearch } from "../shared/savedSearches";
import "./work.css";
const labels: Record<View, string> = { "needs-me": "내 차례", active: "진행 중", backlog: "대기", shipped: "완료", all: "전체" };
const sortValues: QuerySort[] = ["default", "priority", "updated", "id", "state"];
const sortLabels: Record<QuerySort, string> = { default: "기본", priority: "우선순위", updated: "최근", id: "번호", state: "상태" };
// The value a filter chip shows: states and sorts by their Korean names, the
// same words the query bar uses.
function filterValueLabel(key: string, value: string): string {
  if (key === "state") return stateLabel(value);
  if (key === "sort") return sortLabels[value as QuerySort];
  if (key === "p" && value === "0") return "없음";
  return value;
}
export function WorkPage() {
  const showOriginal = useShowOriginal();
  const [originalItems, setOriginalItems] = useState<Record<number, boolean>>({});
  useEffect(() => setOriginalItems({}), [showOriginal]);
  const [overrides, setOverrides] = useState<Record<number, number>>({});
  const [search, setSearch] = useState(() => location.search);
  const [refresh, setRefresh] = useState(0);
  const [projects, setProjects] = useState<string[]>([]);
  const [owners, setOwners] = useState<ProjectOwner[]>([]);
  const [projectsFailed, setProjectsFailed] = useState(false);
  const [wide, setWide] = useState(() => matchMedia("(min-width: 1024px)").matches);
  const [queryDraft, setQueryDraft] = useState<string | null>(null);
  const [saved, setSaved] = useState<SavedSearch[]>(() => readSavedSearches());
  useEffect(() => { const pop = () => setSearch(location.search); window.addEventListener("popstate", pop); return () => window.removeEventListener("popstate", pop); }, []);
  useEffect(() => { const mq = matchMedia("(min-width: 1024px)"); const update = () => setWide(mq.matches); mq.addEventListener("change", update); return () => mq.removeEventListener("change", update); }, []);
  useEffect(() => { void fetchProjectIndex().then(index => { setProjects(index.projects); setOwners(index.owners); }).catch(() => setProjectsFailed(true)); }, []);
  const params = useMemo(() => new URLSearchParams(search), [search]);
  const project = params.get("project") || readStored(STORAGE_KEYS.workProject) || projects[0] || "";
  // The design session that owns the selected project. effective_owner already
  // carries the operator-desk fallback; "(없음 → operator-desk)" marks the
  // project nobody has claimed, since design_lane is then empty.
  const projectOwner = owners.find(o => o.name === project);
  const ownerLabel = projectOwner ? (projectOwner.design_lane ? projectOwner.effective_owner : `(없음 → ${projectOwner.effective_owner})`) : "";
  const view: View = views.find(v => v === params.get("view")) || "needs-me";
  const query = params.get("q") || "", lane = params.get("lane") || "", kind = params.get("kind") || "", state = params.get("state") || "", priority = params.get("p") || "";
  const grade = params.get("grade") || "", tier = params.get("tier") || "";
  const rawSort = params.get("sort") || "default";
  const sort: QuerySort = sortValues.includes(rawSort as QuerySort) ? rawSort as QuerySort : "default";
  const selected = params.get("task");
  const update = (key: string, value: string) => { setQueryDraft(null); const next = new URLSearchParams(search); if (value) next.set(key, value); else next.delete(key); history.pushState(null, "", `${location.pathname}?${next}`); setSearch(location.search); };
  const queryFilters: QueryFilters = {
    text: query, state, lane, project, kind, priority, grade, tier, sort,
    view: view === "needs-me" ? "needs-me" : view === "active" ? "active" : "",
  };
  const canonicalQuery = formatQuery(queryFilters);
  const shownQuery = queryDraft ?? canonicalQuery;
  const queryErrors = queryDraft === null ? [] : parseQuery(queryDraft).errors;
  const applyQuery = (filters: QueryFilters) => {
    const next = new URLSearchParams(search);
    const put = (key: string, value: string) => { if (value) next.set(key, value); else next.delete(key); };
    put("q", filters.text);
    put("lane", filters.lane);
    // The board is fetched per project and the selector always has one; a
    // query that omits the field keeps the current project instead of
    // dropping to the stored default.
    if (filters.project) put("project", filters.project);
    put("kind", filters.kind);
    put("state", filters.state);
    put("p", filters.priority);
    put("grade", filters.grade);
    put("tier", filters.tier);
    put("sort", filters.sort === "default" ? "" : filters.sort);
    // A query without a shortcut leaves the current view alone: the tabs are
    // still the view control.
    if (filters.view) next.set("view", filters.view);
    history.replaceState(null, "", `${location.pathname}?${next}`);
    setSearch(location.search);
  };
  const changeQuery = (text: string) => {
    setQueryDraft(text);
    const parsed = parseQuery(text);
    if (parsed.errors.length === 0) applyQuery(parsed.filters);
  };
  const commitQuery = (text: string) => {
    const parsed = parseQuery(text);
    if (parsed.errors.length === 0) {
      applyQuery(parsed.filters);
      setQueryDraft(null);
    }
  };
  const applySavedQuery = (text: string) => {
    setQueryDraft(text);
    const parsed = parseQuery(text);
    if (parsed.errors.length === 0) applyQuery(parsed.filters);
  };
  const changeSaved = (list: SavedSearch[]) => { setSaved(list); writeSavedSearches(list); };
  useEffect(() => {
    if (!project) return;
    writeStored(STORAGE_KEYS.workProject, project);
    if (!params.has("project")) { const next = new URLSearchParams(search); next.set("project", project); history.replaceState(null, "", `${location.pathname}?${next}`); setSearch(location.search); }
  }, [project, params, search]);
  useEffect(() => { const onChange = () => { setOverrides({}); setRefresh(n => n + 1); }; window.addEventListener("hk-task-changed", onChange); return () => window.removeEventListener("hk-task-changed", onChange); }, []);
  const load = useCallback(async () => ({ project, board: project ? await fetchBoardTasks(project) : null }), [project, refresh]);
  const { data, failed } = usePoll(load, 60000);
  const reload = useCallback(() => setRefresh(value => value + 1), []);
  const board = data?.project === project ? data.board : null;
  const tasks = (board?.tasks || []).map(t => ({ ...t, priority: overrides[t.id] ?? t.priority }));
  const filtered = tasks.filter(t => taskMatchesQuery(t, queryFilters));
  const choices = (key: "lane" | "kind" | "state") => [...new Set([...tasks.map(t => t[key]), params.get(key) || ""].filter(Boolean))].sort();
  const gradeChoices = [...new Set([...tasks.map(t => t.refs.linear?.grade || ""), grade].filter(Boolean))].sort();
  const tierChoices = [...new Set([...tasks.map(t => t.refs.linear?.tier || ""), tier].filter(Boolean))].sort();
  const queryContext: QueryContext = {
    lanes: [...new Set(tasks.map(t => t.lane))].sort(),
    projects,
    kinds: [...new Set(tasks.map(t => t.kind))].sort(),
    grades: gradeChoices,
    tiers: tierChoices,
  };
  const visible = sortTasks(filtered.filter(t => inView(t, view)), sort);
  // The triage rows follow the rendered order, so j/k walks the list the page
  // shows. A task row's inbox key is task:<id>, the same item Home triages.
  // The stored state is one bounded read for the visible rows (the route caps
  // at 200); a row past the cap shows unread until the filters narrow it.
  const stateKeys = visible.slice(0, 200).map(t => `task:${t.id}`).join(",");
  const [inbox, setInbox] = useState<Map<string, InboxStateView>>(() => new Map());
  useEffect(() => {
    if (stateKeys === "") {
      setInbox(new Map());
      return;
    }
    let active = true;
    void fetchInboxStates(stateKeys.split(",")).then(states => { if (active) setInbox(states); }).catch(() => { if (active) setInbox(new Map()); });
    return () => { active = false; };
    // A triage write bumps refresh and every poll moves generatedAt; both
    // must refetch the stored state, or the rows would keep the pre-write
    // read/done marks.
  }, [stateKeys, board?.generatedAt, refresh]);
  const triageRows: TriageRow[] = visible.map(t => {
    const key = `task:${t.id}`;
    const state = inbox.get(key);
    return { key, read: state?.read ?? false, done: state?.done ?? false, woken: state?.woken ?? false, task: { id: t.id, state: t.state, priority: t.priority, kind: t.kind, lane: t.lane, title: t.title, awaiting_decision: t.awaiting_decision, refs: t.refs } };
  });
  const triage = useInboxTriage(triageRows, reload);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.isComposing || e.metaKey || e.ctrlKey || e.altKey || e.shiftKey || isTypingTarget(e.target) || !(e.target instanceof Element && e.target.closest(".work-rows a")) || !["ArrowUp", "ArrowDown"].includes(e.key) || !visible.length) return;
      e.preventDefault();
      const row = e.target.closest(".work-rows a");
      const current = visible.findIndex(t => document.getElementById(`work-row-${t.id}`) === row);
      const index = current < 0 ? 0 : Math.max(0, Math.min(visible.length - 1, current + (e.key === "ArrowDown" ? 1 : -1)));
      document.getElementById(`work-row-${visible[index].id}`)?.focus();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  });
  const activeFilters: [string, string, string][] = [
    ["q", query, "검색"],
    ["lane", lane, "레인"],
    ["kind", kind, "종류"],
    ["state", state, "상태"],
    ["p", priority, "우선순위"],
    ["grade", grade, "등급"],
    ["tier", tier, "티어"],
    ["sort", sort === "default" ? "" : sort, "정렬"],
  ];
  return <div className="work-layout"><section className="work-list"><div className="work-heading"><h1>작업</h1>
    {projectsFailed && <p role="alert">불러오지 못함</p>}
    <label>프로젝트 <select value={project} onChange={e => update("project", e.target.value)}>{project && !projects.includes(project) && <option value={project}>{project}</option>}{projects.map(p => <option key={p}>{p}</option>)}</select></label>
    {ownerLabel && <span className="work-owner">담당 설계 세션 <Mono>{ownerLabel}</Mono></span>}</div>
    <QueryBar value={shownQuery} onChange={changeQuery} onCommit={commitQuery} errors={queryErrors} context={queryContext} actions={<SaveSearch searches={saved} currentQuery={canonicalQuery} onChange={changeSaved} />} />
    <TriageUi triage={triage} />
    <nav className="work-tabs" aria-label="작업 보기">{views.map(v => <button key={v} aria-pressed={view === v} onClick={() => update("view", v)}>{labels[v]} {filtered.filter(t => inView(t, v)).length}</button>)}</nav>
    <SavedSearchChips searches={saved} onApply={applySavedQuery} onChange={changeSaved} />
    <details className="work-filter-sheet" open={wide || undefined}><summary><Icon name="funnel" /> 필터</summary><div className="work-filters">{(["lane", "kind", "state"] as const).map(k => <label key={k}>{{lane: "레인", kind: "종류", state: "상태"}[k]} <select value={params.get(k) || ""} onChange={e => update(k, e.target.value)}><option value="">전체</option>{choices(k).map(v => <option key={v} value={v}>{k === "state" ? stateLabel(v) : v}</option>)}</select></label>)}<label>우선순위 ≥ <input type="number" value={priority} onChange={e => update("p", e.target.value)} /></label><label>등급 <select value={grade} onChange={e => update("grade", e.target.value)}><option value="">전체</option>{gradeChoices.map(v => <option key={v} value={v}>{v}</option>)}</select></label><label>티어 <select value={tier} onChange={e => update("tier", e.target.value)}><option value="">전체</option>{tierChoices.map(v => <option key={v} value={v}>{v}</option>)}</select></label><label>정렬 <select value={sort} onChange={e => update("sort", e.target.value === "default" ? "" : e.target.value)}>{sortValues.map(v => <option key={v} value={v}>{sortLabels[v]}</option>)}</select></label></div></details>
    <div>{activeFilters.filter(([, value]) => value).map(([key, value, label]) => <button key={key} onClick={() => update(key, "")} aria-label={`${label} 필터 해제`}>{label}: {filterValueLabel(key, value)} 해제</button>)}</div>
    {failed && <p role="alert">불러오지 못함</p>}{!board && !failed && <p>{!project && projects.length === 0 ? "프로젝트 없음" : "Loading…"}</p>}{board?.truncated && <p>{tasks.length}+ (truncated)</p>}
    <div className="work-rows">{CHIP_GROUP_ORDER.map(group => {
      const rows = visible.filter(t => statusGroup(t) === group);
      if (!rows.length) return null;
      return <section key={group}><h2 className="work-group">{{decision: "결정 필요", input: "분류 대기", working: "진행 중", failed: "실패", done: "완료", neutral: "대기"}[group]} · {rows.length}</h2>{rows.map(t => {
        const job = t.active_job;
        return <div className="work-translation-row action-row" data-action-task={t.id} key={t.id}><a id={`work-row-${t.id}`} href={`/ui/tasks/${t.id}`} data-inbox-row data-inbox-key={`task:${t.id}`} data-selected={triage.selected.has(`task:${t.id}`) ? "true" : undefined} aria-current={selected === String(t.id) ? "true" : undefined} onKeyDown={e => { if (wide && e.key === "Enter") { e.preventDefault(); update("task", String(t.id)); } }} onClick={e => { if (wide && !e.metaKey && !e.ctrlKey && !e.shiftKey && !e.altKey) { e.preventDefault(); update("task", String(t.id)); } }}>
          <span className="work-row-title"><StateIcon state={t.state} /><Mono>#{t.id}</Mono> <span title={t.title}>{translationAction.applicable(t.title_ko, t.title_ko_stale) ? t.title_ko : t.title}{t.title_ko_stale && <small className="hk-translation-stale"> 갱신 필요</small>}</span></span>
          <span className="work-row-meta"><Chip kind="status" state={t.state} awaiting_decision={t.awaiting_decision} />{job && <span className="hk-chip work-job" title={[job.role, job.model, job.profile, job.host || "기기 미기록", job.harness, job.effort].filter(Boolean).join(" · ")}>{[job.role, job.model, job.profile, job.host || "기기 미기록"].filter(Boolean).join(" · ")}</span>}<Chip kind="lane" lane={t.lane} /><Chip kind="priority" value={t.priority} urgent={t.priority === 1} /><time dateTime={t.updated_at} title={formatMinuteKST(t.updated_at) || t.updated_at}>{formatAge(board?.generatedAt || "", t.updated_at) || "시각 미상"}</time></span>
        </a><button type="button" className="inbox-select" aria-pressed={triage.selected.has(`task:${t.id}`)} aria-label={`${t.title} 선택`} onClick={() => triage.toggle(`task:${t.id}`)}>✓</button><TaskActions compact target={t} onPriority={value => setOverrides(old => ({ ...old, [t.id]: value }))} />{translationAction.applicable(t.title_ko, t.title_ko_stale) && <><TranslationIndicator expanded={originalItems[t.id] ?? showOriginal} onChange={expanded => setOriginalItems(items => ({...items, [t.id]: expanded}))} />{(originalItems[t.id] ?? showOriginal) && <span className="hk-original-text">{t.title}</span>}</>}</div>;
      })}</section>;
    })}</div>
  </section>{wide && selected && /^[1-9][0-9]*$/.test(selected) && <aside className="work-pane"><button onClick={() => update("task", "")}>Close</button><TaskRecord id={Number(selected)} /></aside>}</div>;
}
