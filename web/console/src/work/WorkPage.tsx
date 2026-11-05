import { useCallback, useEffect, useMemo, useState } from "react";
import { fetchBoardTasks, fetchProjects } from "../shared/boardApi";
import { usePoll } from "../shared/usePoll";
import { readStored, STORAGE_KEYS, writeStored } from "../shared/storage";
import { stateLabel } from "../shared/states";
import { inView, views, type View } from "./views";
import { TaskRecord } from "./TaskRecord";
import "./work.css";
const labels: Record<View, string> = { "needs-me": "Needs me", active: "Active", backlog: "Backlog", shipped: "Shipped", all: "All" };
export function WorkPage() {
  const [search, setSearch] = useState(() => location.search);
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
  const load = useCallback(async () => ({ project, board: project ? await fetchBoardTasks(project) : null }), [project]);
  const { data, failed } = usePoll(load, 60000);
  const board = data?.project === project ? data.board : null;
  const tasks = board?.tasks || [];
  const filtered = tasks.filter(t => { const q = query.trim().toLowerCase(); return (!q || t.title.toLowerCase().includes(q) || String(t.id) === q.replace(/^#/, "")) && (!lane || t.lane === lane) && (!kind || t.kind === kind) && (!state || t.state === state) && (!priority || t.priority >= Number(priority)); });
  const choices = (key: "lane" | "kind" | "state") => [...new Set(tasks.map(t => t[key]))].sort();
  const filters = [["q", query], ["lane", lane], ["kind", kind], ["state", state], ["p", priority]] as const;
  return <div className="work-layout"><section className="work-list"><h1>Work</h1>
    {projectsFailed && <p role="alert">불러오지 못함</p>}
    <label>Project <select value={project} onChange={e => update("project", e.target.value)}>{projects.map(p => <option key={p}>{p}</option>)}</select></label>
    <nav className="work-tabs" aria-label="Views">{views.map(v => <button key={v} aria-pressed={view === v} onClick={() => update("view", v)}>{labels[v]} {filtered.filter(t => inView(t, v)).length}</button>)}</nav>
    <div className="work-filters"><label>Search <input value={query} onChange={e => update("q", e.target.value)} /></label>{(["lane", "kind", "state"] as const).map(k => <label key={k}>{k} <select value={params.get(k) || ""} onChange={e => update(k, e.target.value)}><option value="">All</option>{choices(k).map(v => <option key={v} value={v}>{v}</option>)}</select></label>)}<label>Priority ≥ <input type="number" value={priority} onChange={e => update("p", e.target.value)} /></label></div>
    <div>{filters.filter(([, value]) => value).map(([key, value]) => <button key={key} onClick={() => update(key, "")} aria-label={`Remove ${key} filter`}>{key}: {value} ×</button>)}</div>
    {failed && <p role="alert">불러오지 못함</p>}{!board && !failed && <p>{!project && projects.length === 0 ? "프로젝트 없음" : "Loading…"}</p>}{board?.truncated && <p>{tasks.length}+ (truncated)</p>}
    <div className="work-rows">{filtered.filter(t => inView(t, view)).map(t => <a key={t.id} href={`/ui/tasks/${t.id}`} aria-current={selected === String(t.id) ? "true" : undefined} onClick={e => { if (wide && !e.metaKey && !e.ctrlKey && !e.shiftKey && !e.altKey) { e.preventDefault(); update("task", String(t.id)); } }}><span>#{t.id} {t.title}</span><span>{stateLabel(t.state)} · {t.lane} · P{t.priority}</span></a>)}</div>
  </section>{wide && selected && /^[1-9][0-9]*$/.test(selected) && <aside className="work-pane"><button onClick={() => update("task", "")}>Close</button><TaskRecord id={Number(selected)} /></aside>}</div>;
}
