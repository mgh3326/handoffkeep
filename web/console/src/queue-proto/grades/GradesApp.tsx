// #597 read-only grade table: the bench_catalog ladder grouped by grade
// (S+ → C), one row per (profile, effort). All display fields come from the
// API row — gate and estimate notation render the server's values, never a
// hardcoded list. Retired rows are hidden unless the viewer asks for them.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { fetchCatalog, type CatalogQuery } from "./api";
import type { BenchCatalogEntry, CatalogResponse } from "./types";

/** Server-side ladder order (the catalog CHECK constraint's closed set). A
 * grade outside it lands in the trailing "기타" group — never dropped. */
const GRADE_ORDER = ["S+", "S", "A+", "A", "B", "C"];

/** The effort rung order the store publishes ("" default row first, unknown
 * effort strings last) — mirrors benchEffortRanks for display grouping. */
const EFFORT_ORDER: Record<string, number> = { "": 0, low: 1, medium: 2, high: 3, xhigh: 4, max: 5 };

function effortRank(effort: string): number {
  return EFFORT_ORDER[effort] ?? 6;
}

/** The task contract names ESTIMATED_* as the annotation convention for
 * estimated scores; anything else in the field is shown verbatim. */
function isEstimated(entry: BenchCatalogEntry): boolean {
  return entry.benchmark_annotation !== null && entry.benchmark_annotation.startsWith("ESTIMATED");
}

function formatClock(iso: string): string | null {
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) {
    return null;
  }
  const d = new Date(t);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())} ${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())} UTC`;
}

function formatDay(iso: string): string {
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) {
    return "시각 미상";
  }
  const d = new Date(t);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())}`;
}

type Group = { grade: string; rows: BenchCatalogEntry[] };

function groupByGrade(rows: BenchCatalogEntry[]): Group[] {
  const byGrade = new Map<string, BenchCatalogEntry[]>();
  for (const row of rows) {
    const list = byGrade.get(row.grade) ?? [];
    list.push(row);
    byGrade.set(row.grade, list);
  }
  const ordered = [...GRADE_ORDER, ...[...byGrade.keys()].filter((g) => !GRADE_ORDER.includes(g)).sort()];
  const groups: Group[] = [];
  for (const grade of ordered) {
    const list = byGrade.get(grade);
    if (list === undefined) {
      continue;
    }
    groups.push({
      grade,
      rows: [...list].sort((a, b) => a.pool.localeCompare(b.pool) || a.profile.localeCompare(b.profile) || effortRank(a.effort) - effortRank(b.effort) || a.effort.localeCompare(b.effort)),
    });
  }
  return groups;
}

function GateCell({ entry }: { entry: BenchCatalogEntry }) {
  const label = entry.gate === "" ? "미상" : entry.gate;
  return (
    <td className="gr-gate" data-gate={entry.gate === "" ? "unknown" : entry.gate}>
      <span className={`gr-gate-chip${entry.gate !== "default" ? " gr-gate-nominal" : ""}`}>{label}</span>
      {entry.gate_reason !== null ? <span className="gr-gate-reason">{entry.gate_reason}</span> : null}
    </td>
  );
}

function Row({ entry }: { entry: BenchCatalogEntry }) {
  const retired = entry.retired_at !== null;
  return (
    <tr className={retired ? "gr-retired" : undefined} data-profile={entry.profile} data-effort={entry.effort}>
      <td className="gr-profile">
        {entry.profile}
        {retired ? <span className="gr-badge gr-badge-retired">retired {formatDay(entry.retired_at ?? "")}</span> : null}
      </td>
      <td className="gr-effort">{entry.effort === "" ? "기본" : entry.effort}</td>
      <td className="gr-model">{entry.model_id}</td>
      <td className="gr-pool">{entry.pool}</td>
      <td className="gr-score">{entry.score === null ? <span className="gr-missing">미측정</span> : entry.score.toFixed(1)}</td>
      <GateCell entry={entry} />
      <td className="gr-bench">
        {isEstimated(entry) ? <span className="gr-badge gr-badge-est">추정</span> : null}
        {entry.benchmark_source ?? <span className="gr-missing">출처 없음</span>}
        {entry.benchmark_annotation !== null ? <span className="gr-annotation">{entry.benchmark_annotation}</span> : null}
      </td>
      <td className="gr-decided">
        {entry.decided_by} · {formatDay(entry.decided_at)}
        <span className="gr-ref">
          {entry.deviation_ref === "" ? <span className="gr-missing">ref 미상</span> : entry.deviation_ref}
          {entry.boundary_version === "" ? null : <span className="gr-bv"> · {entry.boundary_version}</span>}
        </span>
      </td>
    </tr>
  );
}

