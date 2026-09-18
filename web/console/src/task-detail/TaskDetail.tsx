import { useEffect, useState } from "react";
import { fetchTaskDetail } from "../board/api";
import type { BoardDetail, ParticipantSegment } from "../board/types";

function shortHead(value: string): string {
  return value.length > 9 ? value.slice(0, 9) : value;
}

// Only https targets become links; anything else (javascript:, data:, bare
// strings) renders as plain text. Server refs are never trusted as schemes.
function safeHref(value: string): string | undefined {
  try {
    return new URL(value).protocol === "https:" ? value : undefined;
  } catch {
    return undefined;
  }
}

function Refs({ detail }: { detail: BoardDetail }) {
  const refs = detail.task.refs;
  const cells: { label: string; value: string; href?: string }[] = [];
  if (refs.pr) {
    cells.push({ label: "PR", value: refs.pr, href: safeHref(refs.pr) });
  }
  if (refs.head_sha) {
    cells.push({ label: "head", value: shortHead(refs.head_sha) });
  }
  if (refs.report_path) {
    cells.push({ label: "report", value: refs.report_path, href: `/ui/doc/${refs.report_path}` });
  }
  if (refs.job_id) {
    cells.push({ label: "job", value: refs.job_id });
  }
  if (detail.linear) {
    cells.push({ label: "linear", value: detail.linear.identifier || detail.linear.issue_id });
  }
  if (cells.length === 0) {
    return null;
  }
  return (
    <div className="detail-refs">
      {cells.map((cell) =>
        cell.href ? (
          <a key={cell.label} href={cell.href}>
            {cell.label} {cell.value}
          </a>
        ) : (
          <span key={cell.label}>
            {cell.label} {cell.value}
          </span>
        ),
      )}
    </div>
  );
}

function Dwell({ detail }: { detail: BoardDetail }) {
  if (detail.dwell.length === 0) {
    return <p className="muted">체류 시간 데이터가 없습니다.</p>;
  }
  return (
    <ul className="detail-dwell">
      {detail.dwell.map((segment) => (
        <li key={segment.state}>
          {segment.state}: {segment.seconds}s{segment.open ? " (진행 중)" : ""}
        </li>
      ))}
    </ul>
  );
}

function SegmentRow({ segment }: { segment: ParticipantSegment }) {
  const numberCell = (value: number | null) => (value === null ? <td className="muted">미수집</td> : <td>{value}</td>);
  return (
    <tr>
      <td>{segment.role ?? "unknown"}</td>
      <td>{segment.model_id ?? "unknown"}</td>
      <td>{segment.reps}</td>
      {numberCell(segment.rounds)}
      {numberCell(segment.blockers_found)}
      {numberCell(segment.completed)}
      {numberCell(segment.input_tokens)}
      {numberCell(segment.output_tokens)}
    </tr>
  );
}

function Participants({ detail }: { detail: BoardDetail }) {
  const participants = detail.participants;
  return (
    <section className="detail-participants">
      <h4>
        참가자 <span className="muted">{participants.task_ref}</span> <span className="badge">{participants.coverage}</span>
      </h4>
      {participants.coverage === "not_collected" ? (
        <p className="muted">수집된 bench rep이 없습니다.</p>
      ) : (
        <table className="detail-table">
          <thead>
            <tr>
              <th>role</th>
              <th>model</th>
              <th>reps</th>
              <th>rounds</th>
              <th>blockers</th>
              <th>completed</th>
              <th>input tokens</th>
              <th>output tokens</th>
            </tr>
          </thead>
          <tbody>
            {participants.segments.map((segment, index) => (
              <SegmentRow key={`${segment.role ?? ""}:${segment.model_id ?? ""}:${index}`} segment={segment} />
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

function Transitions({ detail }: { detail: BoardDetail }) {
  if (detail.events.length === 0) {
    return <p className="muted">전이 기록이 없습니다.</p>;
  }
  return (
    <ol className="detail-events">
      {detail.events.map((event) => (
        <li key={event.id}>
          <strong>
            {event.from} → {event.to}
          </strong>{" "}
          by {event.by} at <time>{event.at}</time>
          {event.note ? <pre>{event.note}</pre> : null}
        </li>
      ))}
    </ol>
  );
}

export function TaskDetail({ id, onClose }: { id: number; onClose: () => void }) {
  const [detail, setDetail] = useState<BoardDetail | null>(null);
  const [error, setError] = useState(false);

  useEffect(() => {
    let cancelled = false;
    setDetail(null);
    setError(false);
    fetchTaskDetail(id)
      .then((next) => {
        if (!cancelled) {
          setDetail(next);
        }
      })
      .catch(() => {
        if (!cancelled) {
          setError(true);
        }
      });
    return () => {
      cancelled = true;
    };
  }, [id]);

  return (
    <aside className="task-detail" data-task-id={id}>
      <div className="task-detail-head">
        <h3>{detail ? `#${detail.task.id} ${detail.task.title}` : `#${id}`}</h3>
        <button type="button" onClick={onClose}>
          닫기
        </button>
      </div>
      {error ? <p className="muted">상세 정보를 불러오지 못했습니다.</p> : null}
      {!error && detail === null ? <p className="muted">불러오는 중</p> : null}
      {detail ? (
        <>
          <p className="muted">
            {detail.task.lane}
            {detail.task.parent_lane ? ` ← ${detail.task.parent_lane}` : ""} · {detail.task.kind} · {detail.task.state} · p{detail.task.priority}
          </p>
          <p className="muted">
            created_by {detail.task.created_by}
            {detail.task.claimed_by ? ` · claimed_by ${detail.task.claimed_by}` : ""} · updated <time>{detail.task.updated_at}</time>
          </p>
          <Refs detail={detail} />
          <h4>상태 체류</h4>
          <Dwell detail={detail} />
          <Participants detail={detail} />
          <h4>전이 기록</h4>
          <Transitions detail={detail} />
        </>
      ) : null}
    </aside>
  );
}
