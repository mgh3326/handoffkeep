import { useEffect, useState } from "react";
import { fetchTaskDetail, fetchTaskComments, HttpError } from "../shared/boardApi";
import type { BoardComment, BoardDetail } from "../shared/boardTypes";
import { stateLabel } from "../shared/states";
import { formatMinuteKST } from "../shared/time";
import { TaskActions } from "../shared/actions/TaskActions";
import "./work.css";

function duration(start?: string | null, end?: string | null): string {
  if (!start) return "—";
  const ms = (end ? Date.parse(end) : Date.now()) - Date.parse(start);
  if (!Number.isFinite(ms)) return "—";
  const mins = Math.max(0, Math.floor(ms / 60000));
  return `${Math.floor(mins / 60)}h ${mins % 60}m`;
}
function Section({ title, truncated, children }: { title: string; truncated?: boolean; children: React.ReactNode }) {
  return <section className="work-section"><h2>{title} {truncated && <small>100+</small>}</h2>{children}</section>;
}
export function TaskRecord({ id }: { id: number }) {
  const [comments, setComments] = useState<BoardComment[]>([]);
  const [commentsError, setCommentsError] = useState(false);
  const [data, setData] = useState<BoardDetail | null>(null);
  const [error, setError] = useState<number | null>(null);
  const [attempt, setAttempt] = useState(0);
  useEffect(() => {
    let active = true;
    setData(null); setError(null); setComments([]); setCommentsError(false);
    void fetchTaskDetail(id).then(value => { if (active) { setData(value); void fetchTaskComments(id).then(result => { if (active) setComments(result.comments); }).catch(() => { if (active) setCommentsError(true); }); } }).catch((e: unknown) => { if (active) setError(e instanceof HttpError ? e.status : 500); });
    return () => { active = false; };
  }, [id, attempt]);
  if (error === 404) return <p>#{id} 없음</p>;
  if (error !== null) return <p role="alert">불러오지 못함 <button onClick={() => setAttempt(n => n + 1)}>다시 시도</button></p>;
  if (!data) return <p role="status">Loading…</p>;
  const { task, origins, documents, changes, jobs } = data;
  return <article className="work-record">
    <header data-action-task={task.id}><TaskActions target={task} onPriority={priority => setData(old => old ? { ...old, task: { ...old.task, priority } } : old)} /><h1>#{task.id} {task.title}</h1><p>{stateLabel(task.state)} · {task.project || "—"} · {task.lane} · P{task.priority} · {task.kind}</p></header>
    <Section title="Closure">{task.refs.closure ? <p>{task.refs.closure.type} · {task.refs.closure.evidence}</p> : ["merged", "deployed", "completed", "dropped", "duplicate", "canceled"].includes(task.state) ? <p>종결 증거 없음</p> : <p>기록 없음</p>}</Section>
    <Section title="Origins" truncated={data.origins_truncated}>{origins.length ? origins.map((o, i) => <p key={i}>{o.provider} · {o.namespace} · {o.url ? <a href={o.url}>{o.display_ref || o.url}</a> : o.display_ref || "—"}</p>) : <p>기록 없음</p>}</Section>
    <Section title="Documents" truncated={data.documents_truncated}>{documents.length ? documents.map(d => <p key={d.key}><a href={`/ui/doc/${d.key.split("/").map(encodeURIComponent).join("/")}`}>{d.key}</a> · {d.kind} · {d.origin_class || "미분류"}</p>) : <p>기록 없음</p>}</Section>
    <Section title="Changes" truncated={data.changes_truncated}>{changes.length ? changes.map(c => <div key={c.id}><p><a href={c.pr}>{c.pr}</a> · {c.state} · {c.head_sha?.slice(0, 7)} · {c.merged_at && formatMinuteKST(c.merged_at)} · {c.hold_reason} {c.depends_on_change_id && `depends on #${c.depends_on_change_id} ${c.depends_on_state || ""}`}</p>{c.reviews?.map(r => <p key={r.id}>{r.lens} · {r.round}/{r.pass} · {r.family}/{r.model} · {r.verdict} · c {r.critical ?? "?"} h {r.high ?? "?"} m {r.medium ?? "?"} l {r.low ?? "?"}</p>)}</div>) : <p>기록 없음</p>}</Section>
    <Section title="Jobs" truncated={data.jobs_truncated}>{jobs.length ? jobs.map(j => <div key={j.job_id}><p>{[j.job_id, j.role, j.model, j.harness, j.effort, j.profile, j.host].filter(Boolean).join(" · ")}</p><p>{j.skills_supported === false ? "skills not supported" : j.skills?.map(s => `${s.name} (${s.calls})`).join(", ") || "skills 없음"} · {j.end_reason} {j.diff_stat && `${j.diff_stat.files} files +${j.diff_stat.insertions} −${j.diff_stat.deletions}`}</p></div>) : <p>기록 없음</p>}</Section>
    <Section title="Durations"><p>Queue wait: {task.started_at ? duration(task.created_at, task.started_at) : "—"}</p><p>Work: {task.started_at ? `${task.completed_at || task.canceled_at ? "" : "진행 중 "}${duration(task.started_at, task.completed_at || task.canceled_at)}` : "—"}</p>{jobs.map(j => <p key={j.job_id}>{j.job_id}: spawned → ready {j.ready_at ? duration(j.spawned_at, j.ready_at) : "—"} · ready → ended {j.ended_at ? duration(j.ready_at, j.ended_at) : "—"}</p>)}{data.dwell.length ? data.dwell.map(d => <p key={d.state}>{stateLabel(d.state)}: {Math.floor(d.seconds / 60)}m</p>) : <p>상태 기록 없음</p>}</Section>
    <Section title="타임라인">{commentsError && <p role="alert">댓글을 불러오지 못했습니다.</p>}{[...data.events.map(e => ({ key: `e${e.id}`, at: e.at, actor: e.by, text: `${e.kind || "transition"} · ${e.from} → ${e.to} · ${e.note || ""}` })), ...comments.map(c => ({ key: `c${c.id}`, at: c.created_at, actor: c.author, text: c.body }))].sort((a,b) => Date.parse(a.at) - Date.parse(b.at)).map(item => <p key={item.key}>{formatMinuteKST(item.at)} · {item.actor} · {item.text}</p>)}</Section>
  </article>;
}