export type LoadCatalog = (query: CatalogQuery) => Promise<CatalogResponse>;

/** Row-level wire check: every field the table reads must be present with the
 * type the Go struct emits — required fields as strings, nullable fields as
 * null-or-typed. A row missing a key is a partial payload: fail loudly into
 * the error state instead of rendering undefined or throwing mid-render. */
const ROW_STRING_KEYS = ["profile", "effort", "model_id", "pool", "grade", "gate", "boundary_version", "deviation_ref", "decided_at", "decided_by"] as const;
const ROW_NULLABLE_STRING_KEYS = ["gate_reason", "benchmark_source", "benchmark_annotation", "retired_at"] as const;

function validRow(row: unknown): boolean {
  if (row === null || typeof row !== "object") {
    return false;
  }
  const r = row as Record<string, unknown>;
  for (const k of ROW_STRING_KEYS) {
    if (typeof r[k] !== "string") {
      return false;
    }
  }
  for (const k of ROW_NULLABLE_STRING_KEYS) {
    if (r[k] !== null && typeof r[k] !== "string") {
      return false;
    }
  }
  return r.score === null || typeof r.score === "number";
}

/** A loaded catalog is bound to the query that produced it — rows from one
 * filter selection are never shown under another. */
const keyOf = (q: CatalogQuery) => `${q.pool ?? ""}\u0001${q.includeRetired ? "1" : "0"}`;

/** The grade table owns its fetch lifecycle like LiveQueue: first-load
 * failure is an explicit error, a later refetch failure keeps the last good
 * table under a warning banner — a stale table is never dressed as fresh.
 * Last-good rows are only reused for a refresh of the *same* query; a failed
 * fetch for a changed pool/retired selection is an error, not someone else's
 * ladder. */
