// hk 2280 home dashboard — one read-only page with quota, hosts, running jobs,
// the PR pipeline, and the sources hk does not record. Two independent polls:
// /ui/api/dashboard (quota, tasks, PR refs) and /ui/api/live (hub hosts and
// jobs). A failed refresh keeps the last good payload and says so; a section
// that never loaded says it failed — never an empty "all clear".

import { useEffect, useState, type ReactNode } from "react";
import { fetchLive } from "../../board/api";
import {
  liveAgeLabel,
  liveRequestFailed,
  liveSectionLabel,
  loadLabel,
  memoryLabel,
  pingAgeLabel,
  taskIdForJob,
  type LiveJob,
  type LiveResponse,
} from "../../live";
import { StateIcon, stateLabel } from "../StateIcon";
import { fetchDashboard } from "./api";
import type { DashboardAccount, DashboardBucket, DashboardMissing, DashboardRepo, DashboardResponse, DashboardTask } from "./types";

export const DASHBOARD_POLL_MS = 60_000;

/** Polls one read on a fixed cadence. The next request is scheduled only after
 * the in-flight one settles, so requests never overlap. */
export function usePoll<T>(load: () => Promise<T>, pollMs: number) {
  const [data, setData] = useState<T | null>(null);
  const [failed, setFailed] = useState(false);
  const [at, setAt] = useState<number | null>(null);
  useEffect(() => {
    let cancelled = false;
    let timer = 0;
    const run = async () => {
      try {
        const next = await load();
        if (!cancelled) {
          setData(next);
          setFailed(false);
          setAt(Date.now());
        }
      } catch {
        if (!cancelled) {
          setFailed(true);
        }
      } finally {
        if (!cancelled) {
          timer = window.setTimeout(() => void run(), pollMs);
        }
      }
    };
    void run();
    return () => {
      cancelled = true;
      window.clearTimeout(timer);
    };
  }, [load, pollMs]);
  return { data, failed, at };
}

/** "2시간 5분 후" / "3분 전" relative to now; null for absent timestamps. */
export function relativeLabel(nowMs: number, iso: string | undefined | null): string | null {
  if (!iso) {
    return null;
  }
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) {
    return null;
  }
  const delta = t - nowMs;
  const abs = Math.abs(delta);
  let text: string;
  if (abs < 60_000) {
    return delta >= 0 ? "곧" : "방금";
  } else if (abs < 3_600_000) {
    text = `${Math.floor(abs / 60_000)}분`;
  } else if (abs < 86_400_000) {
    const h = Math.floor(abs / 3_600_000);
    const m = Math.floor((abs % 3_600_000) / 60_000);
    text = m > 0 ? `${h}시간 ${m}분` : `${h}시간`;
  } else {
    const d = Math.floor(abs / 86_400_000);
    const h = Math.floor((abs % 86_400_000) / 3_600_000);
    text = h > 0 ? `${d}일 ${h}시간` : `${d}일`;
  }
  return delta >= 0 ? `${text} 후` : `${text} 전`;
}

/** Pace class: over 1.0 burns faster than the window refills. */
export function paceTone(pace: number | null): "" | "warn" | "danger" {
  if (pace === null) {
    return "";
  }
  if (pace >= 1.25) {
    return "danger";
  }
  return pace > 1 ? "warn" : "";
}

function Card({ title, aside, children, wide }: { title: string; aside?: ReactNode; children: ReactNode; wide?: boolean }) {
  return (
    <section className={`hd-card${wide ? " hd-wide" : ""}`} aria-label={title}>
      <header className="hd-card-head">
        <h2>{title}</h2>
        {aside ? <span className="hd-aside">{aside}</span> : null}
      </header>
      {children}
    </section>
  );
}

function Note({ children, tone }: { children: ReactNode; tone?: "warn" | "danger" }) {
  return <p className={`hd-note${tone ? ` hd-${tone}` : ""}`}>{children}</p>;
}

function UsedBar({ pct, elapsed }: { pct: number; elapsed: number | null }) {
  const clamped = Math.max(0, Math.min(100, pct));
  return (
    <span className="hd-bar" aria-hidden="true">
      <span className="hd-bar-fill" style={{ width: `${clamped}%` }} />
      {elapsed !== null ? <span className="hd-bar-mark" style={{ left: `${Math.max(0, Math.min(100, elapsed))}%` }} /> : null}
    </span>
  );
}

