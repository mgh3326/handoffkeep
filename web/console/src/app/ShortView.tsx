// The short schema view (hk 6392): what a tbls `<prefix>/doc` key opens by
// default. Three groups — largest storage from the stats sibling, highest
// read cost and the cleanup review from the review.json sibling — each row
// carrying the state the owning lane keeps current. A review.json that fails
// validation shows one error line naming the problem, plus the storage
// group, never an empty or half-silent view.

import { useEffect, useState } from "react";
import { HttpError } from "../shared/http";
import { fetchDoc, type DocPayload } from "../shared/doc/docApi";
import {
  STATE_LABEL,
  docHref,
  formatCount,
  fullDocHref,
  largestStorage,
  parseReview,
  parseStatsHistory,
  siblingReviewKey,
  siblingStatsKey,
  tableSectionHref,
  tableState,
  type ReviewDoc,
  type ReviewTable,
  type StatsSnapshot,
  type StorageRow,
} from "../shared/doc/shortView";
import { formatDayUTC, formatMinuteUTC } from "../shared/time";
import "../shared/doc/doc.css";

type Sibling<T> =
  | { status: "loading" }
  | { status: "ready"; value: T }
  | { status: "missing" }
  | { status: "broken"; message: string };

function toSibling<T>(promise: Promise<DocPayload>, parse: (body: string) => T): Promise<Sibling<T>> {
  return promise.then(
    (doc) => {
      try {
        return { status: "ready", value: parse(doc.body) } as Sibling<T>;
      } catch (err) {
        return { status: "broken", message: err instanceof Error ? err.message : String(err) } as Sibling<T>;
      }
    },
    (err: unknown) => {
      if (err instanceof HttpError && err.status === 404) {
        return { status: "missing" } as Sibling<T>;
      }
      return { status: "broken", message: err instanceof Error ? err.message : "불러오지 못했습니다" } as Sibling<T>;
    },
  );
}

type State = {
  stats: Sibling<StatsSnapshot[]>;
  review: Sibling<ReviewDoc>;
};

function deltaText(row: StorageRow): string {
  if (row.delta.kind === "new") {
    return "신규";
  }
  if (row.delta.kind === "same") {
    return "비교 없음";
  }
  const v = row.delta.value;
  return v > 0 ? `+${formatCount(v)}` : formatCount(v);
}

function StateField({ table }: { table: ReviewTable }) {
  if (table.state === "DECIDED") {
    return <span className="hk-short-field">결정 {table.decision}</span>;
  }
  if (table.state === "NOT_A_PROBLEM") {
    return (
      <a className="hk-short-field" href={docHref(table.finding ?? "")}>
        {table.finding}
      </a>
    );
  }
  if (table.state === "WAITING") {
    return <span className="hk-short-field">{table.until}</span>;
  }
  return null;
}
function TableRow({
  docKey,
  name,
  table,
  meta,
}: {
  docKey: string;
  name: string;
  table: ReviewTable;
  meta: string | null;
}) {
  return (
    <li className="hk-short-row">
      <div className="hk-short-line">
        <a className="hk-short-table" href={tableSectionHref(docKey, name)}>
          <code>{name}</code>
        </a>
        <span className="hk-short-badge" data-state={table.state}>
          {STATE_LABEL[table.state]}
        </span>
        <StateField table={table} />
        {meta !== null && <span className="hk-short-meta">{meta}</span>}
      </div>
      {table.note !== "" && <p className="hk-short-note">{table.note}</p>}
    </li>
  );
}

export default function ShortView({ docKey }: { docKey: string }) {
  const statsKey = siblingStatsKey(docKey);
  const reviewKey = siblingReviewKey(docKey);
  const [state, setState] = useState<State>({ stats: { status: "loading" }, review: { status: "loading" } });
  useEffect(() => {
    let cancelled = false;
    setState({ stats: { status: "loading" }, review: { status: "loading" } });
    void Promise.all([
      toSibling<StatsSnapshot[]>(fetchDoc(statsKey), parseStatsHistory),
      toSibling<ReviewDoc>(fetchDoc(reviewKey), parseReview),
    ]).then(([stats, review]) => {
      if (!cancelled) {
        setState({ stats, review });
      }
    });
    return () => {
      cancelled = true;
    };
  }, [statsKey, reviewKey]);

  if (state.stats.status === "loading" || state.review.status === "loading") {
    return <p role="status">Loading summary…</p>;
  }

  const storage = state.stats.status === "ready" ? largestStorage(state.stats.value) : null;
  const review = state.review.status === "ready" ? state.review.value : null;
  const snapshotAt = storage !== null ? formatMinuteUTC(storage.at) : null;

  return (
    <article className="hk-view hk-doc hk-short">
      <header className="hk-doc-head">
        <h1>{docKey}</h1>
        <p className="muted">
          스키마 요약{snapshotAt !== null ? ` · 스냅샷 ${snapshotAt} 기준` : ""}
        </p>
        <p>
          <a className="hk-button" href={fullDocHref(docKey)}>
            전체 문서
          </a>
        </p>
      </header>
      {state.review.status === "broken" && <p role="alert">{state.review.message}</p>}
      {state.review.status === "missing" && (
        <p role="note">상태 파일이 없습니다: {reviewKey} — 저장 용량만 보여줍니다.</p>
      )}
      {state.stats.status === "missing" && <p role="note">통계 파일이 없습니다: {statsKey}.</p>}
      {state.stats.status === "broken" && <p role="note">통계를 보여줄 수 없습니다: {state.stats.message}</p>}
      {storage !== null && (
        <section aria-label="저장 용량 상위">
          <h2>저장 용량 상위</h2>
          <ul className="hk-short-group">
            {storage.rows.map((row) => (
              <TableRow
                key={row.table}
                docKey={docKey}
                name={row.table}
                table={tableState(review?.tables ?? {}, row.table)}
                meta={`${formatCount(row.bytes)} bytes (${deltaText(row)})`}
              />
            ))}
          </ul>
        </section>
      )}
      {review !== null && (
        <section aria-label="읽기 비용 상위">
          <h2>읽기 비용 상위</h2>
          <p className="muted">
            {formatDayUTC(review.readCost.windowStart)}–{formatDayUTC(review.readCost.windowEnd)} · 출처:{" "}
            <a href={docHref(review.readCost.source)}>{review.readCost.source}</a>
          </p>
          <ul className="hk-short-group">
            {review.readCost.rows.slice(0, 5).map((row) => (
              <TableRow
                key={row.table}
                docKey={docKey}
                name={row.table}
                table={tableState(review.tables, row.table)}
                meta={`${formatCount(row.rowsRead)} rows`}
              />
            ))}
          </ul>
        </section>
      )}
      {review !== null && (
        <section aria-label="정리 검토">
          <h2>정리 검토</h2>
          <ul className="hk-short-group">
            {review.cleanup.slice(0, 5).map((name) => (
              <TableRow key={name} docKey={docKey} name={name} table={tableState(review.tables, name)} meta={null} />
            ))}
          </ul>
        </section>
      )}
    </article>
  );
}