export function GradesApp({ load = fetchCatalog }: { load?: LoadCatalog }) {
  const [pool, setPool] = useState("");
  const [includeRetired, setIncludeRetired] = useState(false);
  const [pools, setPools] = useState<string[]>([]);
  const [data, setData] = useState<{ key: string; body: CatalogResponse } | null>(null);
  const [error, setError] = useState<{ key: string; message: string } | null>(null);
  const [refreshFailed, setRefreshFailed] = useState(false);
  const goodKey = useRef<string | null>(null);
  const seq = useRef(0);

  const reload = useCallback(
    async (query: CatalogQuery) => {
      const mine = ++seq.current;
      try {
        const next = await load(query);
        if (mine !== seq.current) {
          return;
        }
        // A payload without a catalog array is a failure, never an empty
        // table — regardless of which loader produced it. Rows missing the
        // fields this screen reads are a partial payload: same treatment.
        if (next === null || typeof next !== "object" || !Array.isArray(next.catalog)) {
          throw new Error("catalog response has no catalog array");
        }
        if (!next.catalog.every(validRow)) {
          throw new Error("catalog row is missing required fields");
        }
        goodKey.current = keyOf(query);
        setData({ key: keyOf(query), body: next });
        setError(null);
        setRefreshFailed(false);
        // Pool choices always come from the unfiltered catalog — a filtered
        // ladder drops consult_only rows, so pools reachable only through
        // them must stay selectable.
        if (!query.pool) {
          setPools([...new Set(next.catalog.map((row) => row.pool))].sort());
        }
      } catch (err) {
        if (mine !== seq.current) {
          return;
        }
        const message = err instanceof Error ? err.message : String(err);
        if (goodKey.current === keyOf(query)) {
          setRefreshFailed(true);
        } else {
          setError({ key: keyOf(query), message });
        }
      }
    },
    [load],
  );

  useEffect(() => {
    void reload({ pool, includeRetired });
  }, [pool, includeRetired, reload]);

  const currentKey = keyOf({ pool, includeRetired });
  const showing = data !== null && data.key === currentKey ? data.body : null;
  const groups = useMemo(() => groupByGrade(showing?.catalog ?? []), [showing]);
  const clock = showing === null ? null : formatClock(showing.generated_at);
  const shownError = error !== null && error.key === currentKey ? error.message : null;

  return (
    <div className="gr-root">
      <header className="gr-head">
        <h1>급표</h1>
        <p className="gr-sub">
          bench_catalog — 프로필·effort 별 배정된 모델과 게이트. 읽기 전용이며 변경은 운영자 PUT 경로만 사용합니다.
        </p>
        <div className="gr-controls">
          <label className="gr-field">
            풀
            <select aria-label="pool filter" value={pool} onChange={(e) => setPool(e.target.value)}>
              <option value="">전체</option>
              {pools.map((p) => (
                <option key={p} value={p}>
                  {p}
                </option>
              ))}
            </select>
          </label>
          <label className="gr-field gr-check">
            <input type="checkbox" checked={includeRetired} onChange={(e) => setIncludeRetired(e.target.checked)} />
            retired 행도 보기
          </label>
          <button type="button" className="hk-btn" onClick={() => void reload({ pool, includeRetired })}>
            새로고침
          </button>
          <span className="gr-status" role="status" data-testid="catalog-status">
            {clock === null ? "확인 시각 알 수 없음" : `${clock} 확인 자료`}
            {refreshFailed && showing !== null ? <span className="gr-warn"> ⚠ 갱신 실패 — 마지막으로 확인한 자료를 보여 주는 중</span> : null}
          </span>
        </div>
        {pool !== "" ? (
          <p className="gr-note" data-testid="pool-note">
            풀 보기는 그 구독의 사다리입니다 — consult_only(자문 전용) 행은 이 보기에서 제외됩니다.
          </p>
        ) : null}
      </header>
      {shownError !== null ? (
        <p className="gr-error" role="alert">
          급표를 불러오지 못했습니다 (catalog unavailable) — {shownError}
        </p>
      ) : showing === null ? (
        <p className="gr-loading">급표를 불러오는 중…</p>
      ) : groups.length === 0 ? (
        <div className="gr-empty" role="status">
          <p>{pool === "" ? "카탈로그가 비어 있습니다 — 등록된 급 배정이 없습니다." : `풀 ${pool} 에 표시할 배정이 없습니다.`}</p>
          {includeRetired ? null : <p className="gr-note">retired 행이 있을 수 있습니다 — &quot;retired 행도 보기&quot;로 확인할 수 있습니다.</p>}
        </div>
      ) : (
        groups.map((group) => (
          <section key={group.grade} className="gr-group" data-grade={group.grade}>
            <h2 className="gr-group-head">
              <span className="gr-grade">{GRADE_ORDER.includes(group.grade) ? group.grade : `기타 (${group.grade})`}</span>
              <span className="gr-count">{group.rows.length}행</span>
            </h2>
            <table className="gr-table">
              <thead>
                <tr>
                  <th>프로필</th>
                  <th>effort</th>
                  <th>모델</th>
                  <th>풀</th>
                  <th>점수</th>
                  <th>게이트</th>
                  <th>근거</th>
                  <th>결정</th>
                </tr>
              </thead>
              <tbody>
                {group.rows.map((row) => (
                  <Row key={`${row.profile} ${row.effort}`} entry={row} />
                ))}
              </tbody>
            </table>
          </section>
        ))
      )}
    </div>
  );
}
