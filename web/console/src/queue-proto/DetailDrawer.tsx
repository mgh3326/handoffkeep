import { Fragment, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { fetchBoardDoc } from "../board/api";
import type { BoardDetail, BoardDoc, ParticipantSegment } from "../board/types";
import { ActivityTabs } from "./ActivityTabs";
import { ageDays, isStale, STALE_MIN_AGE_DAYS } from "./adapter";
import { docPageHref, titleDocKeys } from "./bodydoc";
import { DocInline, type FetchDoc } from "./DocInline";
import { TaskComments, type CommentsClient } from "./TaskComments";
import { useTaskDocMeta, type TaskDocMetaState } from "./taskmeta";
import type { Dataset, Enrichment, ProtoTask } from "./types";

/** Lazily fetched per-drawer detail (live mode). Absent → the task's own
 * fields are the truth, which is the fixture/test path. "notfound" is a
 * resolved 404 — the task id does not exist — never collapsed into "error". */
export type DetailFetchState = { status: "loading" } | { status: "loaded"; data: BoardDetail } | { status: "error" } | { status: "notfound" };

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

/** Copies the canonical /ui/tasks/<id> link — the same URL the full-page
 * detail serves. Hovering the button shows the target even when the
 * clipboard API is unavailable (insecure context, denied permission). */
export function CopyTaskLink({ id }: { id: number }) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(
    () => () => {
      if (timer.current !== null) {
        clearTimeout(timer.current);
      }
    },
    [],
  );
  const url = `${window.location.origin}/ui/tasks/${id}`;
  const copy = () => {
    const done = () => {
      setCopied(true);
      if (timer.current !== null) {
        clearTimeout(timer.current);
      }
      timer.current = setTimeout(() => setCopied(false), 1500);
    };
    if (navigator.clipboard?.writeText) {
      void navigator.clipboard.writeText(url).then(done, () => {});
    }
  };
  return (
    <button type="button" className="qp-copylink" aria-label="Copy task link" title={url} onClick={copy}>
      {copied ? "✓ copied" : "🔗 copy link"}
    </button>
  );
}

/** Titles past the old 96-char list preview cut count as "long" — only those
 * can be carrying the spec that a missing body document would have held. */
const LONG_TITLE_MIN = 96;
function isLongTitle(title: string): boolean {
  return [...title].length > LONG_TITLE_MIN;
}

/** The overview's spec section. Only body_doc is rendered inline; a key found
 * in the title is a named link, never inlined and never written back. An
 * absent body is said in words — the overview is never silently blank — and
 * a body-less task's long title stays readable in full in "등재 원문". */
function TaskBodySection({ task, fetchDoc }: { task: ProtoTask; fetchDoc: FetchDoc }) {
  const bodyDoc = task.body_doc ?? "";
  let content: ReactNode;
  if (bodyDoc !== "") {
    content = <DocInline bodyDoc={bodyDoc} fetchDoc={fetchDoc} stripFrontMatter />;
  } else {
    const titleKeys = titleDocKeys(task.title);
    content = (
      <div className="qp-doc" data-doc-state={titleKeys.length > 0 ? "title-fallback" : "none"}>
        <p className="qp-unknown" role="note">
          본문 문서가 연결되지 않았습니다 (body_doc 없음). 본문은 등재할 때 <code>tasks add --doc &lt;key&gt;</code> 로 붙입니다.
        </p>
        {titleKeys.length > 0 ? (
          <>
            <p>title 에서 찾은 문서 — 링크만 제공하고 본문으로 렌더하지 않습니다:</p>
            <ul className="qp-drawer-refs">
              {titleKeys.map((key) => (
                <li key={key}>
                  <a href={docPageHref(key)}>{key}</a> <span className="muted">(title 에서 찾은 문서)</span>
                </li>
              ))}
            </ul>
          </>
        ) : null}
      </div>
    );
  }
  return (
    <section className="qp-drawer-sec qp-body-sec">
      {/* kept as "본문" — #619's test helper locates this section by heading,
          so renaming it would silently break that suite after merge. */}
      <h4>본문</h4>
      {content}
      {bodyDoc === "" && isLongTitle(task.title) ? (
        // A body-less task's long title is the only place its spec lives;
        // keep it readable in full below the document block.
        <details className="qp-raw-title">
          <summary>등재 원문</summary>
          <p>{task.title}</p>
        </details>
      ) : null}
    </section>
  );
}

