import { useCallback, useMemo } from "react";
import { usePoll } from "../shared/usePoll";
import { formatAge, formatMinuteUTC } from "../shared/time";
import { Mono } from "../shared/ui/Mono";
import { loadFleetStatus } from "./api";
import type {
  FleetStatusPlan,
  FleetStatusQuota,
  FleetStatusSnapshot,
} from "./types";
import "./fleetStatus.css";

// Korean labels for the contract's vocabulary; the raw code stays in title so
// the page and `panewire plan` output name the same thing.
const OUTCOME_LABEL: Record<string, string> = {
  ADMITTED: "배정됨",
  NO_ELIGIBLE_HOST: "가능한 호스트 없음",
  QUOTA_EXHAUSTED: "쿼터 소진",
  QUOTA_UNKNOWN: "쿼터 불명",
  AUTH_UNAVAILABLE: "인증 사용 불가",
  POLICY_UNAVAILABLE: "정책 사용 불가",
  NO_ROLE_CANDIDATE: "역할 후보 없음",
};

const FREE_STATE_LABEL: Record<string, string> = {
  available: "무료 여유",
  exhausted: "무료 소진",
  unknown: "무료 불명",
};

function outcomeLabel(outcome: string): string {
  return OUTCOME_LABEL[outcome] ?? outcome;
}

function freeStateLabel(state: string): string {
  return FREE_STATE_LABEL[state] ?? state;
}

/** Seconds between the snapshot's assembly time and now; null when unparsable. */
function snapshotAgeSec(at: string): number | null {
  const t = Date.parse(at);
  return Number.isFinite(t) ? Math.max(0, (Date.now() - t) / 1000) : null;
}

function formatPct(pct: number | null): string {
  if (pct === null) return "—";
  return `${Number.isInteger(pct) ? pct : pct.toFixed(1)}%`;
}

function formatInstant(iso: string | undefined): string {
  if (!iso) return "";
  return formatMinuteUTC(iso) ?? iso;
}

/** The selection band each (host, account) carries in an admitted plan row —
 * the contract's answer to "which pool is in the waste band": a quota row has
 * no band of its own, the plan row for the grade is the trusted source. */
function planTiers(snapshot: FleetStatusSnapshot): Map<string, Set<string>> {
  const tiers = new Map<string, Set<string>>();
  for (const plan of snapshot.plan) {
    if (!plan.host || !plan.account || !plan.tier) continue;
    const key = `${plan.host}\n${plan.account}`;
    const set = tiers.get(key) ?? new Set<string>();
    set.add(plan.tier);
    tiers.set(key, set);
  }
  return tiers;
}

function HostsSection({ snapshot }: { snapshot: FleetStatusSnapshot }) {
  return (
    <section className="fs-section" aria-label="호스트">
      <div className="fs-section-header">
        <h2 className="fs-section-title">호스트</h2>
        <span className="fs-section-note">연결 · 스폰 수락 · 레인 사용량</span>
      </div>
      {snapshot.hosts.length === 0 ? (
        <p className="fs-empty">기록된 호스트가 없습니다.</p>
      ) : (
        <div className="fs-host-grid">
          {snapshot.hosts.map((host) => (
            <div className="fs-card fs-host" key={host.host}>
              <div className="fs-host-head">
                <Mono className="fs-host-name">{host.host}</Mono>
                <span className={`hk-chip ${host.connected ? "hk-chip--done" : "hk-chip--neutral"}`}>
                  {host.connected ? "연결됨" : "연결 끊김"}
                </span>
                <span
                  className={`hk-chip ${host.accepting ? "hk-chip--done" : "hk-chip--input"}`}
                  title={host.accepting ? undefined : "이 호스트는 새 스폰을 받지 않습니다"}
                >
                  {host.accepting ? "스폰 수락" : "스폰 안 받음"}
                </span>
              </div>
              <div className="fs-host-meta">
                <span>
                  레인 <Mono>{host.lanes_used}</Mono>/<Mono>{host.lanes_cap}</Mono>
                </span>
                {host.version ? <span className="fs-host-version">{host.version}</span> : null}
              </div>
            </div>
          ))}
        </div>
      )}
    </section>
  );
}

function QuotaRow({ row, snapAge, tiers }: { row: FleetStatusQuota; snapAge: number; tiers?: Set<string> }) {
  const age = row.age_s + snapAge;
  return (
    <article className="fs-quota" data-stale={!row.fresh || undefined}>
      <header className="fs-quota-head">
        <span className="fs-pool">{row.pool}</span>
        <Mono className="fs-account">{row.account}</Mono>
        <span className="fs-quota-chips">
          {row.free_state ? (
            <span
              className={`hk-chip ${row.free_state === "available" ? "hk-chip--done" : row.free_state === "exhausted" ? "hk-chip--input" : "hk-chip--neutral"}`}
            >
              {freeStateLabel(row.free_state)}
            </span>
          ) : null}
          {tiers
            ? [...tiers].map((tier) => (
                <span key={tier} className="hk-chip hk-chip--working" title="지금 배정된다면 선택되는 등급">
                  {tier}
                </span>
              ))
            : null}
          <span
            className={`hk-chip ${row.fresh ? "hk-chip--done" : "hk-chip--input"}`}
            title={`신선도 한도 ${formatAge(row.fresh_ttl_s)}`}
          >
            {row.fresh ? "최신" : "오래됨"}
          </span>
        </span>
      </header>
      {row.windows.length > 0 ? (
        <ul className="fs-windows">
          {row.windows.map((w) => (
            <li key={w.window} className="fs-window">
              <span className="fs-window-name">
                <Mono>{w.window}</Mono>
              </span>
              <span className="fs-window-pct">
                <Mono>{formatPct(w.used_pct)}</Mono>
              </span>
              {w.resets_at ? <span className="fs-window-reset">초기화 {formatInstant(w.resets_at)}</span> : null}
            </li>
          ))}
        </ul>
      ) : null}
      <footer className="fs-quota-meta">
        측정 <time dateTime={row.measured_at}>{formatInstant(row.measured_at)}</time> · 경과 {formatAge(age)}
      </footer>
    </article>
  );
}

