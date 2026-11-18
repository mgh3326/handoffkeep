import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { usePoll } from "../shared/usePoll";
import { formatAge, formatMinuteUTC } from "../shared/time";
import { stateLabel, type ChipGroup } from "../shared/states";
import { Chip } from "../shared/ui/Chip";
import { Mono } from "../shared/ui/Mono";
import { loadWaits } from "./api";
import { WAIT_SECTION_KEYS, type WaitRow, type WaitSection, type WaitSectionKey, type WaitsResponse } from "./types";
import "./waits.css";

// Read-only by design: the page has no control that writes. Answering a
// decision happens elsewhere; this page only shows what is waiting on whom.

const POLL_MS = 60_000;

const SECTION_TITLE: Record<WaitSectionKey, string> = {
  needs_operator: "운영자의 답변을 기다리는 것",
  failed_start: "시작에 실패해 멈춘 것",
  waiting_on: "다른 작업·레인·PR·배포를 기다리는 것",
  idle_started: "진행 중이지만 활동이 없는 것",
};

const SECTION_SHORT: Record<WaitSectionKey, string> = {
  needs_operator: "운영자 답변",
  failed_start: "시작 실패",
  waiting_on: "대기",
  idle_started: "활동 없음",
};

// The next action's label and the chip group that colors it. An unknown code
// is shown raw, in the neutral group.
const NEXT_ACTION: Record<string, { label: string; group: ChipGroup }> = {
  answer_decision: { label: "운영자 답변 필요", group: "decision" },
  confirm_answer: { label: "답변 기록됨 · 확인 대기", group: "input" },
  operator_step: { label: "운영자가 직접 할 단계", group: "decision" },
  retry_spawn: { label: "시작 재시도 또는 취소 결정", group: "failed" },
  wait_lane: { label: "레인 응답 대기", group: "neutral" },
  wait_task: { label: "다른 작업 대기", group: "neutral" },
  wait_pr: { label: "PR 대기", group: "neutral" },
  wait_ci: { label: "CI 대기", group: "neutral" },
  wait_external: { label: "외부 의존성 대기", group: "neutral" },
  wait_blocker: { label: "선행 작업 대기", group: "neutral" },
  wait_start_condition: { label: "시작 조건 대기", group: "neutral" },
  wait_review: { label: "리뷰 대기", group: "neutral" },
  check_progress: { label: "진행 상황 확인", group: "neutral" },
};

const ON_KIND_LABEL: Record<string, string> = {
  operator: "운영자",
  lane: "레인",
  task: "작업",
  pr: "PR",
  ci: "CI",
  external: "외부",
};

function nextAction(code: string): { label: string; group: ChipGroup } {
  return NEXT_ACTION[code] ?? { label: code, group: "neutral" };
}

function onKindLabel(kind: string): string {
  return ON_KIND_LABEL[kind] ?? kind;
}

/** The item's anchor in the page; ?item=<key> focuses the row with this id. */
function rowDomId(key: string): string {
  return `wait-${key}`;
}

function formatTime(iso: string): string {
  return formatMinuteUTC(iso) ?? iso;
}

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="wt-fact">
      <dt>{label}</dt>
      <dd>{children}</dd>
    </div>
  );
}

