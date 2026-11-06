import { useCallback, useEffect, useRef, useState } from "react";
import { readCsrfToken } from "../shared/boardApi";
import { isTypingTarget } from "../shared/search";
import { formatMinuteKST } from "../shared/time";
import { loadDetail, postResolve } from "./api";
import { ConfirmSheet } from "./ConfirmSheet";
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
  return Object.prototype.hasOwnProperty.call(states, raw)
    ? states[raw as RecordState]
    : `${states.unknown} ${raw}`;
}
function deadline(iso: string) {
  const ms = Date.parse(iso) - Date.now();
  const minutes = Math.ceil(Math.abs(ms) / 60_000);
  return `${formatMinuteKST(iso) ?? iso} · ${ms < 0 ? `${minutes}분 경과` : `${minutes}분 남음`}${ms >= 0 && ms <= 2 * 60 * 60_000 ? " · 임박" : ""}`;
}
export function DecisionPage({ id }: { id: string }) {
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
        screen !== "editing" ||
        !detail?.answerable ||
        !detail.request
      )
        return;
      const n = Number(e.key);
      if (Number.isInteger(n) && n > 0 && n <= detail.request.options.length)
        setOption(detail.request.options[n - 1].key);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [detail, screen]);
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
      if (knownConflict || (response.status === 200 && accepted))
        await fetchDetail();
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
          <button onClick={() => location.reload()}>다시 시도</button>
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
    detail.disabled_reason === "source_elsewhere"
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
      <a href={"/ui/decisions" + location.search}>← 결정 목록</a>
      <header>
        <p className="decision-state">● {label(state)}</p>
        <h1>결정 #{detail.record.id}</h1>
        <p>
          {detail.task?.project ?? detail.record.lane}{" "}
          {detail.task && (
            <>
              · <a href={`/ui/tasks/${detail.task.id}`}>{detail.task.title}</a>
            </>
          )}
        </p>
        {req && (
          <p>
            {req.id} · revision {req.revision} · {req.answer_policy}
          </p>
        )}
      </header>
      <h2>{req?.question ?? detail.record.question}</h2>
      {detail.task && <p>태스크 상태: {detail.task.state}</p>}
      {req?.resolution && (
        <p>
          답변: {req.resolution.option} {req.resolution.text}
        </p>
      )}
      {pending && (
        <p>
          중계 답 접수 · 운영자 확인 대기 / 접수 · 확인 대기 — {pending.actor} ·{" "}
          {pending.channel} · {pending.received_at}. 기한은 계속 진행됩니다.
        </p>
      )}
      {stale && (
        <p role="alert">요청 revision이 바뀌었습니다. 다시 확인하세요.</p>
      )}
      {req && (
        <>
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
                aria-checked={option === o.key}
                disabled={!detail.answerable || screen !== "editing"}
                key={o.key}
                onClick={() => setOption(o.key)}
              >
                <strong>
                  {i + 1}. {o.label}
                </strong>{" "}
                {o.recommended && <span>권장</span>}{" "}
                {req.default_option === o.key && <span>미응답 시 기본값</span>}
              </button>
            ))}
          </div>
          {req.allow_free && (
            <label>
              메모
              <textarea
                value={text}
                disabled={!detail.answerable || screen !== "editing"}
                onChange={(e) => setText(e.target.value)}
              />
            </label>
          )}
          {req.due_at && <p>기한: {deadline(req.due_at)}</p>}
          <p>
            답이 없으면 {req.default_action} (
            {req.default_trigger || "기록된 트리거 없음"})
          </p>
          {state === "default_applied" && req.resolution?.receipt && (
            <p>기본값 적용됨 {req.resolution.receipt}</p>
          )}
          <details>
            <summary>맥락</summary>
            <p>
              {detail.task?.title} · {detail.task?.state}
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
                {h.id} · revision {h.revision} · {label(h.state)}
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
            <button onClick={checkResult}>결과 확인</button>
            {checked && body && (
              <button
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