/** 목적 — the task's summary, read only from the linked body_doc's
 * hk-task/v1 front-matter. Nothing is inferred: no metadata reads as
 * "요약 미작성", an unreadable document reads as unverified. */
function PurposeSection({ meta }: { meta: TaskDocMetaState }) {
  return (
    <section className="qp-drawer-sec qp-purpose">
      <h4>목적</h4>
      {meta.status === "loading" ? (
        <p className="muted">요약 확인 중…</p>
      ) : meta.status === "error" ? (
        <p className="qp-unknown">요약 미확인 — 본문 문서를 읽지 못했습니다.</p>
      ) : meta.status === "ready" && meta.meta?.summary ? (
        <p>{meta.meta.summary}</p>
      ) : (
        <p className="muted">요약 미작성</p>
      )}
    </section>
  );
}

function SegmentRow({ segment }: { segment: ParticipantSegment }) {
  const cell = (value: number | null) => (value === null ? <td className="qp-unknown">미수집</td> : <td>{value}</td>);
  return (
    <tr>
      <td>{segment.role ?? "unknown"}</td>
      <td>{segment.model_id ?? "unknown"}</td>
      <td>{segment.reps}</td>
      {cell(segment.rounds)}
      {cell(segment.blockers_found)}
      {cell(segment.completed)}
      {cell(segment.input_tokens)}
      {cell(segment.output_tokens)}
    </tr>
  );
}

type BodyProps = {
  dataset: Dataset;
  task: ProtoTask;
  detail?: DetailFetchState;
  /** Document loader for the overview body; defaults to the board BFF. */
  fetchDoc?: FetchDoc;
  /** Comment reader/writer for the 코멘트 tab. Absent → the synthetic
   * preview path, which never touches the network and says so. */
  comments?: CommentsClient;
};

/** The task detail content — the one component shared by the peek drawer and
 * the /ui/tasks/<id> page. Shell chrome (nav/close/back-link) is each host's
 * own; the tabs and sections below are not duplicated anywhere. */
