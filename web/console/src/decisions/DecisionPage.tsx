import { TranslatedText, useShowOriginal } from "../shared/translation";
import { useCallback, useEffect, useRef, useState } from "react";
import { readCsrfToken } from "../shared/boardApi";
import { isTypingTarget } from "../shared/search";
import { formatMinuteKST } from "../shared/time";
import { Chip } from "../shared/ui/Chip";
import { Mono } from "../shared/ui/Mono";
import { stateLabel } from "../shared/states";
import { formatAge, formatRemaining } from "./format";
import { loadDetail, postResolve } from "./api";
import { Icon } from "../shared/Icon";
import { ConfirmSheet } from "./ConfirmSheet";
import { DecisionChip, stateChip } from "./DecisionChip";
import { readDraft, saveDraft, type Draft } from "./drafts";
import { ResultBadge, resultLabel } from "./ResultBadge";
import type {
  Detail,
  RecordState,
  ResolveBody,
  ScreenState,
  SubmitResult,
} from "./types";
import "./decisions.css";

const states: Record<RecordState, string> = {
  open: "열림 · 응답 대기",
  overdue: "기한 경과 · 기본값 미적용",
  uncleaned: "미정리 요청 · 종료된 태스크",
  answered: "답변됨",
  default_applied: "기본값 적용됨",
  withdrawn: "철회됨",
  superseded: "대체됨",
  unrecorded: "미기록",
  unknown: "지원되지 않는 상태",
};
function label(raw: string): string {
  if (raw === "unknown") return `${states.unknown} ${raw}`;
  return Object.prototype.hasOwnProperty.call(states, raw)
    ? states[raw as RecordState]
    : `${states.unknown} ${raw}`;
}
const policies: Record<string, string> = {
  operator_direct: "직접 답만",
  operator_relay_allowed: "중계 답 허용",
};
function policyLabel(raw: string): string {
  return Object.prototype.hasOwnProperty.call(policies, raw) ? policies[raw] : raw;
}
// The same relative-time functions as the list, so a row and its page agree.
function deadline(iso: string) {
  const now = new Date().toISOString();
  const left = formatRemaining(now, iso);
  const past = left === null ? formatAge(now, iso) : null;
  const ms = Date.parse(iso) - Date.now();
  const rel = left !== null ? `${left} 남음` : past !== null ? `${past} 경과` : "";
  return `${formatMinuteKST(iso) ?? iso}${rel ? ` · ${rel}` : ""}${ms >= 0 && ms <= 2 * 60 * 60_000 ? " · 임박" : ""}`;
}
export function DecisionPage({ id }: { id: string }) {
  const globalOriginal = useShowOriginal();
  const [optionsOriginal, setOptionsOriginal] = useState<boolean | null>(null);
  const [detail, setDetail] = useState<Detail | null>(null);
  const [screen, setScreen] = useState<ScreenState>("loading");
  const [option, setOption] = useState("");
  const [text, setText] = useState("");
  const [result, setResult] = useState<SubmitResult | null>(null);
  const [rawResult, setRawResult] = useState("");
  const [error, setError] = useState("");
  const [stale, setStale] = useState(false);
  const [body, setBody] = useState<ResolveBody | null>(null);
  const [checked, setChecked] = useState(false);
  const [sentAt, setSentAt] = useState("");
  const busy = useRef(false);
  const opener = useRef<HTMLButtonElement>(null);
  const editable = screen === "editing" || screen === "send_failed";
  useEffect(() => {
    if (result)
      window.dispatchEvent(
        new CustomEvent("hk-console-announce", {
          detail: resultLabel(result, rawResult),
        }),
      );
  }, [result, rawResult]);
  useEffect(() => {
    if (screen === "unconfirmed")
      window.dispatchEvent(
        new CustomEvent("hk-console-announce", {
          detail: resultLabel("unconfirmed"),
        }),
      );
  }, [screen]);
  const fetchDetail = useCallback(async () => {
    const next = await loadDetail(id);
    setDetail(next);
    return next;
  }, [id]);
  useEffect(() => {
    let active = true;
    setScreen("loading");
    loadDetail(id)
      .then((d) => {
        if (!active) return;
        setDetail(d);
        const draft = d.request ? readDraft(id, d.request.id) : null;
        setOption(draft?.option ?? "");
        setText(draft?.text ?? "");
        if (draft?.event_key && d.request) {
          setBody({
            request_id: d.request.id,
            option: draft.option,
            text: draft.text,
            event_key: draft.event_key,
          });
          setSentAt(draft.saved_at);
          setScreen("unconfirmed");
        } else setScreen("editing");
      })
      .catch(() => {
        if (active) setScreen("read_failed");
      });
    return () => {
      active = false;
    };
  }, [id]);
  useEffect(() => {
    if (
      !detail?.request ||
      screen === "loading" ||
      screen === "unconfirmed" ||
      screen === "sending"
    )
      return;
    saveDraft(id, detail.request.id, {
      option,
      text,
      saved_at: new Date().toISOString(),
    });
  }, [detail, id, option, text, screen]);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (
        e.isComposing ||
        isTypingTarget(e.target) ||
        !editable ||
        !detail?.answerable ||
        !detail.request
      )
        return;
      const n = Number(e.key);
      if (Number.isInteger(n) && n > 0 && n <= detail.request.options.length) {
        setOption(detail.request.options[n - 1].key);
        setStale(false);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [detail, editable]);
  const close = () => {
    setScreen("editing");
    opener.current?.focus();
  };
  async function send(payload: ResolveBody) {
    setScreen("sending");
    try {
      const response = await postResolve(payload);
      const knownConflict =
        response.status === 409 &&
        ["conflict", "stale_revision", "expired", "superseded"].includes(
          response.result,
        );
      const accepted =
        (response.status === 200 && response.result === "answered") ||
        (response.status === 202 && response.result === "pending_confirmation");
      if (knownConflict || accepted) await fetchDetail();
      setResult(response.result);
      setRawResult(response.raw);
      if (accepted || knownConflict) {
        setOption("");
        setText("");
        setScreen("editing");
        saveDraft(id, payload.request_id, {
          option: "",
          text: "",
          saved_at: new Date().toISOString(),
        });
      } else if (response.status >= 200 && response.status < 300) {
        setScreen("unconfirmed");
        setChecked(false);
      } else {
        saveDraft(id, payload.request_id, {
          option: payload.option,
          text: payload.text,
          saved_at: new Date().toISOString(),
        });
        setError(response.raw);
        setScreen("send_failed");
      }
    } catch {
      setScreen("unconfirmed");
      setChecked(false);
    } finally {
      busy.current = false;
    }
  }
  async function confirm() {
    if (busy.current || !detail?.request) return;
    busy.current = true;
    setScreen("sending");
    try {
      const next = await fetchDetail();
      if (
        !next.answerable ||
        next.request?.id !== detail.request.id ||
        next.request.revision !== detail.request.revision
      ) {
        setStale(true);
        setOption("");
        setText("");
        setScreen("editing");
        busy.current = false;
        return;
      }
      const payload = {
        request_id: detail.request.id,
        option,
        text,
        event_key: `console:${id}:${detail.request.id}:${crypto.randomUUID()}`,
      };
      const at = new Date().toISOString();
      setBody(payload);
      setSentAt(at);
      saveDraft(id, payload.request_id, {
        option,
        text,
        event_key: payload.event_key,
        saved_at: at,
      });
      await send(payload);
    } catch {
      setError("현재 요청을 확인하지 못했습니다");
      setScreen("read_failed");
      busy.current = false;
    }
  }
  async function checkResult() {
    try {
      const d = await fetchDetail();
      if (!d.answerable || d.request?.id !== body?.request_id) {
        if (body)
          saveDraft(id, body.request_id, {
            option: "",
            text: "",
            saved_at: new Date().toISOString(),
          });
        setScreen("editing");
        return;
      }
      setChecked(true);
    } catch {
      setError("불러오지 못함");
    }
  }
  if (screen === "loading") return <p role="status">불러오는 중</p>;
  if (!detail)
    return (
      <article className="decision-page">
        <p role="alert">
          불러오지 못함{" "}
          <button type="button" className="hk-button" onClick={() => location.reload()}>
            다시 시도
          </button>
        </p>
      </article>
    );
  const req = detail.request;
  const state = req?.state ?? detail.record.status;
  const latestAttempt =
    req && detail.attempts.find((a) => a.revision_request_id === req.id);
  const pending =
    detail.answerable && latestAttempt?.result === "pending_confirmation"
      ? latestAttempt
      : null;
  const reason =
    (!Object.prototype.hasOwnProperty.call(states, state) ||
      state === "unknown") &&
    !detail.disabled_reason
      ? `${states.unknown} ${state}`
      : detail.disabled_reason === "source_elsewhere"
        ? "이 원천은 기존 페이지에서 답합니다"
        : detail.disabled_reason === "superseded"
          ? "대체된 요청입니다"
          : detail.disabled_reason === "closed"
            ? "닫힌 요청입니다"
            : detail.disabled_reason
              ? `지원되지 않는 사유 ${detail.disabled_reason}`
              : "";
  const oldDrafts: { request: string; draft: Draft }[] = detail.history
    .filter((h) => h.id !== req?.id)
    .flatMap((h) => {
      const draft = readDraft(id, h.id);
      return draft && (draft.option || draft.text || draft.event_key)
        ? [{ request: h.id, draft }]
        : [];
    });
  return (
    <article className="decision-page">
      <a className="decision-back" href={"/ui/decisions" + location.search}>
        <Icon name="caret-down" className="decision-back-icon" />
        결정 목록
      </a>
      <header>
        <p className="decision-state">
          <DecisionChip {...stateChip(state)} label={label(state)} />
        </p>
        <h1>결정 #{detail.record.id}</h1>
        <p>
          {detail.task?.project ?? detail.record.lane}{" "}
          {detail.task && (
            <>
              · <a href={`/ui/tasks/${detail.task.id}`}>{!globalOriginal && detail.task.title_ko && !detail.task.title_ko_stale ? detail.task.title_ko : detail.task.title}{detail.task.title_ko_stale && <small> 갱신 필요</small>}</a>
            </>
          )}
        </p>
        {req && (
          <p className="decision-ref">
            <Mono>{req.id}</Mono> · revision {req.revision} · {policyLabel(req.answer_policy)}
          </p>
        )}
      </header>
      <h2 className="decision-question"><TranslatedText original={req?.question ?? detail.record.question} korean={req?.question_ko ?? detail.record.question_ko} stale={req?.question_ko_stale ?? detail.record.question_ko_stale} /></h2>
      {detail.task && (
        <p className="decision-task-state">
          태스크 상태: <Chip kind="status" state={detail.task.state} />
        </p>
      )}
      {state === "answered" && req?.resolution && (
        <p>
          답변: {req.resolution.option} {req.resolution.text}
        </p>
      )}
      {pending && (
        <section>
          {result !== "pending_confirmation" && (
            <ResultBadge result="pending_confirmation" />
          )}
          <p>
            중계 답 접수 · 운영자 확인 대기 — {pending.actor} ·{" "}
            {pending.channel} · {pending.received_at}. 기한은 계속 진행됩니다.
          </p>
        </section>
      )}
      {stale && (
        <p role="alert">요청 revision이 바뀌었습니다. 다시 확인하세요.</p>
      )}
      {req && (
        <>
          {req.options_ko_stale && <small>갱신 필요</small>}
          {req.options_ko && !req.options_ko_stale && <button type="button" aria-pressed={optionsOriginal ?? globalOriginal} onClick={() => setOptionsOriginal(!(optionsOriginal ?? globalOriginal))}>{(optionsOriginal ?? globalOriginal) ? "선택지 번역" : "선택지 원문"}</button>}
          <div
            role="radiogroup"
            aria-label="선택지"
            aria-describedby={error ? "decision-error" : undefined}
            className="decision-options"
          >
            {req.options.map((o, i) => (
              <button
                type="button"
                role="radio"
                className="decision-option"
                aria-checked={option === o.key}
                disabled={!detail.answerable || !editable}
                key={o.key}
                onClick={() => {
                  setOption(o.key);
                  setStale(false);
                }}
              >
                <span className="decision-option-mark" aria-hidden="true">
                  {option === o.key && <Icon name="check" />}
                </span>
                <span className="decision-option-body">
                  <strong>
                    {i + 1}. {!(optionsOriginal ?? globalOriginal) && req.options_ko && !req.options_ko_stale ? req.options_ko[i]?.label ?? o.label : o.label}
                  </strong>
                  <span className="hk-chips">
                    {o.recommended && <DecisionChip group="done" label="권장" icon="check" />}
                    {req.default_option === o.key && (
                      <DecisionChip group="neutral" label="미응답 시 기본값" />
                    )}
                  </span>
                </span>
              </button>
            ))}
          </div>
          {req.allow_free && (
            <label className="decision-memo">
              메모
              <textarea
                className="hk-input"
                value={text}
                disabled={!detail.answerable || !editable}
                onChange={(e) => {
                  setText(e.target.value);
                  setStale(false);
                }}
              />
            </label>
          )}
          <div className="decision-meta">
            {req.due_at && <p>기한: {deadline(req.due_at)}</p>}
            <p>
              답이 없으면 {req.default_action} (
              {req.default_trigger || "기록된 트리거 없음"})
            </p>
            {state === "default_applied" && req.resolution?.receipt && (
              <p>기본값 적용됨 {req.resolution.receipt}</p>
            )}
          </div>
          <details>
            <summary>맥락</summary>
            <p>
              {detail.task?.title} · {detail.task ? stateLabel(detail.task.state) : ""}
            </p>
            {req.doc && <a href={`/ui/doc/${req.doc}`}>{req.doc}</a>}
            {req.refs?.map((ref) => (
              <p key={ref}>{ref}</p>
            ))}
          </details>
          <details id="decision-history">
            <summary>이력</summary>
            {detail.history.map((h) => (
              <p key={h.id}>
                {h.id} · revision {h.revision} · {label(h.state)} · <TranslatedText original={h.question} korean={h.question_ko} stale={h.question_ko_stale} />
              </p>
            ))}
            {detail.attempts.map((a) => (
              <p key={a.id}>
                {a.actor} · {a.channel} · {a.result} · {a.received_at}
              </p>
            ))}
          </details>
          {oldDrafts.map((d) => (
            <aside key={d.request}>
              이전 revision 초안 ({d.request}): {d.draft.option} {d.draft.text}
            </aside>
          ))}
        </>
      )}
      <div className="decision-cta">
        {screen === "unconfirmed" ? (
          <>
            <ResultBadge result="unconfirmed" />
            <p>
              {body?.request_id} · {body?.event_key} · {sentAt}
            </p>
            <button type="button" className="hk-button" onClick={checkResult}>
              결과 확인
            </button>
            {checked && body && (
              <button
                type="button"
                className="hk-button hk-button--primary"
                disabled={!readCsrfToken()}
                onClick={() => {
                  if (!busy.current) {
                    busy.current = true;
                    void send(body);
                  }
                }}
              >
                다시 보내기
              </button>
            )}
            {!readCsrfToken() && (
              <p>
                로그인을 다시 확인하려면 새로고침하세요{" "}
                <a href={location.pathname + location.search}>새로고침</a>
              </p>
            )}
          </>
        ) : (
          <>
            <button
              ref={opener}
              type="button"
              className="hk-button hk-button--primary decision-answer"
              disabled={
                !detail.answerable ||
                !readCsrfToken() ||
                screen === "sending" ||
                !req ||
                !Object.prototype.hasOwnProperty.call(states, state) ||
                state === "unknown"
              }
              onClick={() => {
                if (!option && !text.trim()) {
                  setError("선택지 또는 메모를 입력하세요");
                  return;
                }
                setError("");
                setScreen("confirming");
              }}
            >
              {screen === "sending" ? "전송 중" : "답변 확인"}
            </button>
            {reason && (
              <p>
                {reason}{" "}
                {detail.disabled_reason === "source_elsewhere" ? (
                  <a href="/ui/decisions/legacy">기존 페이지</a>
                ) : detail.disabled_reason === "superseded" &&
                  detail.record.superseded_record_id ? (
                  <a
                    href={`/ui/decisions/${detail.record.superseded_record_id}`}
                  >
                    새 요청
                  </a>
                ) : detail.disabled_reason === "closed" ? (
                  <a href="#decision-history">이력 보기</a>
                ) : null}
              </p>
            )}
            {state === "uncleaned" && (
              <p>
                종료된 태스크에 남은 미정리 요청입니다.{" "}
                <a href="#decision-history">이력 보기</a>
              </p>
            )}
            {!readCsrfToken() && (
              <p>
                로그인을 다시 확인하려면 새로고침하세요{" "}
                <a href={location.pathname + location.search}>새로고침</a>
              </p>
            )}
          </>
        )}
        {error && (
          <p id="decision-error" role="alert">
            {error}
          </p>
        )}
      </div>
      {screen === "confirming" && req && (
        <ConfirmSheet
          option={req.options.find((o) => o.key === option)?.label ?? option}
          text={text}
          revision={req.revision}
          onEdit={close}
          onCancel={close}
          onConfirm={() => void confirm()}
        />
      )}
      {result && <ResultBadge result={result} raw={rawResult} />}
    </article>
  );
}
