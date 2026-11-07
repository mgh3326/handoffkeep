import { useCallback, useEffect, useRef, useState } from "react";
import { fetchTaskDetail, fetchTaskComments, HttpError } from "../shared/boardApi";
import { usePoll } from "../shared/usePoll";
import type { BoardComment, BoardDetail } from "../shared/boardTypes";
import { stateLabel } from "../shared/states";
import { formatMinuteKST } from "../shared/time";
import { eventText } from "../shared/timeline";
import { TaskActions } from "../shared/actions/TaskActions";
import { CommandLadder } from "../shared/actions/CommandLadder";
import type { OperatorCommandView } from "../shared/actions/commands";
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
type DetailLoad = { detail: BoardDetail } | { missing: true; id: number };
export function TaskRecord({ id }: { id: number }) {
  const [comments, setComments] = useState<BoardComment[]>([]);
  const [commentsError, setCommentsError] = useState(false);
  const [commands, setCommands] = useState<OperatorCommandView[]>([]);
  const [priorityOverride, setPriorityOverride] = useState<number | null>(null);
  const [attempt, setAttempt] = useState(0);
  // Locally recorded commands stay visible until a poll includes them, so a
  // poll that started before the write cannot replace the fresh row.
  const pending = useRef(new Map<number, OperatorCommandView>());
  const load = useCallback(async (): Promise<DetailLoad> => {
    try {
      return { detail: await fetchTaskDetail(id) };
    } catch (e) {
      if (e instanceof HttpError && e.status === 404) return { missing: true, id };
      throw e;
    }
  }, [id, attempt]);
  // The detail polls so an executor's receipt advances the command ladder
  // without a reload. Retry and a lifecycle write restart the loop through
  // attempt.
  const { data: loaded, failed } = usePoll(load, 15000);
  const missing = !!loaded && "missing" in loaded && loaded.id === id;
  const data = loaded && "detail" in loaded && loaded.detail.task.id === id ? loaded.detail : null;
  useEffect(() => { setCommands([]); setPriorityOverride(null); setComments([]); setCommentsError(false); pending.current.clear(); }, [id]);
  useEffect(() => {
    if (!data) return;
    for (const command of data.commands) pending.current.delete(command.id);
    setCommands([...pending.current.values(), ...data.commands]);
  }, [data]);
  const loadedOnce = data !== null;
  useEffect(() => {
    if (!loadedOnce) return;
    let active = true;
    void fetchTaskComments(id).then(result => { if (active) setComments(result.comments); }).catch(() => { if (active) setCommentsError(true); });
    return () => { active = false; };
  }, [id, loadedOnce]);
  if (missing) return <p>#{id} 없음</p>;
  if (!data) return failed ? <p role="alert">불러오지 못함 <button onClick={() => setAttempt(n => n + 1)}>다시 시도</button></p> : <p role="status">Loading…</p>;
  const { task: loadedTask, origins, documents, changes, jobs } = data;
  const task = priorityOverride === null ? loadedTask : { ...loadedTask, priority: priorityOverride };
  return <article className="work-record">
    {failed && <p role="alert" className="work-stale">최신 정보를 불러오지 못했습니다. 마지막으로 받은 내용입니다.</p>}
    <header data-action-task={task.id}><TaskActions target={task} onPriority={priority => setPriorityOverride(priority)} onCommand={command => { pending.current.set(command.id, command); setCommands(old => [command, ...old]); }} onApplied={() => setAttempt(n => n + 1)} /><h1>#{task.id} {task.title}</h1><p>{stateLabel(task.state)} · {task.project || "—"} · {task.lane} · P{task.priority} · {task.kind}</p></header>
    <Section title="종료 근거">{task.refs.closure ? <p>{task.refs.closure.type} · {task.refs.closure.evidence}</p> : ["merged", "deployed", "completed", "dropped", "duplicate", "canceled"].includes(task.state) ? <p>종결 증거 없음</p> : <p>기록 없음</p>}</Section>
    <Section title="출처" truncated={data.origins_truncated}>{origins.length ? origins.map((o, i) => <p key={i}>{o.provider} · {o.namespace} · {o.url ? <a href={o.url}>{o.display_ref || o.url}</a> : o.display_ref || "—"}</p>) : <p>기록 없음</p>}</Section>
    <Section title="문서" truncated={data.documents_truncated}>{documents.length ? documents.map(d => <p key={d.key}><a href={`/ui/doc/${d.key.split("/").map(encodeURIComponent).join("/")}`}>{d.key}</a> · {d.kind} · {d.origin_class || "미분류"}</p>) : <p>기록 없음</p>}</Section>
    <Section title="변경" truncated={data.changes_truncated}>{changes.length ? changes.map(c => <div key={c.id}><p><a href={c.pr}>{c.pr}</a> · {c.state} · {c.head_sha?.slice(0, 7)} · {c.merged_at && formatMinuteKST(c.merged_at)} · {c.hold_reason} {c.depends_on_change_id && `depends on #${c.depends_on_change_id} ${c.depends_on_state || ""}`}</p>{c.reviews?.map(r => <p key={r.id}>{r.lens} · {r.round}/{r.pass} · {r.family}/{r.model} · {r.verdict} · c {r.critical ?? "?"} h {r.high ?? "?"} m {r.medium ?? "?"} l {r.low ?? "?"}</p>)}</div>) : <p>기록 없음</p>}</Section>
    <Section title="작업 실행" truncated={data.jobs_truncated}>{jobs.length ? jobs.map(j => <div key={j.job_id}><p>{[j.job_id, j.role, j.model, j.harness, j.effort, j.profile, j.host].filter(Boolean).join(" · ")}</p><p>{j.skills_supported === false ? "skills not supported" : j.skills?.map(s => `${s.name} (${s.calls})`).join(", ") || "skills 없음"} · {j.end_reason} {j.diff_stat && `${j.diff_stat.files} files +${j.diff_stat.insertions} −${j.diff_stat.deletions}`}</p></div>) : <p>기록 없음</p>}</Section>
    <Section title="소요 시간"><p>대기: {task.started_at ? duration(task.created_at, task.started_at) : "—"}</p><p>작업: {task.started_at ? `${task.completed_at || task.canceled_at ? "" : "진행 중 "}${duration(task.started_at, task.completed_at || task.canceled_at)}` : "—"}</p>{jobs.map(j => <p key={j.job_id}>{j.job_id}: spawned → ready {j.ready_at ? duration(j.spawned_at, j.ready_at) : "—"} · ready → ended {j.ended_at ? duration(j.ready_at, j.ended_at) : "—"}</p>)}{data.dwell.length ? data.dwell.map(d => <p key={d.state}>{stateLabel(d.state)}: {Math.floor(d.seconds / 60)}m</p>) : <p>상태 기록 없음</p>}</Section>
    <Section title="운영 명령" truncated={data.commands_truncated}>{commands.length ? commands.map(command => <CommandLadder key={command.id} command={command} />) : <p>기록 없음</p>}</Section>
    <Section title="타임라인">{commentsError && <p role="alert">댓글을 불러오지 못했습니다.</p>}{[...data.events.map(e => ({ key: `e${e.id}`, at: e.at, actor: e.by, text: eventText(e) })), ...comments.map(c => ({ key: `c${c.id}`, at: c.created_at, actor: c.author, text: c.body }))].sort((a,b) => Date.parse(a.at) - Date.parse(b.at)).map(item => <p key={item.key}>{formatMinuteKST(item.at)} · {item.actor} · {item.text}</p>)}</Section>
  </article>;
}
