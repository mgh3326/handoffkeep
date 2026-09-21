// TaskComments — the detail's 코멘트 tab: a task's append-only comments in
// creation order plus the write form (hk:doc 2410 §4 AC 3·4·6).
//
// - A comment is data, not an approval or a dispatch. The form says so in
//   plain view, and writing one changes nothing about the task.
// - The body renders through the overview's DocMarkdown (lazy chunk) — the
//   same sanitize boundary as the task body, not a second renderer.
// - The list keeps loading / 오류 / 권한 없음 / 태스크 없음 / 빈 목록 apart:
//   a failed read is never shown as "no comments".
// - The write sends only the body and the session CSRF token; the author is
//   whoever the server authenticated.

import { Suspense, useCallback, useEffect, useState, type FormEvent, type KeyboardEvent } from "react";
import { CommentWriteError, fetchTaskComments, postTaskComment, readCsrfToken, type TaskComments as TaskCommentsData } from "../board/api";
import type { BoardComment } from "../board/types";
import { DocMarkdown, RendererBoundary } from "./DocInline";

/** Server-side comment body limit (store.TaskCommentMaxBytes). */
export const COMMENT_MAX_BYTES = 64 * 1024;

export type CommentsClient = {
  list: (taskId: number) => Promise<TaskCommentsData>;
  create: (taskId: number, body: string, csrf: string) => Promise<BoardComment>;
  csrf: () => string | null;
};

export const liveCommentsClient: CommentsClient = {
  list: fetchTaskComments,
  create: postTaskComment,
  csrf: readCsrfToken,
};

type ListState =
  | { status: "loading" }
  | { status: "ready"; comments: BoardComment[]; truncated: boolean }
  | { status: "forbidden" }
  | { status: "notfound" }
  | { status: "error" };

type WriteState = { status: "idle" } | { status: "sending" } | { status: "saved"; id: number } | { status: "refused"; message: string };

function statusOf(err: unknown): number | undefined {
  return (err as { status?: number } | null)?.status;
}

const encoder = new TextEncoder();

/** The words for a refused or failed write. Every server outcome gets its own
 * sentence; none of them reads as success. */
export function writeRefusal(err: unknown): string {
  if (!(err instanceof CommentWriteError)) {
    return "오류 — 코멘트를 저장하지 못했습니다.";
  }
  switch (err.code) {
    case "comment_empty":
      return "빈 본문 — 내용이 없어 저장하지 않았습니다.";
    case "comment_too_long":
      return `길이 초과 — 코멘트는 ${COMMENT_MAX_BYTES.toLocaleString("en-US")} 바이트까지입니다. 저장하지 않았습니다.`;
    case "not_found":
      return "태스크 없음 — 이 태스크가 존재하지 않아 저장하지 않았습니다.";
    case "csrf_rejected":
      return "CSRF 거부 — 페이지의 쓰기 토큰이 만료됐거나 맞지 않습니다. 페이지를 새로 고친 뒤 다시 보내세요. 저장하지 않았습니다.";
    case "origin_rejected":
      return "출처 거부 — 이 콘솔에서 보낸 요청이 아니어서 저장하지 않았습니다.";
    case "comment_secret_like":
      return "거부 — 비밀값처럼 보이는 문자열이 있어 저장하지 않았습니다.";
    case "comment_invalid":
      return "형식 거부 — 허용되지 않는 문자가 있어 저장하지 않았습니다.";
    case "author_not_accepted":
    case "unknown_field":
    case "invalid_form":
      return "형식 거부 — 요청 형식이 맞지 않아 저장하지 않았습니다(작성자는 서버가 정합니다).";
  }
  if (err.status === 401) {
    return "인증 없음 — 로그인 세션이 만료됐습니다. 페이지를 새로 고쳐 다시 로그인하세요. 저장하지 않았습니다.";
  }
  if (err.status === 403) {
    return "권한 없음 — 이 계정으로는 코멘트를 쓸 수 없습니다. 저장하지 않았습니다.";
  }
  if (err.status === 0) {
    return "오류 — 서버에 닿지 못했습니다. 저장됐는지 확인하려면 목록을 다시 불러오세요.";
  }
  return `오류 — 코멘트를 저장하지 못했습니다(HTTP ${err.status}).`;
}

function CommentBody({ body }: { body: string }) {
  const raw = <div className="qp-comment-raw">{body}</div>;
  return (
    <RendererBoundary body={body}>
      <Suspense fallback={raw}>
        <DocMarkdown body={body} section={null} />
      </Suspense>
    </RendererBoundary>
  );
}

function CommentList({ state, onReload }: { state: ListState; onReload: () => void }) {
  const reload = (
    <button type="button" className="qp-comment-reload" onClick={onReload}>
      다시 불러오기
    </button>
  );
  switch (state.status) {
    case "loading":
      return <p className="muted">코멘트를 불러오는 중…</p>;
    case "error":
      return (
        <p className="qp-unknown" role="note">
          오류 — 코멘트를 불러오지 못했습니다. 코멘트가 없다는 뜻이 아닙니다. {reload}
        </p>
      );
    case "forbidden":
      return (
        <p className="qp-unknown" role="note">
          권한 없음 — 코멘트를 읽을 권한이 없습니다(로그인 세션이 만료됐을 수 있습니다). 코멘트가 없다는 뜻이 아닙니다. {reload}
        </p>
      );
    case "notfound":
      return (
        <p className="qp-unknown" role="note">
          태스크 없음 — 이 id 의 태스크가 없어 코멘트도 없습니다.
        </p>
      );
    case "ready":
      if (state.comments.length === 0) {
        return <p className="muted">코멘트 없음 — 아직 아무도 코멘트를 남기지 않았습니다.</p>;
      }
      return (
        <>
          <ol className="qp-comments" aria-label="코멘트 (오래된 순)">
            {state.comments.map((comment) => (
              <li key={comment.id} className="qp-comment" data-comment-id={comment.id}>
                <div className="qp-comment-head">
                  <span className="qp-comment-author">{comment.author}</span>
                  <time dateTime={comment.created_at}>{comment.created_at}</time>
                  <span className="muted">#{comment.id}</span>
                </div>
                <div className="qp-doc-body qp-comment-body">
                  <CommentBody body={comment.body} />
                </div>
              </li>
            ))}
          </ol>
          {state.truncated ? (
            <p className="qp-unknown" role="note">
              일부만 보임 — 코멘트가 화면 상한보다 많아 앞부분만 보입니다. 전체는 <code>handoffkeep tasks comments</code> 로 읽습니다.
            </p>
          ) : null}
        </>
      );
  }
}