function WaitRowItem({ row, focused }: { row: WaitRow; focused: boolean }) {
  const action = nextAction(row.next_action);
  return (
    <li id={rowDomId(row.key)} className={focused ? "wt-row wt-row--focus" : "wt-row"} data-focus={focused ? "true" : undefined}>
      <div className="wt-row-head">
        <span className={`hk-chip hk-chip--${action.group}`} data-group={action.group}>
          {action.label}
        </span>
        {row.grounds_link ? (
          <a className="wt-row-title" href={row.grounds_link}>
            {row.title}
          </a>
        ) : (
          <span className="wt-row-title">{row.title}</span>
        )}
        {row.task_id > 0 ? <Mono>#{row.task_id}</Mono> : null}
      </div>
      <dl className="wt-facts">
        {row.lane ? (
          <Fact label="레인">
            <Chip kind="lane" lane={row.lane} />
          </Fact>
        ) : null}
        {row.on_kind ? (
          <Fact label="대기 대상">
            {onKindLabel(row.on_kind)}
            {row.on_ref ? (
              <>
                {" · "}
                <Mono>{row.on_ref}</Mono>
              </>
            ) : null}
          </Fact>
        ) : null}
        <Fact label="사유">{row.reason}</Fact>
        <Fact label="시작">
          <time dateTime={row.since}>{formatTime(row.since)}</time>
        </Fact>
        <Fact label="경과">{formatAge(row.age_seconds)}</Fact>
        <Fact label="상태">{stateLabel(row.state)}</Fact>
        {row.project ? <Fact label="프로젝트">{row.project}</Fact> : null}
        {row.request_id ? (
          <Fact label="결정 요청">
            <Mono>{row.request_id}</Mono>
          </Fact>
        ) : null}
      </dl>
      {row.answer ? (
        <div className="wt-answer">
          <p className="wt-answer-words">답변: {row.answer.words}</p>
          <p className="wt-answer-meta">
            기록한 사람 {row.answer.actor} · <time dateTime={row.answer.at}>{formatTime(row.answer.at)}</time>
          </p>
        </div>
      ) : null}
    </li>
  );
}

function SectionBlock({ section, focusKey }: { section: WaitSection; focusKey: string | null }) {
  const id = `wt-section-${section.key}`;
  return (
    <section className="wt-section" aria-labelledby={id}>
      <div className="wt-section-head">
        <h2 id={id} className="wt-section-title">
          {SECTION_TITLE[section.key]}
        </h2>
        <span className="wt-count">{section.count}건</span>
      </div>
      {section.truncated ? (
        <p className="wt-note wt-note--warn">
          목록이 잘렸습니다. 전체 {section.count}건 중 {section.rows.length}건만 표시합니다.
        </p>
      ) : null}
      {section.rows.length === 0 ? (
        <p className="wt-empty">없음</p>
      ) : (
        <ul className="wt-list">
          {section.rows.map((row) => (
            <WaitRowItem key={`${section.key}:${row.key}`} row={row} focused={row.key === focusKey} />
          ))}
        </ul>
      )}
    </section>
  );
}

function BacklogBlock({ data }: { data: WaitsResponse }) {
  const { backlog } = data;
  return (
    <section className="wt-section" aria-labelledby="wt-backlog-title">
      <div className="wt-section-head">
        <h2 id="wt-backlog-title" className="wt-section-title">
          일반 백로그
        </h2>
        <span className="wt-count">{backlog.count}건</span>
      </div>
      <p className="wt-note">대기 상태가 없는 작업입니다. 위의 구획에는 들어가지 않습니다.</p>
      {backlog.truncated ? (
        <p className="wt-note wt-note--warn">
          목록이 잘렸습니다. 전체 {backlog.count}건 중 {backlog.rows.length}건만 표시합니다.
        </p>
      ) : null}
      {backlog.rows.length === 0 ? (
        <p className="wt-empty">없음</p>
      ) : (
        <ul className="wt-list">
          {backlog.rows.map((row) => (
            <li key={row.task_id} className="wt-backlog-row">
              <a className="wt-backlog-link" href={`/ui/tasks/${row.task_id}`}>
                <Mono>#{row.task_id}</Mono>
                <span className="wt-backlog-title">{row.title}</span>
              </a>
              <div className="wt-backlog-meta">
                {row.lane ? <Chip kind="lane" lane={row.lane} /> : null}
                <time dateTime={row.created_at}>{formatTime(row.created_at)}</time>
              </div>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

export function WaitsPage() {
  const load = useCallback(() => loadWaits(), []);
  const { data, failed } = usePoll(load, POLL_MS);
  // The filters are read once, on mount: they name the view, not a live query.
  const [params] = useState(() => new URLSearchParams(window.location.search));
  const rawSection = params.get("section");
  const focusKey = params.get("item");
  const sectionFilter = WAIT_SECTION_KEYS.find((k) => k === rawSection) ?? null;
  const unknownSection = rawSection !== null && rawSection !== "" && sectionFilter === null;

  // Scroll to the focused row once; later polls must not move the page.
  const scrolled = useRef(false);
  const visible = useMemo(() => {
    if (!data) return [];
    return sectionFilter ? data.sections.filter((s) => s.key === sectionFilter) : data.sections;
  }, [data, sectionFilter]);
  const focusFound = useMemo(
    () => (focusKey ? visible.some((s) => s.rows.some((r) => r.key === focusKey)) : false),
    [visible, focusKey],
  );
  useEffect(() => {
    if (!focusKey || !focusFound || scrolled.current) return;
    const el = document.getElementById(rowDomId(focusKey));
    if (el) {
      scrolled.current = true;
      el.scrollIntoView?.({ block: "center" });
    }
  }, [focusKey, focusFound, data]);

  const header = (
    <header className="wt-header">
      <h1 className="wt-page-title">운영자와 대기 상태</h1>
      {data ? (
        <div className="wt-time">
          기준 시각 <time dateTime={data.as_of}>{formatTime(data.as_of)}</time>
        </div>
      ) : null}
    </header>
  );

  if (failed && !data) {
    return (
      <div className="wt-root" role="region" aria-label="운영자와 대기 상태">
        {header}
        <p className="wt-empty" role="alert">
          대기 상태 데이터를 불러오지 못했습니다. 잠시 후 다시 시도합니다.
        </p>
      </div>
    );
  }

  if (!data) {
    return (
      <div className="wt-root" role="region" aria-label="운영자와 대기 상태">
        {header}
        <p className="wt-empty" role="status">
          대기 상태 불러오는 중…
        </p>
      </div>
    );
  }

  const byKey = new Map(data.sections.map((s) => [s.key, s]));

  return (
    <div className="wt-root" role="region" aria-label="운영자와 대기 상태">
      {header}

      {failed ? (
        <p className="wt-notice" role="alert">
          갱신에 실패했습니다. 아래는 기준 시각 {formatTime(data.as_of)}의 자료입니다.
        </p>
      ) : null}

      <p className="wt-counts" aria-label="구획별 건수">
        {WAIT_SECTION_KEYS.map((key) => (
          <span key={key} className="wt-count-item">
            {SECTION_SHORT[key]} {byKey.get(key)?.count ?? 0}건
          </span>
        ))}
        <span className="wt-count-item">일반 백로그 {data.backlog.count}건</span>
      </p>

      {sectionFilter ? (
        <p className="wt-notice">
          구획 필터: <strong>{SECTION_TITLE[sectionFilter]}</strong> · <a href="/ui/waits">전체 보기</a>
        </p>
      ) : null}
      {unknownSection ? <p className="wt-notice">알 수 없는 구획입니다: {rawSection}. 전체를 표시합니다.</p> : null}
      {focusKey && !focusFound ? (
        <p className="wt-notice" role="status">
          항목 <Mono>{focusKey}</Mono>을(를) 이 화면에서 찾을 수 없습니다.
        </p>
      ) : null}

      {visible.map((section) => (
        <SectionBlock key={section.key} section={section} focusKey={focusFound ? focusKey : null} />
      ))}

      {sectionFilter ? null : <BacklogBlock data={data} />}
    </div>
  );
}
