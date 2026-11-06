// #529 — 서비스별 배포 상태 블록. It renders on the Deploys page
// (deploys/DeploysPage). Read-only: one GET to /ui/api/deploy-pending on
// mount and on the poll cadence; no writes, no notifications, no GitHub
// calls. The merged list heading stays "마지막 배포 이후 머지됨" — the
// surface must never claim "미배포 확정" (design doc §2 advisory), and a
// service without records renders "기록 없음", never "최신".
// Styles live in deploys.css (D1 tokens only); there are no inline styles.

import { useEffect, useState } from "react";
import { Icon } from "../shared/Icon";
import { Chip } from "../shared/ui/Chip";
import { CollapsedWarning } from "../shared/ui/CollapsedWarning";
import { MonoRef } from "../shared/ui/MonoRef";
import {
  deployElapsed,
  deployPRLabel,
  deployStamp,
  fetchDeployPending,
  shortDeployedRef,
  type DeployPendingResponse,
  type DeployRecordView,
  type DeployServiceView,
} from "./deploy";
import { useDeployPending } from "./deployPoll";

/** Attempts shown before "더 보기"; the server sends up to ten. */
const RECENT_VISIBLE = 3;
/** Malformed-record keys listed before "더 보기". */
const INVALID_VISIBLE = 20;

const RESULT_LABEL: Record<string, string> = {
  success: "성공",
  failed: "실패",
  rolled_back: "롤백",
};

/** Reason classes the server emits for a malformed deploy record; anything
 * newer than this list renders verbatim. */
const INVALID_REASON_LABEL: Record<string, string> = {
  key: "키 시각 형식이 계약(YYYYMMDDTHHMMSSZ)이 아님",
  deployed_at: "deployed_at이 RFC3339가 아님",
  schema: "deploy-record/v0 본문이 아님(schema·service·result)",
};
const invalidReasonLabel = (reason: string) => INVALID_REASON_LABEL[reason] ?? reason;

/** A deploy result as a status chip: success is the done group, a failure or
 * rollback the failed group (warning icon). The words always print. */
function ResultChip({ result }: { result: string }) {
  return <Chip kind="status" state="deployed" stalled={result !== "success"} label={RESULT_LABEL[result] ?? result} />;
}

/** The deployed version: a SHA or digest cut to 12 characters with a copy
 * button, a version string verbatim, or an explicit gap. */
function VersionChip({ deployedRef, what }: { deployedRef: string | null; what: string }) {
  const short = shortDeployedRef(deployedRef);
  if (deployedRef === null || short === null) {
    return <span className="hk-chip dp-chip-muted">ref 없음</span>;
  }
  return (
    <span className="hk-chip dp-chip-ref">
      <MonoRef value={deployedRef} short={short} label={`${what} 전체 값 복사`} />
    </span>
  );
}

/** Relative age from the server's clock, the absolute time in the title; a
 * record without a usable time is a muted chip, never a guess. */
function When({ now, iso }: { now: string; iso: string | null }) {
  const stamp = deployStamp(iso);
  if (iso === null || stamp === null) {
    return <span className="hk-chip dp-chip-muted">시각 미기록</span>;
  }
  return (
    <time dateTime={iso} title={`${stamp} KST`}>
      {deployElapsed(now, iso) ?? stamp}
    </time>
  );
}

const docHref = (key: string) => `/ui/doc/${key.split("/").map(encodeURIComponent).join("/")}`;

function AttemptRow({ rec, now }: { rec: DeployRecordView; now: string }) {
  return (
    <li className="dp-attempt" data-result={rec.result}>
      <ResultChip result={rec.result} />
      <VersionChip deployedRef={rec.deployed_ref} what="시도 버전" />
      <a className="dp-key" href={docHref(rec.record_key)}>
        {rec.record_key}
      </a>
      <When now={now} iso={rec.deployed_at} />
      {rec.failed_step !== null ? <span className="dp-muted">{rec.failed_step}단계</span> : null}
      {rec.serving_maybe_changed ? (
        <span className="dp-warn dp-attempt-note" role="note">
          <Icon name="warning" />
          배포 명령은 완료 — 서빙 판이 바뀌었을 수 있음
        </span>
      ) : null}
    </li>
  );
}

