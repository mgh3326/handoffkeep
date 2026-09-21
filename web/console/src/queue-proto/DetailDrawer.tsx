import { useEffect, useRef } from "react";
import type { BoardDetail } from "../board/types";
import { ageDays, isStale, STALE_MIN_AGE_DAYS } from "./adapter";
import type { Dataset, Enrichment, ProtoTask } from "./types";

/** Lazily fetched per-drawer detail (live mode). Absent → the task's own
 * fields are the truth, which is the fixture/test path. */
export type DetailFetchState = { status: "loading" } | { status: "loaded"; data: BoardDetail } | { status: "error" };

function safeHref(value: string): string | undefined {
  try {
    return new URL(value).protocol === "https:" ? value : undefined;
  } catch {
    return undefined;
  }
}

function Val({ value }: { value: string | number | null }) {
  if (value === null) {
    return <span className="qp-unknown">unknown</span>;
  }
  return <span>{value}</span>;
}

type DrawerProps = {
  dataset: Dataset;
  task: ProtoTask;
  detail?: DetailFetchState;
  orderedIds: number[];
  onClose: () => void;
  onNav: (id: number) => void;
};

export function DetailDrawer({ dataset, task, detail, orderedIds, onClose, onNav }: DrawerProps) {
  const ref = useRef<HTMLDivElement>(null);
  const index = orderedIds.indexOf(task.id);
  const prevId = index > 0 ? orderedIds[index - 1] : null;
  const nextId = index >= 0 && index < orderedIds.length - 1 ? orderedIds[index + 1] : null;
  const enr: Enrichment | undefined = dataset.enrichment[task.id];
  const now = dataset.generatedAt;

  useEffect(() => {
    ref.current?.focus();
  }, [task.id]);

  const onKeyDown = (event: React.KeyboardEvent) => {
    if (event.key === "Escape") {
      event.preventDefault();
      onClose();
    } else if (event.key === "ArrowUp" && prevId !== null) {
      event.preventDefault();
      onNav(prevId);
    } else if (event.key === "ArrowDown" && nextId !== null) {
      event.preventDefault();
      onNav(nextId);
    }
  };

  const stateAge = ageDays(now, task.state_entered_at);
  const createdAge = ageDays(now, task.created_at);

  // Detail-loaded fields fall back to the task's own values. An absent or
  // empty value stays honest: not-collected renders "unknown", never 0.
  const dwell = detail?.status === "loaded" ? detail.data.dwell : task.dwell;
  const coverage =
    detail?.status === "loaded"
      ? {
          status: detail.data.participants.coverage,
          participants: detail.data.participants.coverage === "collected" ? detail.data.participants.segments.length : null,
          truncated: detail.data.participants.truncated === true,
        }
      : { status: task.coverage.status, participants: task.coverage.participants, truncated: false };

  return (
    <div className="qp-drawer" role="dialog" aria-modal="true" aria-label={`task ${task.id} detail`} ref={ref} tabIndex={-1} onKeyDown={onKeyDown}>
      <div className="qp-drawer-head">
        <h3>#{task.id}</h3>
        <div className="qp-drawer-nav">
          <button type="button" aria-label="Previous task" disabled={prevId === null} onClick={() => prevId !== null && onNav(prevId)}>
            ↑ prev
          </button>
          <button type="button" aria-label="Next task" disabled={nextId === null} onClick={() => nextId !== null && onNav(nextId)}>
            ↓ next
          </button>
          <button type="button" aria-label="Close detail" className="qp-drawer-close" onClick={onClose}>
            ✕ close
          </button>
        </div>
      </div>
      <p className="qp-drawer-title">{task.title}</p>
      <p className="qp-source-status">
        source status: <strong>{dataset.source === "live" ? "live /ui/api/board" : "synthetic fixture"}</strong> — {dataset.completeness} ·{" "}
        {dataset.completenessNote}
      </p>
      {isStale(task, now) ? (
        <p className="qp-stale-note" role="note">
          ⚠ stale — non-terminal task aged ≥{STALE_MIN_AGE_DAYS}d since created_at. Age does not imply the premise is still
          valid or safe to execute.
        </p>
      ) : null}
      <dl className="qp-drawer-meta">
        <dt>state</dt>
        <dd>{task.state}</dd>
        <dt>kind</dt>
        <dd>{task.kind}</dd>
        <dt>lane</dt>
        <dd>{task.lane}</dd>
        <dt>claimant</dt>
        <dd>
          <Val value={task.claimant} />
        </dd>
        <dt>priority</dt>
        <dd>p{task.priority}</dd>
        <dt>created age</dt>
        <dd>
          {createdAge === null ? <span className="qp-unknown">unknown</span> : `${createdAge}d`}{" "}
          <span className="muted">(since created_at)</span>
        </dd>
        <dt>current-state age</dt>
        <dd>
          {stateAge === null ? <span className="qp-unknown">unknown</span> : `${stateAge}d`}{" "}
          <span className="muted">(since state_entered_at)</span>
        </dd>
        <dt>due</dt>
        <dd>
          <Val value={task.due_at} />
        </dd>
        <dt>blocker</dt>
        <dd>
          <Val value={task.blocker} />
        </dd>
      </dl>
      {task.decision ? (
        <section className="qp-drawer-sec">
          <h4>decision needed</h4>
          <p>{task.decision.question}</p>
          <p className="muted">evidence: {task.decision.evidence}</p>
        </section>
      ) : null}
      <section className="qp-drawer-sec">
        <h4>refs</h4>
        <ul className="qp-drawer-refs">
          {task.refs.report_path ? <li>report: {task.refs.report_path}</li> : null}
          {task.refs.job_id ? <li>job: {task.refs.job_id}</li> : null}
          {task.refs.pr ? (
            <li>
              pr: {safeHref(task.refs.pr) ? <a href={safeHref(task.refs.pr)}>{task.refs.pr}</a> : task.refs.pr}
            </li>
          ) : null}
          {task.refs.head_sha ? <li>head: {task.refs.head_sha.slice(0, 9)}</li> : null}
        </ul>
      </section>
      <section className="qp-drawer-sec">
        <h4>draft enrichment (synthetic, unreviewed)</h4>
        {enr ? (
          <dl className="qp-drawer-meta">
            <dt>area</dt>
            <dd>
              <Val value={enr.area} />
            </dd>
            <dt>bundle</dt>
            <dd>
              <Val value={enr.bundle} />
            </dd>
            <dt>standalone</dt>
            <dd>{enr.standalone ? "yes (intentional)" : "no"}</dd>
            <dt>labels</dt>
            <dd>{enr.labels.length > 0 ? enr.labels.join(", ") : "none"}</dd>
          </dl>
        ) : (
          <p className="muted">unclassified — no draft enrichment</p>
        )}
        {enr && enr.relations.length > 0 ? (
          <ul className="qp-drawer-refs">
            {enr.relations.map((rel) => (
              <li key={`${rel.type}-${rel.otherId}`}>
                {rel.type} → #{rel.otherId} <span className="muted">({rel.note})</span>
              </li>
            ))}
          </ul>
        ) : null}
      </section>
      <section className="qp-drawer-sec">
        <h4>dwell</h4>
        {dwell.length === 0 ? (
          detail?.status === "loaded" ? (
            <p className="muted">none recorded</p>
          ) : (
            // An empty dwell list means not collected — it must render as
            // unknown, never as an implied "0s dwell" blank section.
            <p className="qp-unknown">unknown — not collected</p>
          )
        ) : (
          <ul className="qp-drawer-refs">
            {dwell.map((seg) => (
              <li key={seg.state}>
                {seg.state}: {seg.seconds}s{seg.open ? " (in progress)" : ""}
              </li>
            ))}
          </ul>
        )}
      </section>
      <section className="qp-drawer-sec">
        <h4>participation coverage</h4>
        {coverage.status === "not_collected" ? (
          <p className="qp-unknown">unknown — not collected</p>
        ) : (
          <p>
            collected · participants:{" "}
            <strong data-testid="participant-count">
              {coverage.participants ?? "unknown"}
              {coverage.truncated ? "+" : ""}
            </strong>
          </p>
        )}
      </section>
      <section className="qp-drawer-sec">
        <h4>history</h4>
        {detail === undefined ? (
          task.events.length === 0 ? (
            <p className="muted">none recorded</p>
          ) : (
            <ol className="qp-drawer-refs">
              {task.events.map((event) => (
                <li key={event.id}>
                  {event.from} → {event.to} by {event.by} at <time>{event.at}</time>
                  {event.note ? <span className="muted"> — {event.note}</span> : null}
                </li>
              ))}
            </ol>
          )
        ) : detail.status === "loading" ? (
          <p className="muted">loading…</p>
        ) : detail.status === "error" ? (
          <p className="qp-unknown">unavailable — detail fetch failed</p>
        ) : detail.data.events.length === 0 ? (
          <p className="muted">none recorded</p>
        ) : (
          <ol className="qp-drawer-refs">
            {detail.data.events.map((event) => (
              <li key={event.id}>
                {event.from} → {event.to} by {event.by} at <time>{event.at}</time>
                {event.note ? <span className="muted"> — {event.note}</span> : null}
              </li>
            ))}
          </ol>
        )}
      </section>
    </div>
  );
}
