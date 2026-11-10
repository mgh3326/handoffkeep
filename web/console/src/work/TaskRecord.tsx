import { TranslatedText } from "../shared/translation";
import { useEffect, useRef, useState } from "react";
import { fetchTaskDetail, fetchTaskComments, fetchTaskAttachments, HttpError } from "../shared/boardApi";
import type { BoardAttachment, BoardComment, BoardDetail } from "../shared/boardTypes";
import { stateLabel } from "../shared/states";
import { formatMinuteKST } from "../shared/time";
import { eventText } from "../shared/timeline";
import { TaskActions } from "../shared/actions/TaskActions";
import { JobRunView } from "./JobRunView";
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
  const [attachments, setAttachments] = useState<BoardAttachment[]>([]);
  const [attachmentsError, setAttachmentsError] = useState(false);
  const [commentsError, setCommentsError] = useState(false);
  const [data, setData] = useState<BoardDetail | null>(null);
  const [error, setError] = useState<number | null>(null);
  const [attempt, setAttempt] = useState(0);
  const loadedID = useRef(0);
  useEffect(() => {
    let active = true;
    // A refresh after a write keeps the record mounted (focus, comments and
    // timeline stay); only a different task clears it.
    if (loadedID.current !== id) {
      loadedID.current = id;
      setData(null); setComments([]); setCommentsError(false); setAttachments([]); setAttachmentsError(false);
    }
    setError(null);
    void fetchTaskDetail(id).then(value => { if (active) { setData(value); void fetchTaskComments(id).then(result => { if (active) setComments(result.comments); }).catch(() => { if (active) setCommentsError(true); }); void fetchTaskAttachments(id).then(result => { if (active) setAttachments(result); }).catch(() => { if (active) setAttachmentsError(true); }); } }).catch((e: unknown) => { if (active) setError(e instanceof HttpError ? e.status : 500); });
    return () => { active = false; };
  }, [id, attempt]);
  if (error === 404) return <p>#{id} 없음</p>;
  if (error !== null) return <p role="alert">불러오지 못함 <button onClick={() => setAttempt(n => n + 1)}>다시 시도</button></p>;
  if (!data) return <p role="status">Loading…</p>;
  const { task, origins, documents, changes, jobs } = data;
  return <article className="work-record">
    <header data-action-task={task.id}><TaskActions target={task} onPriority={priority => setData(old => old ? { ...old, task: { ...old.task, priority } } : old)} onApplied={() => setAttempt(n => n + 1)} /><h1>#{task.id} <TranslatedText original={task.title} korean={task.title_ko} stale={task.title_ko_stale} /></h1><p>{stateLabel(task.state)} · {task.project || "—"} · {task.lane} · P{task.priority} · {task.kind}</p></header>
    {data.body && <Section title="본문"><TranslatedText original={data.body} korean={data.body_ko} stale={data.body_ko_stale} /></Section>}
    <Section title="종료 근거">{task.refs.closure ? <p>{task.refs.closure.type} · {task.refs.closure.evidence}</p> : ["merged", "deployed", "completed", "dropped", "duplicate", "canceled"].includes(task.state) ? <p>종결 증거 없음</p> : <p>기록 없음</p>}</Section>
    <Section title="출처" truncated={data.origins_truncated}>{origins.length ? origins.map((o, i) => <p key={i}>{o.provider} · {o.namespace} · {o.url ? <a href={o.url}>{o.display_ref || o.url}</a> : o.display_ref || "—"}</p>) : <p>기록 없음</p>}</Section>
    <Section title="문서" truncated={data.documents_truncated}>{documents.length ? documents.map(d => <p key={d.key}><a href={`/ui/doc/${d.key.split("/").map(encodeURIComponent).join("/")}`}>{d.key}</a> · {d.kind} · {d.origin_class || "미분류"}</p>) : <p>기록 없음</p>}</Section>
    <Section title="첨부">{attachmentsError ? <p role="alert">첨부를 불러오지 못했습니다.</p> : attachments.length ? attachments.map(a => <p key={a.sha256}><a href={`/ui/files/${a.sha256}`}>{a.original_name}</a> · {a.size_bytes} bytes · {a.mime} · {formatMinuteKST(a.created_at)} · {a.created_by}</p>) : <p>첨부 없음</p>}</Section>
    <Section title="변경" truncated={data.changes_truncated}>{changes.length ? changes.map(c => <div key={c.id}><p><a href={c.pr}>{c.pr}</a> · {c.state} · {c.head_sha?.slice(0, 7)} · {c.merged_at && formatMinuteKST(c.merged_at)} · {c.hold_reason} {c.depends_on_change_id && `depends on #${c.depends_on_change_id} ${c.depends_on_state || ""}`}</p>{c.reviews?.map(r => <p key={r.id}>{r.lens} · {r.round}/{r.pass} · {r.family}/{r.model} · {r.verdict} · c {r.critical ?? "?"} h {r.high ?? "?"} m {r.medium ?? "?"} l {r.low ?? "?"}</p>)}</div>) : <p>기록 없음</p>}</Section>
    <Section title="작업 실행" truncated={data.jobs_truncated}>{jobs.length ? jobs.map(j => <JobRunView key={j.job_id} job={j} pr={j.job_id === task.refs.job_id ? task.refs.pr : undefined} />) : <p>기록 없음</p>}</Section>
    <Section title="소요 시간"><p>대기: {task.started_at ? duration(task.created_at, task.started_at) : "—"}</p><p>작업: {task.started_at ? `${task.completed_at || task.canceled_at ? "" : "진행 중 "}${duration(task.started_at, task.completed_at || task.canceled_at)}` : "—"}</p>{jobs.map(j => <p key={j.job_id}>{j.job_id}: spawned → ready {j.ready_at ? duration(j.spawned_at, j.ready_at) : "—"} · ready → ended {j.ended_at ? duration(j.ready_at, j.ended_at) : "—"}</p>)}{data.dwell.length ? data.dwell.map(d => <p key={d.state}>{stateLabel(d.state)}: {Math.floor(d.seconds / 60)}m</p>) : <p>상태 기록 없음</p>}</Section>
    <Section title="타임라인">{commentsError && <p role="alert">댓글을 불러오지 못했습니다.</p>}{[...data.events.map(e => ({ key: `e${e.id}`, at: e.at, actor: e.by, text: eventText(e) })), ...comments.map(c => ({ key: `c${c.id}`, at: c.created_at, actor: c.author, text: <TranslatedText original={c.body} korean={c.body_ko} stale={c.body_ko_stale} /> }))].sort((a,b) => Date.parse(a.at) - Date.parse(b.at)).map(item => <p key={item.key}>{formatMinuteKST(item.at)} · {item.actor} · {item.text}</p>)}</Section>
  </article>;
}
