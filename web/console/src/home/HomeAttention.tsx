import { stateLabel } from "../shared/states";
import { formatMinuteUTC } from "../shared/time";
import { usePoll } from "../shared/usePoll";
import { loadAttention } from "./api";
import type {
  Attention,
  AttentionDecision,
  AttentionDeploy,
  AttentionTask,
} from "./types";
import "./home.css";

/** Where a decision opens. A request has its own page (the route arrives with
 * the decision page, hk 3063); a lane event has no request id, so it opens the
 * inbox. */
function decisionHref(x: AttentionDecision): string {
  return x.source === "request" ? `/ui/decisions/${x.id}` : "/ui/decisions";
}

function when(iso: string): string {
  return formatMinuteUTC(iso) ?? "시각 미상";
}

function More({ count, shown }: { count: number; shown: number }) {
  return count > shown ? <p className="ha-more">+{count - shown} more</p> : null;
}

function Section({
  title,
  count,
  href,
  split,
  children,
}: {
  title: string;
  count: number;
  href?: string;
  split?: string;
  children: React.ReactNode;
}) {
  const heading = href ? <a href={href}>{title}</a> : title;
  return (
    <section className="ha-section" aria-label={title}>
      <h3 className="ha-head">
        {heading}
        <span className="ha-count" aria-label={`${count} ${title}`}>
          {count}
        </span>
        {split ? <span className="ha-split">{split}</span> : null}
      </h3>
      {children}
    </section>
  );
}

function DecisionRow({ x }: { x: AttentionDecision }) {
  return (
    <li className="ha-item">
      <a className="ha-link" href={decisionHref(x)}>
        {x.question || "(no question text)"}
      </a>
      <span className="ha-meta">
        <span>{x.lane}</span>
        {x.overdue ? <span className="ha-overdue">overdue</span> : null}
        {x.due_at ? <span>due {when(x.due_at)}</span> : null}
        <span>asked {when(x.created_at)}</span>
      </span>
    </li>
  );
}

function TaskRow({ x }: { x: AttentionTask }) {
  return (
    <li className="ha-item">
      <a className="ha-link" href={`/ui/tasks/${x.id}`}>
        #{x.id} {x.title}
      </a>
      <span className="ha-meta">
        <span>{x.lane}</span>
        <span>{stateLabel(x.state)}</span>
        <span className={x.reason === "stalled" ? "ha-stalled" : undefined}>
          {x.reason === "stalled" ? "stalled" : "awaiting decision"}
        </span>
        <span>since {when(x.since)}</span>
      </span>
    </li>
  );
}

function DeployRow({ x }: { x: AttentionDeploy }) {
  return (
    <li className="ha-item">
      <a className="ha-link" href={`/ui/tasks/${x.task_id}`}>
        #{x.task_id} {x.title}
      </a>
      <span className="ha-meta">
        <span>{x.service}</span>
        <span>merged {when(x.merged_at)}</span>
      </span>
    </li>
  );
}

function Body({ data }: { data: Attention }) {
  const { decisions, tasks, deploys } = data;
  if (decisions.count === 0 && tasks.count === 0 && deploys.count === 0) {
    return <p className="ha-empty">Nothing is waiting on you.</p>;
  }
  return (
    <>
      <Section title="Decisions" count={decisions.count}>
        {decisions.items.length === 0 ? (
          <p className="ha-none">None.</p>
        ) : (
          <ul className="ha-list">
            {decisions.items.map((x) => (
              <DecisionRow key={`${x.source}:${x.id}`} x={x} />
            ))}
          </ul>
        )}
        <More count={decisions.count} shown={decisions.items.length} />
      </Section>
      <Section
        title="Tasks"
        count={tasks.count}
        split={`${tasks.awaiting_decision} awaiting · ${tasks.stalled} stalled`}
      >
        {tasks.items.length === 0 ? (
          <p className="ha-none">None.</p>
        ) : (
          <ul className="ha-list">
            {tasks.items.map((x) => (
              <TaskRow key={x.id} x={x} />
            ))}
          </ul>
        )}
        <More count={tasks.count} shown={tasks.items.length} />
      </Section>
      <Section title="Deploys" count={deploys.count} href="/ui/deploys">
        {deploys.items.length === 0 ? (
          <p className="ha-none">None.</p>
        ) : (
          <ul className="ha-list">
            {deploys.items.map((x) => (
              <DeployRow key={`${x.service}:${x.task_id}`} x={x} />
            ))}
          </ul>
        )}
        <More count={deploys.count} shown={deploys.items.length} />
        {deploys.events_capped ? (
          <p className="ha-note">The merge scan hit its limit; this list may be incomplete.</p>
        ) : null}
      </Section>
    </>
  );
}

/** Home "waiting on me": open decisions, tasks that need the operator and
 * merges not yet deployed, from GET /ui/api/attention. */
export function HomeAttention() {
  const { data, failed } = usePoll(loadAttention, 60_000);
  return (
    <div className="ha-root">
      <h2 className="ha-title">Waiting on me</h2>
      {failed ? (
        <p className="ha-error" role="alert">
          Could not load what is waiting on you
          {data ? " · showing the last good data" : ""}.
        </p>
      ) : null}
      {data ? (
        <Body data={data} />
      ) : failed ? null : (
        <p className="ha-none" role="status">
          Loading…
        </p>
      )}
    </div>
  );
}