function RecentAttempts({ svc, now }: { svc: DeployServiceView; now: string }) {
  const [all, setAll] = useState(false);
  if (svc.recent.length === 0) return null;
  const shown = all ? svc.recent : svc.recent.slice(0, RECENT_VISIBLE);
  const hidden = svc.recent.length - RECENT_VISIBLE;
  return (
    <>
      <h4 className="dp-subhead">{svc.docs_capped ? "최근 시도(조회 범위 내)" : "최근 시도"}</h4>
      <ul className="dp-attempts" aria-label={`${svc.service} 최근 배포`}>
        {shown.map((rec) => (
          <AttemptRow key={rec.record_key} rec={rec} now={now} />
        ))}
      </ul>
      {hidden > 0 ? (
        <button type="button" className="dp-more" aria-expanded={all} onClick={() => setAll(!all)}>
          {all ? "접기" : `나머지 ${hidden}건 더 보기`}
        </button>
      ) : null}
    </>
  );
}

function MergedList({ svc, now }: { svc: DeployServiceView; now: string }) {
  if (svc.merged_boundary === "unrecorded") {
    return <p className="dp-note">배포 시각(deployed_at) 미기록 — 마지막 배포 이후 머지 목록의 기준이 없습니다.</p>;
  }
  if (svc.merged_boundary === "no_current") {
    return (
      <p className="dp-note">
        {svc.docs_capped
          ? "조회 범위 안에 성공 배포 기록이 없어 마지막 배포 이후 머지 목록을 계산할 수 없습니다."
          : "성공 배포 기록이 없어 마지막 배포 이후 머지 목록을 계산할 수 없습니다."}
      </p>
    );
  }
  return (
    <div className="dp-merged">
      <h4 className="dp-subhead">마지막 배포 이후 머지됨 ({svc.merged_since.length})</h4>
      {svc.merged_since.length === 0 ? (
        <p className="dp-note">해당 없음</p>
      ) : (
        <ul className="dp-merged-list">
          {svc.merged_since.map((task) => (
            <li key={task.task_id} className="dp-merged-item">
              <a className="dp-nowrap" href={`/ui/tasks/${task.task_id}`}>
                #{task.task_id}
              </a>
              <span className="dp-merged-title">{task.title}</span>
              <a className="dp-nowrap" href={task.pr} target="_blank" rel="noreferrer">
                {deployPRLabel(task.pr)}
              </a>
              <span className="dp-muted">
                <When now={now} iso={task.merged_at} />
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/** #620 AC4 — each malformed record names its key and every failed check so
 * the operator knows what to rewrite. Collapsed to one line by default; an
 * older server sends no list, and the count still stands on its own. */
function InvalidRecords({ svc }: { svc: DeployServiceView }) {
  const [all, setAll] = useState(false);
  if (svc.invalid_count === undefined || svc.invalid_count <= 0) return null;
  const list = svc.invalid ?? [];
  const byReason = new Map<string, number>();
  for (const rec of list) {
    for (const reason of rec.reasons) byReason.set(reason, (byReason.get(reason) ?? 0) + 1);
  }
  const shown = all ? list : list.slice(0, INVALID_VISIBLE);
  return (
    <CollapsedWarning title={`${svc.invalid_count}개 기록 형식 불일치`} testId="deploy-invalid-warning">
      <p className="dp-invalid-count">형식이 맞지 않는 기록 {svc.invalid_count}건은 표시하지 않았습니다.</p>
      {byReason.size > 0 ? (
        <p className="dp-invalid-count">
          {[...byReason].map(([reason, n]) => `${invalidReasonLabel(reason)} ${n}건`).join(" · ")}
        </p>
      ) : null}
      {list.length > 0 ? (
        <ul className="dp-invalid-list" data-testid="deploy-invalid-list">
          {shown.map((rec) => (
            <li key={rec.key} className="dp-invalid-item">
              <span className="hk-mono">{rec.key}</span>
              <span className="dp-muted">{rec.reasons.map(invalidReasonLabel).join(" · ")}</span>
            </li>
          ))}
        </ul>
      ) : null}
      {list.length > shown.length ? (
        <button type="button" className="dp-more" onClick={() => setAll(true)}>
          {list.length - shown.length}개 더 보기
        </button>
      ) : null}
    </CollapsedWarning>
  );
}

function ServiceCard({ svc, now }: { svc: DeployServiceView; now: string }) {
  // The last attempt is the newest record the server sent; a service whose only
  // record is its current version has just that one.
  const last = svc.latest ?? svc.recent[0] ?? svc.current;
  const noCurrent =
    svc.record_count === 0
      ? "기록 없음"
      : svc.docs_capped
        ? /* Under a capped scan, "no success" only holds inside the window — an
             older success may exist beyond it, so say "unverified", not "none". */
          "조회 범위 안에 성공 배포 기록이 없습니다 — 상한 밖은 미확인"
        : "성공 배포 기록 없음";
  return (
    <section className="dp-card" aria-label={`${svc.service} 배포`}>
      <header className="dp-card-head">
        <h3 className="dp-svc">{svc.service}</h3>
        {svc.target ? <span className="hk-chip dp-chip-host">{svc.target}</span> : null}
        <span className="hk-mono dp-muted">{svc.repo}</span>
      </header>
      <div className="dp-status" data-result={svc.current === null ? "none" : svc.current.result}>
        {svc.current === null ? (
          <span className="dp-muted">{noCurrent}</span>
        ) : (
          <>
            {/* docs_capped means the scan stopped before the end of the key
                space — a record beyond the cap could carry a later
                deployed_at, so the shown current/latest are in-window results,
                not fleet truth. */}
            <span className="dp-muted">{svc.docs_capped ? "현재 판(조회 범위 내)" : "현재 판"}</span>
            <VersionChip deployedRef={svc.current.deployed_ref} what={`${svc.service} 현재 판`} />
          </>
        )}
        {last !== null ? (
          <>
            <span className="dp-muted">마지막 시도</span>
            <ResultChip result={last.result} />
            <When now={now} iso={last.deployed_at} />
          </>
        ) : null}
      </div>
      <RecentAttempts svc={svc} now={now} />
      {svc.docs_capped ? (
        <p className="dp-warn dp-note">
          <Icon name="warning" />
          기록 조회 상한에 닿았습니다 — 더 오래된 배포 기록이 있을 수 있어 현재 판·최근 시도·머지 목록 모두 조회 범위 안의 결과입니다.
        </p>
      ) : null}
      <InvalidRecords svc={svc} />
      <MergedList svc={svc} now={now} />
    </section>
  );
}

type Props = {
  fetchStatus?: () => Promise<DeployPendingResponse>;
  pollMs?: number;
};

/** Self-fetching block: owns its own poll via useDeployPending so the queue
 * list's dataset and types stay untouched. A failed refresh keeps the last
 * good payload with a warning; a first-load failure is an explicit error,
 * never an empty board. */
export function DeployPanel({ fetchStatus = fetchDeployPending, pollMs = 15_000 }: Props) {
  const { data, failed, at } = useDeployPending(fetchStatus, pollMs);
  useEffect(() => {
    if (!data) return;
    const previousTitle = document.title;
    document.title = `Deploys · ${data.services.reduce((sum, svc) => sum + svc.merged_since.length, 0)} waiting`;
    return () => { document.title = previousTitle; };
  }, [data]);
  const checked = at === null ? null : new Intl.DateTimeFormat("en-GB", { timeZone: "Asia/Seoul", hour: "2-digit", minute: "2-digit", second: "2-digit", hourCycle: "h23" }).format(at);

  if (data === null) {
    return (
      <section className="dp-panel" aria-label="배포 상태" data-deploy-state={failed ? "error" : "loading"}>
        <p className="dp-note" role={failed ? "alert" : "status"}>
          {failed ? "배포 정보를 불러오지 못했습니다 — 다시 시도합니다." : "배포 정보를 불러오는 중…"}
        </p>
      </section>
    );
  }
  return (
    <section className="dp-panel" aria-label="배포 상태" data-deploy-state={failed ? "stale" : "ready"}>
      <div className="dp-panel-head">
        <h2 className="dp-title">서비스별 현황</h2>
        {checked !== null ? (
          <span className={failed ? "dp-warn" : "dp-muted"} role={failed ? "alert" : undefined}>
            {failed ? "갱신 실패 · " : ""}마지막 확인 {checked} KST
          </span>
        ) : null}
        {data.events_capped ? <span className="dp-warn">머지 이벤트 조회 상한에 닿아 목록이 잘렸을 수 있습니다.</span> : null}
      </div>
      {[...data.services].sort((a, b) => a.service.localeCompare(b.service)).map((svc) => (
        <ServiceCard key={svc.service} svc={svc} now={data.generated_at} />
      ))}
      <p className="dp-note">
        배포가 필요 없는 머지(문서·CI·스킬)도 목록에 섞여 있을 수 있습니다. PR은 태스크에 적힌 PR을 기준으로 연결하며, 머지를 마친 PR이
        아니라 처음 연 PR일 수 있습니다.
      </p>
    </section>
  );
}