export function DetailBody({ dataset, task, detail, fetchDoc, comments: commentsClient }: BodyProps) {
  const enr: Enrichment | undefined = dataset.enrichment[task.id];
  const now = dataset.generatedAt;
  const stateAge = ageDays(now, task.state_entered_at);
  const createdAge = ageDays(now, task.created_at);

  // One fetch per document key, shared by the metadata read and the body
  // renderer — the overview never asks the BFF for the same doc twice.
  const sharedFetchDoc = useMemo<FetchDoc>(() => {
    const base = fetchDoc ?? fetchBoardDoc;
    const cache = new Map<string, Promise<BoardDoc>>();
    return (key: string) => {
      const hit = cache.get(key);
      if (hit !== undefined) {
        return hit;
      }
      const promise = base(key);
      // A rejected fetch is dropped so reopening the drawer retries — a
      // transient error must not stick for the session's lifetime.
      promise.catch(() => cache.delete(key));
      cache.set(key, promise);
      return promise;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps -- the cache is
    // scoped to the current task so a stale body never survives a switch.
  }, [fetchDoc, task.id]);
  const meta = useTaskDocMeta(task.body_doc ?? "", sharedFetchDoc);
  const displayTitle = meta.status === "ready" ? (meta.meta?.displayTitle ?? null) : null;
  const titleLong = isLongTitle(task.title);
  // The 원문 text mounts on open — a closed details must not duplicate the
  // title into the DOM (text queries, find-in-page, copy all see it once).
  const [titleSrcOpen, setTitleSrcOpen] = useState(false);

  // Detail-loaded fields fall back to the task's own values only while no
  // fetch state exists; loading/error get their own honest states. An absent
  // or empty value stays honest: not-collected renders "unknown", never 0.
  const dwell = detail?.status === "loaded" ? detail.data.dwell : task.dwell;
  const coverage =
    detail?.status === "loaded"
      ? {
          status: detail.data.participants.coverage,
          participants: detail.data.participants.coverage === "collected" ? detail.data.participants.segments.length : null,
          truncated: detail.data.participants.truncated === true,
        }
      : { status: task.coverage.status, participants: task.coverage.participants, truncated: false };

  // Reading order is the operator's question order: what → why → who is
  // waiting on what → decision → spec → auxiliaries. Missing values are
  // named "미기록", never inferred from created_by or the latest note.
  const overview = (
    <>
      {/* Provenance of synthetic preview rows stays explicit. The live queue
          names its data status in the page header; internal endpoint names
          are developer detail and stay off the product screen (2402 Q9). */}
      {dataset.source === "synthetic" ? (
        <p className="qp-source-status">
          source status: <strong>synthetic fixture</strong> — {dataset.completeness} · {dataset.completenessNote}
        </p>
      ) : null}
      <PurposeSection meta={meta} />
      <section className="qp-drawer-sec qp-now">
        <h4>현재 상황</h4>
        <p>
          <strong>{task.state}</strong> · 책임 {task.lane !== "" ? task.lane : "미기록"} · 실행 담당{" "}
          {task.claimant !== null && task.claimant !== "" ? task.claimant : "미기록"}
        </p>
        {isStale(task, now) ? (
          <p className="qp-stale-note" role="note">
            ⚠ stale — non-terminal task aged ≥{STALE_MIN_AGE_DAYS}d since created_at. Age does not imply the premise is still
            valid or safe to execute.
          </p>
        ) : null}
      </section>
      <section className="qp-drawer-sec qp-next">
        <h4>다음 행동 · 대기</h4>
        {task.blocker !== null && task.blocker !== "" ? (
          <p>대기 — {task.blocker}</p>
        ) : (
          <p className="muted">대기 사유 미기록</p>
        )}
      </section>
      <section className="qp-drawer-sec qp-decision">
        <h4>결정</h4>
        {task.decision ? (
          <>
            <p>{task.decision.question}</p>
            <p className="muted">evidence: {task.decision.evidence}</p>
          </>
        ) : (
          // task.decision is only ever populated for synthetic fixtures — on
          // live data "absent" means "not connected", never "no request".
          // A needs_decision state still names itself honestly.
          <p className="muted">
            {task.state === "needs_decision"
              ? "상태는 결정 필요 — 결정 내용은 아직 미연결입니다."
              : "결정 요청 정보 미연결 — 결정 카드 연결은 #618 에서."}
          </p>
        )}
      </section>
      <TaskBodySection task={task} fetchDoc={sharedFetchDoc} />
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
          {detail?.status === "loaded" && detail.data.linear ? (
            <li>linear: {detail.data.linear.identifier || detail.data.linear.issue_id}</li>
          ) : null}
        </ul>
      </section>
      {/* Raw record fields stay recoverable but folded — they are audit
          data, not part of the five-second overview. */}
      <details className="qp-drawer-sec qp-meta-more">
        <summary>기록 세부</summary>
        <dl className="qp-drawer-meta">
          {([
            ["state", task.state],
            ["kind", task.kind],
            ["lane", task.lane !== "" ? task.lane : null],
            ["parent lane", task.parent_lane],
            ["claimant", task.claimant],
            ["priority", `p${task.priority}`],
            ["created age", createdAge === null ? null : `${createdAge}d`, "since created_at"],
            ["created by", task.created_by === "" ? null : task.created_by],
            ["updated", task.updated_at],
            ["current-state age", stateAge === null ? null : `${stateAge}d`, "since state_entered_at"],
            ["due", task.due_at],
            ["blocker", task.blocker],
          ] as [string, string | number | null, string?][]).map(([k, v, src]) => (
            <Fragment key={k}>
              <dt>{k}</dt>
              <dd>
                <Val value={v} /> {src !== undefined ? <span className="muted">({src})</span> : null}
              </dd>
            </Fragment>
          ))}
        </dl>
      </details>
      {dataset.source === "synthetic" ? (
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
      ) : null}
      <section className="qp-drawer-sec">
        <h4>dwell</h4>
        {detail?.status === "loading" ? (
          <p className="muted">loading…</p>
        ) : detail?.status === "error" ? (
          <p className="qp-unknown">unavailable — detail fetch failed</p>
        ) : dwell.length === 0 ? (
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
        {detail?.status === "loading" ? (
          <p className="muted">loading…</p>
        ) : detail?.status === "error" ? (
          <p className="qp-unknown">unavailable — detail fetch failed</p>
        ) : coverage.status === "not_collected" ? (
          <p className="qp-unknown">unknown — not collected</p>
        ) : (
          <>
            <p>
              collected · participants:{" "}
              <strong data-testid="participant-count">
                {coverage.participants ?? "unknown"}
                {coverage.truncated ? "+" : ""}
              </strong>
            </p>
            {detail?.status === "loaded" && detail.data.participants.segments.length > 0 ? (
              <div className="qp-table-scroll">
                <table className="qp-participants">
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
                    {detail.data.participants.segments.map((segment, index) => (
                      <SegmentRow key={`${segment.role ?? ""}:${segment.model_id ?? ""}:${index}`} segment={segment} />
                    ))}
                  </tbody>
                </table>
              </div>
            ) : null}
          </>
        )}
      </section>
    </>
  );

  // Keyed by task: switching tasks in the peek panel starts a fresh list and
  // an empty draft, never the previous task's comments.
  const comments = (
    <section className="qp-drawer-sec">
      <h4>코멘트</h4>
      {commentsClient ? (
        <TaskComments key={task.id} taskId={task.id} client={commentsClient} />
      ) : (
        <p className="qp-unknown" role="note">
          코멘트가 연결되지 않은 화면입니다(합성 미리보기) — 코멘트가 없다는 뜻이 아닙니다.
        </p>
      )}
    </section>
  );

  // Transitions are task_events. A note is data: plain text only, never
  // parsed as markdown or HTML.
  const transitions = (
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
      ) : detail.status === "notfound" ? (
        <p className="qp-unknown">unavailable — task not found</p>
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
  );

  return (
    <>
      <div className="qp-drawer-titlewrap">
        <p className="qp-drawer-title qp-title-clamp">{displayTitle ?? task.title}</p>
        {displayTitle !== null ? (
          <p className="qp-title-note muted">표시 제목 — 본문 문서의 display_title 입니다.</p>
        ) : titleLong ? (
          <p className="qp-title-note muted">긴 제목의 발췌입니다 — 원문은 펼치기로.</p>
        ) : null}
        {/* Every displayed title is clamped to two lines, so the 원문
            disclosure is always offered — a clipped title is never left
            without one-action recovery, at any width or zoom. */}
        <details className="qp-title-src" onToggle={(e) => setTitleSrcOpen(e.currentTarget.open)}>
          <summary>원문 제목 펼치기</summary>
          {titleSrcOpen ? <p>{task.title}</p> : null}
        </details>
      </div>
      <ActivityTabs panels={{ overview, comments, transitions }} />
    </>
  );
}

type DrawerProps = {
  dataset: Dataset;
  /** The requested id — shown in the head even before the task resolves. */
  taskId: number;
  /** null while a deep-linked task is fetched, or when it does not exist. */
  task: ProtoTask | null;
  detail?: DetailFetchState;
  orderedIds: number[];
  /** true once the task is known-absent (404, or no fetcher and no row). */
  notFound?: boolean;
  onClose: () => void;
  onNav: (id: number) => void;
  fetchDoc?: FetchDoc;
  comments?: CommentsClient;
};

// Non-modal peek panel: no inert background, no aria-modal — the list stays
// live and clicking another row swaps the content. Focus lands in the panel
// only on the closed→open transition; switching tasks keeps focus where the
// user is so keyboard and pointer both travel list↔panel freely.
export function DetailDrawer({ dataset, taskId, task, detail, orderedIds, notFound = false, onClose, onNav, fetchDoc, comments }: DrawerProps) {
  const ref = useRef<HTMLDivElement>(null);
  const index = orderedIds.indexOf(taskId);
  const prevId = index > 0 ? orderedIds[index - 1] : null;
  const nextId = index >= 0 && index < orderedIds.length - 1 ? orderedIds[index + 1] : null;

  useEffect(() => {
    ref.current?.focus();
  }, []);

  // Non-modal panel: focus may legitimately sit in the list while the panel
  // is open, so Escape listens at the document — closing must not depend on
  // where focus is. Arrow prev/next stays panel-scoped.
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        onClose();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);

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

  return (
    <div className="qp-drawer" role="dialog" aria-label={`task ${taskId} detail`} ref={ref} tabIndex={-1} onKeyDown={onKeyDown}>
      <div className="qp-drawer-head">
        <h3>
          #{taskId} <CopyTaskLink id={taskId} />
        </h3>
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
      {task === null ? (
        notFound ? (
          <p className="qp-unknown" role="note">
            task #{taskId} not found — no queue task has this id
          </p>
        ) : detail?.status === "error" ? (
          <p className="qp-unknown">unavailable — detail fetch failed</p>
        ) : (
          <p className="muted">loading…</p>
        )
      ) : (
        <DetailBody dataset={dataset} task={task} detail={detail} fetchDoc={fetchDoc} comments={comments} />
      )}
    </div>
  );
}