function BucketRow({ bucket, now }: { bucket: DashboardBucket; now: number }) {
  const tone = paceTone(bucket.pace);
  const reset = relativeLabel(now, bucket.resets_at);
  return (
    <tr>
      <td className="hd-mono">{bucket.window}</td>
      <td className="hd-muted">{bucket.scope === "model" ? bucket.model || "model" : "account"}</td>
      <td className="hd-num">
        {bucket.used_pct.toFixed(0)}%
        <UsedBar pct={bucket.used_pct} elapsed={bucket.elapsed_pct} />
      </td>
      <td className={`hd-num${tone ? ` hd-${tone}` : ""}`}>
        {bucket.pace === null ? <span className="hd-muted">—</span> : `×${bucket.pace.toFixed(2)}`}
      </td>
      <td title={bucket.resets_at ?? ""}>
        {bucket.reset_passed ? <span className="hd-warn">리셋 지남</span> : (reset ?? <span className="hd-muted">미상</span>)}
      </td>
    </tr>
  );
}

function QuotaAccount({ account, now }: { account: DashboardAccount; now: number }) {
  const fingerprint = account.account_key.slice(account.pool.length + 1);
  const observed = account.auto ? (account.auto.confirmed_at ?? account.auto.measured_at) : null;
  const age = relativeLabel(now, observed);
  const stale = observed !== null && now - Date.parse(observed) > 30 * 60_000;
  return (
    <div className="hd-account">
      <div className="hd-account-head">
        <strong>{account.pool}</strong>
        <span className="hd-mono hd-muted" title={account.account_key}>
          {fingerprint.slice(0, 6)}
        </span>
        {account.auto?.plan ? <span className="hd-chip">{account.auto.plan}</span> : null}
        <span className={`hd-meta${stale ? " hd-warn" : ""}`}>{age ? `측정 ${age}` : "자동 측정 없음"}</span>
      </div>
      {account.auto && account.auto.buckets.length > 0 ? (
        <table className="hd-table">
          <thead>
            <tr>
              <th>창</th>
              <th>범위</th>
              <th>사용</th>
              <th title="사용률 ÷ 경과율 (1.0이면 리셋 시점에 100%)">페이스</th>
              <th>리셋</th>
            </tr>
          </thead>
          <tbody>
            {account.auto.buckets.map((bucket, i) => (
              <BucketRow key={`${bucket.window}-${bucket.scope}-${bucket.model ?? ""}-${i}`} bucket={bucket} now={now} />
            ))}
          </tbody>
        </table>
      ) : null}
      {account.manual.length > 0 ? (
        <ul className="hd-list hd-manual">
          {account.manual.map((m) => (
            <li key={m.window} className={m.expired ? "hd-muted" : undefined}>
              <span className="hd-chip">수동</span>
              <span className="hd-mono">{m.window}</span>
              <span>{m.used_pct === null ? "값 없음" : `${m.used_pct.toFixed(0)}%`}</span>
              <span className="hd-meta">{m.expired ? "만료됨" : `만료 ${relativeLabel(now, m.expires_at) ?? "미상"}`}</span>
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}

function QuotaCard({ data, now }: { data: DashboardResponse; now: number }) {
  return (
    <Card title="쿼터" aside={`${data.quota.length}개 계정`} wide>
      {data.quota.length === 0 ? <Note>hk 공유 쿼터 저장소에 기록된 계정이 없습니다.</Note> : null}
      <div className="hd-accounts">
        {data.quota.map((account) => (
          <QuotaAccount key={account.account_key} account={account} now={now} />
        ))}
      </div>
      {data.quota_truncated ? <Note tone="warn">쿼터 행이 많아 일부만 표시합니다.</Note> : null}
    </Card>
  );
}

function SectionStatus({ status, fetchedAt, now }: { status: LiveResponse["nodes"]["status"]; fetchedAt: string; now: number }) {
  if (status === "ok") {
    return null;
  }
  const age = relativeLabel(now, fetchedAt);
  return (
    <Note tone="warn">
      {liveSectionLabel(status)}
      {age ? ` — ${age} 데이터 표시 중` : " — 표시할 데이터 없음"}
    </Note>
  );
}

function HostsCard({ live, now }: { live: LiveResponse | null; now: number }) {
  if (live === null) {
    return (
      <Card title="호스트">
        <Note>불러오는 중…</Note>
      </Card>
    );
  }
  const nodes = live.nodes;
  return (
    <Card title="호스트" aside={`${nodes.items.length}대`}>
      <SectionStatus status={nodes.status} fetchedAt={nodes.fetched_at} now={now} />
      {nodes.items.length > 0 ? (
        <table className="hd-table">
          <thead>
            <tr>
              <th>호스트</th>
              <th>상태</th>
              <th>마지막 핑</th>
              <th>부하</th>
              <th>메모리</th>
              <th>작업</th>
            </tr>
          </thead>
          <tbody>
            {nodes.items.map((node) => (
              <tr key={node.machine_id}>
                <td className="hd-mono">{node.machine_id}</td>
                <td>
                  {node.state}
                  {node.accepting_effective ? null : <span className="hd-chip hd-warn">수용 중지</span>}
                </td>
                <td>{pingAgeLabel(node.last_ping_ms)}</td>
                <td>{loadLabel(node.load)}</td>
                <td>{memoryLabel(node.memory)}</td>
                <td className="hd-num">{node.active_jobs === null ? <span className="hd-muted">미상</span> : node.active_jobs}</td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : nodes.status === "ok" ? (
        <Note>허브에 등록된 호스트가 없습니다.</Note>
      ) : null}
    </Card>
  );
}

function TaskLink({ task }: { task: { id: number; title: string } }) {
  return (
    <a href={`/ui/tasks/${task.id}`} className="hd-task">
      <span className="hd-mono">#{task.id}</span> {task.title}
    </a>
  );
}

function JobsCard({ live, data, now }: { live: LiveResponse | null; data: DashboardResponse | null; now: number }) {
  const work = data?.work ?? [];
  const tasksById = new Map<number, DashboardTask>(work.map((task) => [task.id, task]));
  const jobs: LiveJob[] = live ? [...live.jobs.items] : [];
  jobs.sort((a, b) => a.machine.localeCompare(b.machine) || (a.owner_lane ?? "").localeCompare(b.owner_lane ?? "") || a.job_id.localeCompare(b.job_id));
  const liveJobIds = new Set(jobs.map((job) => job.job_id));
  // Active tasks the hub does not list a job for: no refs.job_id, or a job_id
  // the hub no longer reports. Only meaningful once the jobs list has loaded.
  const jobsKnown = live !== null && live.jobs.fetched_at !== "";
  const orphaned = jobsKnown ? work.filter((task) => !task.job_id || !liveJobIds.has(task.job_id)) : [];
  return (
    <Card title="실행 중 작업" aside={live ? `잡 ${jobs.length} · 태스크 ${work.length}` : undefined} wide>
      {live === null ? <Note>불러오는 중…</Note> : <SectionStatus status={live.jobs.status} fetchedAt={live.jobs.fetched_at} now={now} />}
      {jobs.length > 0 ? (
        <table className="hd-table">
          <thead>
            <tr>
              <th>호스트</th>
              <th>레인</th>
              <th>역할</th>
              <th>모델</th>
              <th>태스크</th>
              <th>시작</th>
            </tr>
          </thead>
          <tbody>
            {jobs.map((job) => {
              const taskId = live ? taskIdForJob(live, job.job_id) : null;
              const task = taskId === null ? undefined : tasksById.get(taskId);
              return (
                <tr key={`${job.machine}-${job.job_id}`}>
                  <td className="hd-mono">{job.machine}</td>
                  <td className="hd-mono">{job.owner_lane || <span className="hd-muted">—</span>}</td>
                  <td>{job.role || <span className="hd-muted">—</span>}</td>
                  <td>{job.tier || task?.tier || <span className="hd-muted">미수집</span>}</td>
                  <td className="hd-ellipsis">
                    {task ? <TaskLink task={task} /> : taskId !== null ? <a href={`/ui/tasks/${taskId}`}>#{taskId}</a> : <span className="hd-muted hd-mono">{job.job_id}</span>}
                  </td>
                  <td>{liveAgeLabel(new Date(now).toISOString(), job.started_at) ?? <span className="hd-muted">미상</span>}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      ) : live !== null && live.jobs.status === "ok" ? (
        <Note>허브에 실행 중인 잡이 없습니다.</Note>
      ) : null}
      {orphaned.length > 0 ? (
        <>
          <h3 className="hd-subhead">허브 잡이 없는 활성 태스크</h3>
          <ul className="hd-list">
            {orphaned.map((task) => (
              <li key={task.id}>
                <StateIcon state={task.state} />
                <span className="hd-meta">{stateLabel(task.state)}</span>
                <TaskLink task={task} />
                <span className="hd-meta hd-mono">{task.lane}</span>
                <span className="hd-meta">{task.job_id ? `job ${task.job_id} 없음` : "job_id 미기록"}</span>
              </li>
            ))}
          </ul>
        </>
      ) : null}
      {data?.tasks_truncated ? <Note tone="warn">태스크가 많아 최근 일부만 표시합니다.</Note> : null}
    </Card>
  );
}

function PRRow({ pr, now, merged }: { pr: DashboardRepo["open"][number]; now: number; merged?: boolean }) {
  return (
    <li>
      {merged ? null : <StateIcon state={pr.state} />}
      {/* The server sets number only for an exact https://github.com/<owner>/<repo>/pull/<n>
          ref; any other refs.pr value is shown as text, never as a link. */}
      {pr.number ? (
        <a href={pr.pr} className="hd-mono" rel="noreferrer" target="_blank">
          #{pr.number}
        </a>
      ) : (
        <span className="hd-mono">{pr.pr}</span>
      )}
      <TaskLink task={{ id: pr.task_id, title: pr.title }} />
      <span className="hd-meta">{merged ? `머지 ${relativeLabel(now, pr.merged_at) ?? ""}` : stateLabel(pr.state)}</span>
      {pr.head_sha ? <span className="hd-meta hd-mono">{pr.head_sha.slice(0, 7)}</span> : null}
    </li>
  );
}

function PRCard({ data, now }: { data: DashboardResponse; now: number }) {
  const open = data.prs.reduce((n, repo) => n + repo.open.length, 0);
  return (
    <Card title="PR 파이프라인" aside={`열림 ${open}`} wide>
      <Note>태스크의 refs.pr과 상태로 구성합니다. 리뷰·CI 상태는 hk에 기록되지 않습니다.</Note>
      {data.prs.map((repo) => (
        <div key={repo.repo || "-"} className="hd-repo">
          <div className="hd-account-head">
            <strong className="hd-mono">{repo.repo || "인식 못한 refs.pr"}</strong>
            <span className="hd-meta">
              열림 {repo.open.length} · 7일 머지 {repo.merged.length}
            </span>
          </div>
          {repo.open.length > 0 ? (
            <ul className="hd-list">
              {repo.open.map((pr) => (
                <PRRow key={`${pr.task_id}-${pr.pr}`} pr={pr} now={now} />
              ))}
            </ul>
          ) : null}
          {repo.merged.length > 0 ? (
            <details className="hd-merged">
              <summary>최근 머지 {repo.merged.length}</summary>
              <ul className="hd-list">
                {repo.merged.map((pr) => (
                  <PRRow key={`${pr.task_id}-${pr.pr}`} pr={pr} now={now} merged />
                ))}
              </ul>
            </details>
          ) : null}
        </div>
      ))}
    </Card>
  );
}

function MissingCard({ missing }: { missing: DashboardMissing[] }) {
  const stops = missing.find((m) => m.section === "quota_stops");
  return (
    <Card title="쿼터 정지 결정">
      <Note>{stops ? `hk에 기록 없음 — ${stops.note}` : "hk에 기록 없음"}</Note>
      <h3 className="hd-subhead">hk에 없는 데이터 소스</h3>
      <ul className="hd-list hd-missing">
        {missing.map((m) => (
          <li key={`${m.section}-${m.source}`}>
            <strong>{m.source}</strong>
            <span className="hd-meta">{m.note}</span>
          </li>
        ))}
      </ul>
    </Card>
  );
}

export function DashboardApp({
  loadDashboard = fetchDashboard,
  loadLive = fetchLive,
  pollMs = DASHBOARD_POLL_MS,
}: {
  loadDashboard?: () => Promise<DashboardResponse>;
  loadLive?: () => Promise<LiveResponse>;
  pollMs?: number;
}) {
  const dashboard = usePoll(loadDashboard, pollMs);
  const live = usePoll(loadLive, pollMs);
  const now = Math.max(dashboard.at ?? 0, live.at ?? 0) || Date.now();
  const updated = dashboard.at ? new Date(dashboard.at).toLocaleTimeString() : null;
  return (
    <div className="hd-root">
      <header className="hd-head">
        <h1>홈 — Home</h1>
        <p className="hd-sub">
          읽기 전용 · 60초마다 갱신{updated ? ` · 마지막 갱신 ${updated}` : ""}
          {dashboard.failed || live.failed ? <span className="hd-warn"> · 갱신 실패, 마지막 데이터 표시 중</span> : null}
        </p>
      </header>
      {dashboard.data === null ? (
        dashboard.failed ? (
          <Note tone="danger">대시보드를 불러오지 못했습니다. 60초 후 다시 시도합니다.</Note>
        ) : (
          <Note>불러오는 중…</Note>
        )
      ) : null}
      <div className="hd-grid">
        {dashboard.data ? <QuotaCard data={dashboard.data} now={now} /> : null}
        <HostsCard live={live.data ?? (live.failed ? liveRequestFailed() : null)} now={now} />
        <JobsCard live={live.data ?? (live.failed ? liveRequestFailed() : null)} data={dashboard.data} now={now} />
        {dashboard.data ? <PRCard data={dashboard.data} now={now} /> : null}
        {dashboard.data ? <MissingCard missing={dashboard.data.missing} /> : null}
      </div>
    </div>
  );
}
