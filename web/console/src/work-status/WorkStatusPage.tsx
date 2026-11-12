import { useCallback, useMemo, useState } from "react";
import { usePoll } from "../shared/usePoll";
import { loadWorkStatus } from "./api";
import { StateIcon } from "../shared/StateIcon";
import { Chip } from "../shared/ui/Chip";
import { Mono } from "../shared/ui/Mono";
import { formatMinuteUTC } from "../shared/time";
import { stateLabel } from "../shared/states";
import "./workStatus.css";

function formatDuration(seconds: number): string {
  if (seconds < 60) return `${Math.max(0, Math.floor(seconds))}초`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}분`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) {
    const rem = minutes % 60;
    return rem > 0 ? `${hours}시간 ${rem}분` : `${hours}시간`;
  }
  const days = Math.floor(hours / 24);
  const remHours = hours % 24;
  return remHours > 0 ? `${days}일 ${remHours}시간` : `${days}일`;
}

export function WorkStatusPage() {
  const load = useCallback(() => loadWorkStatus(), []);
  const { data, failed } = usePoll(load, 30_000);
  const [backlogProjectFilter, setBacklogProjectFilter] = useState<string>("all");

  const backlogProjects = useMemo(() => {
    if (!data) return [];
    const set = new Set<string>();
    for (const task of data.backlog) {
      set.add(task.project || "(미지정)");
    }
    return Array.from(set).sort();
  }, [data]);

  const filteredBacklog = useMemo(() => {
    if (!data) return [];
    if (backlogProjectFilter === "all") return data.backlog;
    return data.backlog.filter((t) => (t.project || "(미지정)") === backlogProjectFilter);
  }, [data, backlogProjectFilter]);

  if (failed && !data) {
    return (
      <div className="ws-root" role="region" aria-label="작업 현황">
        <h1 className="ws-title">작업 현황</h1>
        <p className="ws-empty" role="alert">
          작업 현황 데이터를 불러오지 못했습니다. 잠시 후 다시 시도합니다.
        </p>
      </div>
    );
  }

  if (!data) {
    return (
      <div className="ws-root" role="region" aria-label="작업 현황">
        <h1 className="ws-title">작업 현황</h1>
        <p className="ws-empty" role="status">
          작업 현황 불러오는 중…
        </p>
      </div>
    );
  }

  return (
    <div className="ws-root" role="region" aria-label="작업 현황">
      <header className="ws-header">
        <h1 className="ws-title">작업 현황</h1>
        <div className="ws-time">
          기준 시각: <time dateTime={data.generated_at}>{formatMinuteUTC(data.generated_at) ?? data.generated_at}</time>
        </div>
      </header>

      {/* KPI Summary Cards */}
      <div className="ws-summary-grid" role="region" aria-label="요약 지표">
        <div className="ws-card ws-card--highlight">
          <span className="ws-card-label">전체 열린 작업</span>
          <span className="ws-card-value">{data.summary.total_open}</span>
        </div>
        <div className="ws-card">
          <span className="ws-card-label">대기</span>
          <span className="ws-card-value">{data.summary.backlog}</span>
        </div>
        <div className="ws-card">
          <span className="ws-card-label">진행 중</span>
          <span className="ws-card-value">{data.summary.in_work}</span>
        </div>
        <div className="ws-card">
          <span className="ws-card-label">검토 중</span>
          <span className="ws-card-value">{data.summary.in_review}</span>
        </div>
        <div className="ws-card">
          <span className="ws-card-label">배포 대기</span>
          <span className="ws-card-value">{data.summary.waiting_deploy}</span>
        </div>
        <div className="ws-card">
          <span className="ws-card-label">머지됨 (배포 기록 없음)</span>
          <span className="ws-card-value">{data.summary.merged_no_deploy_record}</span>
        </div>
        <div className="ws-card">
          <span className="ws-card-label">번들 (작업 제외)</span>
          <span className="ws-card-value">{data.summary.bundle_parents}</span>
        </div>
      </div>

      {/* Movement Section */}
      <section className="ws-section" aria-label="이동 현황 (최근 7일 및 오늘)">
        <div className="ws-section-header">
          <h2 className="ws-section-title">작업 증감 (이동 현황)</h2>
          <span className="ws-section-note">KST 일별 생성 vs 완료/종료/배포</span>
        </div>
        <div className="ws-movement-grid">
          {data.movement.map((m) => {
            const net = m.created - m.closed;
            return (
              <div key={m.date} className="ws-movement-day">
                <span className="ws-movement-date">{m.date.slice(5)}</span>
                <div className="ws-movement-stats">
                  <span className="ws-movement-created" title={`생성 ${m.created}건`}>
                    +{m.created}
                  </span>
                  <span className="ws-movement-closed" title={`종료 ${m.closed}건`}>
                    -{m.closed}
                  </span>
                </div>
                <span
                  className="ws-movement-net"
                  style={{ color: net > 0 ? "var(--hk-coral)" : net < 0 ? "var(--hk-green)" : "var(--hk-text-muted)" }}
                  title="순 증감"
                >
                  {net > 0 ? `+${net}` : net}
                </span>
              </div>
            );
          })}
        </div>
      </section>

      {/* Project Counts Table */}
      <section className="ws-section" aria-label="프로젝트별 작업 건수">
        <div className="ws-section-header">
          <h2 className="ws-section-title">프로젝트별 작업 건수</h2>
          <span className="ws-section-note">배포 대기는 프로젝트별 최신 배포 이후 머지 건수만 반영</span>
        </div>
        <div className="ws-table-wrap">
          <table className="ws-table">
            <thead>
              <tr>
                <th>프로젝트</th>
                <th className="ws-num">대기</th>
                <th className="ws-num">진행 중</th>
                <th className="ws-num">검토 중</th>
                <th className="ws-num">배포 대기</th>
                <th className="ws-num">머지됨 (기록 없음)</th>
                <th className="ws-num ws-total-col">전체 열린 작업</th>
                <th className="ws-num">번들</th>
              </tr>
            </thead>
            <tbody>
              {data.projects.map((p) => (
                <tr key={p.project || "(미지정)"}>
                  <td>
                    {p.project ? (
                      <Chip kind="lane" lane={p.project} />
                    ) : (
                      <span className="ws-item-id">(미지정)</span>
                    )}
                  </td>
                  <td className="ws-num">{p.counts.backlog}</td>
                  <td className="ws-num">{p.counts.in_work}</td>
                  <td className="ws-num">{p.counts.in_review}</td>
                  <td className="ws-num">{p.counts.waiting_deploy}</td>
                  <td className="ws-num">{p.counts.merged_no_deploy_record}</td>
                  <td className="ws-num ws-total-col">{p.counts.total_open}</td>
                  <td className="ws-num">{p.counts.bundle_parents}</td>
                </tr>
              ))}
            </tbody>
            <tfoot>
              <tr style={{ background: "var(--hk-surface-2)", fontWeight: "var(--hk-weight-strong)" }}>
                <td>합계</td>
                <td className="ws-num">{data.summary.backlog}</td>
                <td className="ws-num">{data.summary.in_work}</td>
                <td className="ws-num">{data.summary.in_review}</td>
                <td className="ws-num">{data.summary.waiting_deploy}</td>
                <td className="ws-num">{data.summary.merged_no_deploy_record}</td>
                <td className="ws-num ws-total-col">{data.summary.total_open}</td>
                <td className="ws-num">{data.summary.bundle_parents}</td>
              </tr>
            </tfoot>
          </table>
        </div>
      </section>

      {/* In-Work List */}
      <section className="ws-section" aria-label="진행 중인 작업 목록">
        <div className="ws-section-header">
          <h2 className="ws-section-title">진행 중인 작업 ({data.in_work.length})</h2>
          <span className="ws-section-note">조사 중 · 개발 중 · 검토 중</span>
        </div>
        {data.in_work.length === 0 ? (
          <p className="ws-empty">진행 중인 작업이 없습니다.</p>
        ) : (
          <ul className="ws-list">
            {data.in_work.map((task) => (
              <li key={task.id} className="ws-item">
                <a className="ws-item-link" href={`/ui/tasks/${task.id}`}>
                  <StateIcon state={task.state} />
                  <span className="ws-item-id">
                    <Mono>#{task.id}</Mono>
                  </span>
                  <span className="ws-item-title">{task.title}</span>
                </a>
                <div className="ws-item-meta">
                  <Chip kind="status" state={task.state} label={stateLabel(task.state)} />
                  {task.project ? <Chip kind="lane" lane={task.project} /> : null}
                  {task.host || task.job_id ? (
                    <span className="hk-chip" title={task.job_id || task.host || undefined}>
                      {[task.host, task.job_id].filter(Boolean).join(" · ")}
                    </span>
                  ) : null}
                  {task.pr ? (
                    <a
                      href={task.pr}
                      target="_blank"
                      rel="noopener noreferrer"
                      className="hk-chip"
                      title={task.pr}
                    >
                      PR ↗
                    </a>
                  ) : null}
                  <time title={task.since ? formatMinuteUTC(task.since) ?? task.since : "시각 미상"}>
                    {formatDuration(task.age_seconds)}
                  </time>
                </div>
              </li>
            ))}
          </ul>
        )}
      </section>

      {/* Backlog List */}
      <section className="ws-section" aria-label="대기 목록">
        <div className="ws-section-header">
          <h2 className="ws-section-title">대기 목록 ({filteredBacklog.length})</h2>
          <div className="ws-filter-bar">
            <label htmlFor="ws-project-filter" className="ws-section-note">
              프로젝트 필터:
            </label>
            <select
              id="ws-project-filter"
              className="ws-select"
              value={backlogProjectFilter}
              onChange={(e) => setBacklogProjectFilter(e.target.value)}
            >
              <option value="all">전체 ({data.backlog.length})</option>
              {backlogProjects.map((p) => {
                const count = data.backlog.filter((t) => (t.project || "(미지정)") === p).length;
                return (
                  <option key={p} value={p}>
                    {p} ({count})
                  </option>
                );
              })}
            </select>
          </div>
        </div>
        {filteredBacklog.length === 0 ? (
          <p className="ws-empty">대기 중인 작업이 없습니다.</p>
        ) : (
          <ul className="ws-list">
            {filteredBacklog.map((task) => (
              <li key={task.id} className="ws-item">
                <a className="ws-item-link" href={`/ui/tasks/${task.id}`}>
                  <StateIcon state={task.state} />
                  <span className="ws-item-id">
                    <Mono>#{task.id}</Mono>
                  </span>
                  <span className="ws-item-title">{task.title}</span>
                </a>
                <div className="ws-item-meta">
                  {task.bundle_id && task.bundle_title ? (
                    <a
                      className="ws-bundle-tag"
                      href={`/ui/tasks/${task.bundle_id}`}
                      title={task.bundle_title}
                    >
                      번들 #{task.bundle_id}
                    </a>
                  ) : null}
                  {task.project ? <Chip kind="lane" lane={task.project} /> : null}
                  <Chip kind="priority" value={task.priority} urgent={task.priority <= 2} />
                  <time title={formatMinuteUTC(task.created_at) ?? task.created_at}>
                    {task.created_at.slice(0, 10)}
                  </time>
                </div>
              </li>
            ))}
          </ul>
        )}
      </section>
    </div>
  );
}