export function TaskComments({ taskId, client }: { taskId: number; client: CommentsClient }) {
  const [list, setList] = useState<ListState>({ status: "loading" });
  const [draft, setDraft] = useState("");
  const [write, setWrite] = useState<WriteState>({ status: "idle" });
  const [reloads, setReloads] = useState(0);
  const csrf = client.csrf();

  useEffect(() => {
    let cancelled = false;
    setList({ status: "loading" });
    client.list(taskId).then(
      (data) => {
        if (!cancelled) {
          setList({ status: "ready", comments: data.comments, truncated: data.truncated });
        }
      },
      (err) => {
        if (cancelled) {
          return;
        }
        const status = statusOf(err);
        setList(status === 404 ? { status: "notfound" } : status === 401 || status === 403 ? { status: "forbidden" } : { status: "error" });
      },
    );
    return () => {
      cancelled = true;
    };
  }, [taskId, client, reloads]);

  const reload = useCallback(() => setReloads((n) => n + 1), []);

  const bytes = encoder.encode(draft).length;
  const empty = draft.trim() === "";
  const tooLong = bytes > COMMENT_MAX_BYTES;

  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (csrf === null || write.status === "sending") {
      return;
    }
    if (empty) {
      setWrite({ status: "refused", message: "빈 본문 — 내용을 입력하세요. 보내지 않았습니다." });
      return;
    }
    if (tooLong) {
      setWrite({ status: "refused", message: `길이 초과 — ${bytes.toLocaleString("en-US")} / ${COMMENT_MAX_BYTES.toLocaleString("en-US")} 바이트. 보내지 않았습니다.` });
      return;
    }
    setWrite({ status: "sending" });
    client.create(taskId, draft, csrf).then(
      (comment) => {
        setDraft("");
        setWrite({ status: "saved", id: comment.id });
        // Only the server's own record is shown; a list that was not loaded
        // is re-read rather than painted over with one comment.
        setList((current) =>
          current.status === "ready"
            ? { ...current, comments: current.comments.some((c) => c.id === comment.id) ? current.comments : [...current.comments, comment] }
            : current,
        );
        if (list.status !== "ready") {
          reload();
        }
      },
      (err) => setWrite({ status: "refused", message: writeRefusal(err) }),
    );
  };

  // Typing must not drive the panel's task prev/next arrow keys.
  const onKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key !== "Escape") {
      event.stopPropagation();
    }
  };

  return (
    <div className="qp-comments-tab" data-comments-state={list.status === "ready" && list.comments.length === 0 ? "empty" : list.status}>
      <CommentList state={list} onReload={reload} />
      <form className="qp-comment-form" onSubmit={submit} aria-label="코멘트 작성">
        <p className="qp-comment-notice" role="note">
          <strong>코멘트는 데이터입니다 — 승인·발주가 아닙니다.</strong> 코멘트를 남겨도 태스크의 상태·우선순위·전이는 바뀌지 않고 어떤 레인에도
          전달되지 않습니다. 결정 답변은 <a href="/ui/decisions">결정함</a>에서 합니다.
        </p>
        {csrf === null ? (
          <p className="qp-unknown" role="note">
            쓰기 불가 — 이 페이지에 쓰기 토큰(CSRF)이 없습니다. 페이지를 새로 고치세요.
          </p>
        ) : null}
        <fieldset disabled={csrf === null}>
          <label className="qp-comment-label" htmlFor={`qp-comment-${taskId}`}>
            새 코멘트 <span className="muted">— 텍스트와 최소 마크다운, HTML 은 렌더하지 않습니다. 작성자는 로그인한 계정으로 서버가 기록합니다.</span>
          </label>
          <textarea
            id={`qp-comment-${taskId}`}
            name="body"
            className="qp-comment-input"
            rows={4}
            value={draft}
            onChange={(event) => {
              setDraft(event.target.value);
              if (write.status === "refused" || write.status === "saved") {
                setWrite({ status: "idle" });
              }
            }}
            onKeyDown={onKeyDown}
          />
          <div className="qp-comment-actions">
            <span className={tooLong ? "qp-comment-count qp-comment-over" : "qp-comment-count muted"} data-testid="comment-bytes">
              {bytes.toLocaleString("en-US")} / {COMMENT_MAX_BYTES.toLocaleString("en-US")} 바이트
            </span>
            <button type="submit" className="qp-comment-submit" disabled={write.status === "sending"}>
              {write.status === "sending" ? "보내는 중…" : "코멘트 남기기"}
            </button>
          </div>
        </fieldset>
        <p className="qp-comment-result" role="status" aria-live="polite" data-write-state={write.status}>
          {write.status === "saved" ? `저장됨 — 코멘트 #${write.id}` : write.status === "refused" ? write.message : ""}
        </p>
      </form>
    </div>
  );
}
