import { useCallback, useEffect, useMemo, useState } from "react";
import { fetchBoardTasks, fetchProjects } from "../shared/boardApi";
import { usePoll } from "../shared/usePoll";
import { readStored, STORAGE_KEYS, writeStored } from "../shared/storage";
import { CHIP_GROUP_ORDER, statusGroup } from "../shared/states";
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
import { TriageUi, useInboxTriage, type TriageRow } from "../shared/actions/InboxTriage";
import { fetchInboxStates, type InboxStateView } from "../shared/inboxApi";
import "./work.css";
const labels: Record<View, string> = { "needs-me": "내 차례", active: "진행 중", backlog: "대기", shipped: "완료", all: "전체" };
export function WorkPage() {
  const [overrides, setOverrides] = useState<Record<number, number>>({});
  const [search, setSearch] = useState(() => location.search);
  const [refresh, setRefresh] = useState(0);
  const [projects, setProjects] = useState<string[]>([]);
  const [projectsFailed, setProjectsFailed] = useState(false);
  const [wide, setWide] = useState(() => matchMedia("(min-width: 1024px)").matches);
  useEffect(() => { const pop = () => setSearch(location.search); window.addEventListener("popstate", pop); return () => window.removeEventListener("popstate", pop); }, []);
  useEffect(() => { const mq = matchMedia("(min-width: 1024px)"); const update = () => setWide(mq.matches); mq.addEventListener("change", update); return () => mq.removeEventListener("change", update); }, []);
  useEffect(() => { void fetchProjects().then(setProjects).catch(() => setProjectsFailed(true)); }, []);
  const params = useMemo(() => new URLSearchParams(search), [search]);
  const project = params.get("project") || readStored(STORAGE_KEYS.workProject) || projects[0] || "";
  const view: View = views.find(v => v === params.get("view")) || "needs-me";
  const query = params.get("q") || "", lane = params.get("lane") || "", kind = params.get("kind") || "", state = params.get("state") || "", priority = params.get("p") || "";
  const selected = params.get("task");
  const update = (key: string, value: string) => { const next = new URLSearchParams(search); if (value) next.set(key, value); else next.delete(key); history.pushState(null, "", `${location.pathname}?${next}`); setSearch(location.search); };
  useEffect(() => {
    if (!project) return;
    writeStored(STORAGE_KEYS.workProject, project);
    if (!params.has("project")) { const next = new URLSearchParams(search); next.set("project", project); history.replaceState(null, "", `${location.pathname}?${next}`); setSearch(location.search); }
  }, [project, params, search]);
  const load = useCallback(async () => ({ project, board: project ? await fetchBoardTasks(project) : null }), [project, refresh]);
  const { data, failed } = usePoll(load, 60000);
  const reload = useCallback(() => setRefresh(value => value + 1), []);
  const board = data?.project === project ? data.board : null;
  const tasks = (board?.tasks || []).map(t => ({ ...t, priority: overrides[t.id] ?? t.priority }));
  const filtered = tasks.filter(t => { const q = query.trim().toLowerCase(); return (!q || t.title.toLowerCase().includes(q) || String(t.id) === q.replace(/^#/, "")) && (!lane || t.lane === lane) && (!kind || t.kind === kind) && (!state || t.state === state) && (!priority || t.priority >= Number(priority)); });
  const choices = (key: "lane" | "kind" | "state") => [...new Set([...tasks.map(t => t[key]), params.get(key) || ""].filter(Boolean))].sort();
  const visible = filtered.filter(t => inView(t, view)).sort((a, b) => CHIP_GROUP_ORDER.indexOf(statusGroup(a)) - CHIP_GROUP_ORDER.indexOf(statusGroup(b)) || (a.priority || Number.POSITIVE_INFINITY) - (b.priority || Number.POSITIVE_INFINITY) || b.updated_at.localeCompare(a.updated_at) || b.id - a.id);
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
  }, [stateKeys]);
  const triageRows: TriageRow[] = visible.map(t => {
    const key = `task:${t.id}`;
    const state = inbox.get(key);
    return { key, read: state?.read ?? false, done: state?.done ?? false, woken: state?.woken ?? false, task: { id: t.id, state: t.state, priority: t.priority } };
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
  const filters = [["q", query], ["lane", lane], ["kind", kind], ["state", state], ["p", priority]] as const;
  return <div className="work-layout"><section className="work-list"><div className="work-heading"><h1>작업</h1>
    {projectsFailed && <p role="alert">불러오지 못함</p>}
    <label>프로젝트 <select value={project} onChange={e => update("project", e.target.value)}>{project && !projects.includes(project) && <option value={project}>{project}</option>}{projects.map(p => <option key={p}>{p}</option>)}</select></label></div>
    <TriageUi triage={triage} />
    <nav className="work-tabs" aria-label="작업 보기">{views.map(v => <button key={v} aria-pressed={view === v} onClick={() => update("view", v)}>{labels[v]} {filtered.filter(t => inView(t, v)).length}</button>)}</nav>
    <details className="work-filter-sheet" open={wide || undefined}><summary><Icon name="funnel" /> 필터</summary><div className="work-filters"><label>검색 <input value={query} onChange={e => update("q", e.target.value)} /></label>{(["lane", "kind", "state"] as const).map(k => <label key={k}>{{lane: "레인", kind: "종류", state: "상태"}[k]} <select value={params.get(k) || ""} onChange={e => update(k, e.target.value)}><option value="">전체</option>{choices(k).map(v => <option key={v} value={v}>{v}</option>)}</select></label>)}<label>우선순위 ≥ <input type="number" value={priority} onChange={e => update("p", e.target.value)} /></label></div></details>
    <div>{filters.filter(([, value]) => value).map(([key, value]) => <button key={key} onClick={() => update(key, "")} aria-label={`${{q: "검색", lane: "레인", kind: "종류", state: "상태", p: "우선순위"}[key]} 필터 해제`}>{{q: "검색", lane: "레인", kind: "종류", state: "상태", p: "우선순위"}[key]}: {value} 해제</button>)}</div>
    {failed && <p role="alert">불러오지 못함</p>}{!board && !failed && <p>{!project && projects.length === 0 ? "프로젝트 없음" : "Loading…"}</p>}{board?.truncated && <p>{tasks.length}+ (truncated)</p>}
    <div className="work-rows">{CHIP_GROUP_ORDER.map(group => {
      const rows = visible.filter(t => statusGroup(t) === group);
      if (!rows.length) return null;
      return <section key={group}><h2 className="work-group">{{decision: "결정 필요", input: "분류 대기", working: "진행 중", failed: "실패", done: "완료", neutral: "대기"}[group]} · {rows.length}</h2>{rows.map(t => {
        const job = t.active_job;
        return <div className="action-row" data-action-task={t.id} key={t.id}><a id={`work-row-${t.id}`} href={`/ui/tasks/${t.id}`} data-inbox-row data-inbox-key={`task:${t.id}`} data-selected={triage.selected.has(`task:${t.id}`) ? "true" : undefined} aria-current={selected === String(t.id) ? "true" : undefined} onKeyDown={e => { if (wide && e.key === "Enter") { e.preventDefault(); update("task", String(t.id)); } }} onClick={e => { if (wide && !e.metaKey && !e.ctrlKey && !e.shiftKey && !e.altKey) { e.preventDefault(); update("task", String(t.id)); } }}>
          <span className="work-row-title"><StateIcon state={t.state} /><Mono>#{t.id}</Mono> <span title={t.title}>{t.title}</span></span>
          <span className="work-row-meta"><Chip kind="status" state={t.state} awaiting_decision={t.awaiting_decision} />{job && <span className="hk-chip work-job" title={[job.role, job.model, job.profile, job.host || "기기 미기록", job.harness, job.effort].filter(Boolean).join(" · ")}>{[job.role, job.model, job.profile, job.host || "기기 미기록"].filter(Boolean).join(" · ")}</span>}<Chip kind="lane" lane={t.lane} /><Chip kind="priority" value={t.priority} urgent={t.priority === 1} /><time dateTime={t.updated_at} title={formatMinuteKST(t.updated_at) || t.updated_at}>{formatAge(board?.generatedAt || "", t.updated_at) || "시각 미상"}</time></span>
        </a><button type="button" className="inbox-select" aria-pressed={triage.selected.has(`task:${t.id}`)} aria-label={`${t.title} 선택`} onClick={() => triage.toggle(`task:${t.id}`)}>✓</button><TaskActions compact target={t} onPriority={value => setOverrides(old => ({ ...old, [t.id]: value }))} /></div>;
      })}</section>;
    })}</div>
  </section>{wide && selected && /^[1-9][0-9]*$/.test(selected) && <aside className="work-pane"><button onClick={() => update("task", "")}>Close</button><TaskRecord id={Number(selected)} /></aside>}</div>;
}