function QuotaSection({ snapshot, snapAge }: { snapshot: FleetStatusSnapshot; snapAge: number }) {
  const byHost = useMemo(() => {
    const groups = new Map<string, FleetStatusQuota[]>();
    for (const row of snapshot.quota) {
      const list = groups.get(row.host) ?? [];
      list.push(row);
      groups.set(row.host, list);
    }
    return [...groups.entries()];
  }, [snapshot.quota]);
  const tiers = useMemo(() => planTiers(snapshot), [snapshot]);

  return (
    <section className="fs-section" aria-label="호스트별 쿼터">
      <div className="fs-section-header">
        <h2 className="fs-section-title">호스트별 쿼터</h2>
        <span className="fs-section-note">계정 · 윈도우 사용률 · 신선도 · 선택 등급</span>
      </div>
      {byHost.length === 0 ? (
        <p className="fs-empty">쿼터 읽기값이 없습니다.</p>
      ) : (
        byHost.map(([host, rows]) => (
          <div className="fs-quota-group" key={host}>
            <h3 className="fs-group-title">
              <Mono>{host}</Mono>
            </h3>
            {rows.map((row) => (
              <QuotaRow
                key={`${row.host}/${row.pool}/${row.account}`}
                row={row}
                snapAge={snapAge}
                tiers={tiers.get(`${row.host}\n${row.account}`)}
              />
            ))}
          </div>
        ))
      )}
    </section>
  );
}

function planDetail(plan: FleetStatusPlan): string {
  return [plan.seat, plan.pool, plan.host, plan.account, plan.tier]
    .filter((part): part is string => typeof part === "string" && part !== "")
    .join(" · ");
}

function PlanSection({ snapshot }: { snapshot: FleetStatusSnapshot }) {
  return (
    <section className="fs-section" aria-label="지금 배정된다면">
      <div className="fs-section-header">
        <h2 className="fs-section-title">지금 배정된다면</h2>
        <span className="fs-section-note">각 등급의 {snapshot.plan[0]?.t ?? "T1"} 구현 작업을 지금 보냈을 때의 드라이런 — 아무것도 바꾸지 않습니다</span>
      </div>
      {snapshot.plan.length === 0 ? (
        <p className="fs-empty">배정 계획이 없습니다.</p>
      ) : (
        <div className="fs-plan-list">
          {snapshot.plan.map((plan) => {
            const admitted = plan.outcome === "ADMITTED";
            const detail = planDetail(plan);
            return (
              <div className="fs-plan" key={plan.grade}>
                <span className="fs-grade">
                  <Mono>{plan.grade}</Mono>
                </span>
                <span
                  className={`hk-chip ${admitted ? "hk-chip--done" : "hk-chip--input"}`}
                  title={plan.outcome}
                >
                  {outcomeLabel(plan.outcome)}
                </span>
                <span className="fs-plan-detail">
                  {detail ? <Mono>{detail}</Mono> : <span className="fs-plan-none">—</span>}
                </span>
              </div>
            );
          })}
        </div>
      )}
    </section>
  );
}

function SnapshotView({ snapshot, updatedAt }: { snapshot: FleetStatusSnapshot; updatedAt?: string }) {
  const snapAge = snapshotAgeSec(snapshot.at) ?? 0;
  return (
    <div className="fs-root" role="region" aria-label="플릿 현황">
      <header className="fs-header">
        <h1 className="fs-title">플릿 현황</h1>
        <div className="fs-time">
          스냅샷 <time dateTime={snapshot.at}>{formatInstant(snapshot.at)}</time> · 경과 {formatAge(snapAge)}
          {updatedAt ? (
            <>
              {" "}
              · 기록 <time dateTime={updatedAt}>{formatInstant(updatedAt)}</time>
            </>
          ) : null}
        </div>
      </header>
      <HostsSection snapshot={snapshot} />
      <QuotaSection snapshot={snapshot} snapAge={snapAge} />
      <PlanSection snapshot={snapshot} />
    </div>
  );
}

export function FleetStatusPage() {
  const load = useCallback(() => loadFleetStatus(), []);
  const { data, failed } = usePoll(load, 60_000);

  if (failed && !data) {
    return (
      <div className="fs-root" role="region" aria-label="플릿 현황">
        <h1 className="fs-title">플릿 현황</h1>
        <p className="fs-empty" role="alert">
          플릿 현황 데이터를 불러오지 못했습니다. 잠시 후 다시 시도합니다.
        </p>
      </div>
    );
  }

  if (!data) {
    return (
      <div className="fs-root" role="region" aria-label="플릿 현황">
        <h1 className="fs-title">플릿 현황</h1>
        <p className="fs-empty" role="status">
          플릿 현황 불러오는 중…
        </p>
      </div>
    );
  }

  if (!data.snapshot) {
    return (
      <div className="fs-root" role="region" aria-label="플릿 현황">
        <header className="fs-header">
          <h1 className="fs-title">플릿 현황</h1>
        </header>
        <p className="fs-empty" role="status">
          아직 플릿 스냅샷이 없습니다. panewire hub가 주기적으로 기록하기 시작하면 호스트, 쿼터와 배정 계획이 여기에 표시됩니다.
        </p>
      </div>
    );
  }

  return <SnapshotView snapshot={data.snapshot} updatedAt={data.updated_at} />;
}
