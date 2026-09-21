// DocInline — the task overview's body document. It fetches the document by
// key from the read-only board BFF and keeps five outcomes apart: loading /
// 문서 없음 / 권한 없음 / 형식 미지원 / 오류. Only a "markdown" document is
// handed to the renderer, which is loaded lazily on first use; everything
// else is shown as escaped raw text. Nothing here writes anywhere.

import { Component, lazy, Suspense, useEffect, useState, type ReactNode } from "react";
import { fetchBoardDoc } from "../board/api";
import type { BoardDoc } from "../board/types";
import { docPageHref, parseBodyDoc } from "./bodydoc";

// The dynamic import is the list/renderer split point: this module is in the
// queue entry's static graph, DocMarkdown is not.
const DocMarkdown = lazy(() => import("./DocMarkdown"));

export type FetchDoc = (key: string) => Promise<BoardDoc>;

type DocState =
  | { status: "loading" }
  | { status: "ready"; doc: BoardDoc }
  | { status: "missing" }
  | { status: "forbidden" }
  | { status: "unsupported"; reason: string; doc?: BoardDoc }
  | { status: "error" };

const REASON_TEXT: Record<string, string> = {
  json: "JSON 문서는 본문으로 렌더하지 않습니다",
  too_large: "인라인 렌더 한도(256KiB)를 넘는 문서입니다",
  not_text: "텍스트가 아닌 문서입니다",
  invalid_key: "문서 key 모양이 아닙니다",
};

function RawText({ body }: { body: string }) {
  return <pre className="qp-doc-raw">{body}</pre>;
}

/** Catches a failed renderer chunk load or a render error and falls back to
 * the raw text — the document stays readable, the reason is stated. */
class RendererBoundary extends Component<{ body: string; children: ReactNode }, { failed: boolean }> {
  state = { failed: false };
  static getDerivedStateFromError() {
    return { failed: true };
  }
  render() {
    if (this.state.failed) {
      return (
        <>
          <p className="qp-unknown" role="note">
            오류 — 렌더러를 불러오지 못해 원문 텍스트로 보입니다.
          </p>
          <RawText body={this.props.body} />
        </>
      );
    }
    return this.props.children;
  }
}

function statusOf(err: unknown): number | undefined {
  return (err as { status?: number } | null)?.status;
}

export function DocInline({ bodyDoc, fetchDoc = fetchBoardDoc }: { bodyDoc: string; fetchDoc?: FetchDoc }) {
  const ref = parseBodyDoc(bodyDoc);
  const key = ref?.key ?? null;
  // The state remembers which key it belongs to: a late response for a
  // previous task's document can never paint over the current one.
  const [state, setState] = useState<{ key: string; value: DocState } | null>(null);
  const [sectionFound, setSectionFound] = useState<boolean | null>(null);

  useEffect(() => {
    if (key === null) {
      return;
    }
    let cancelled = false;
    setState({ key, value: { status: "loading" } });
    setSectionFound(null);
    fetchDoc(key).then(
      (doc) => {
        if (cancelled) {
          return;
        }
        setState({
          key,
          value: doc.format === "markdown" ? { status: "ready", doc } : { status: "unsupported", reason: doc.reason ?? "", doc },
        });
      },
      (err) => {
        if (cancelled) {
          return;
        }
        const status = statusOf(err);
        const value: DocState =
          status === 404
            ? { status: "missing" }
            : status === 401 || status === 403
              ? { status: "forbidden" }
              : status === 400
                ? { status: "unsupported", reason: "invalid_key" }
                : { status: "error" };
        setState({ key, value });
      },
    );
    return () => {
      cancelled = true;
    };
  }, [key, fetchDoc]);

  if (ref === null) {
    return (
      <div className="qp-doc" data-doc-state="unsupported">
        <p className="qp-unknown" role="note">
          형식 미지원 — body_doc 값이 문서 key 모양이 아닙니다: <code>{bodyDoc}</code>
        </p>
      </div>
    );
  }

  const current: DocState = state !== null && state.key === ref.key ? state.value : { status: "loading" };
  const doc = current.status === "ready" || current.status === "unsupported" ? current.doc : undefined;
  return (
    <div className="qp-doc" data-doc-state={current.status} aria-busy={current.status === "loading"}>
      <p className="qp-doc-source">
        본문 문서 <code>{ref.key}</code>
        {ref.section !== null ? (
          <>
            {" "}
            · 절 <code>#{ref.section}</code>
            {sectionFound === false ? <span className="muted"> (문서에서 찾지 못함)</span> : null}
          </>
        ) : null}{" "}
        · <a href={docPageHref(ref.key)}>원문 보기</a>
        {doc ? (
          <span className="muted">
            {" "}
            · 갱신 <time>{doc.updated_at}</time> · sha256 <code>{doc.sha256.slice(0, 12)}</code>
          </span>
        ) : null}
      </p>
      {current.status === "loading" ? <p className="muted">본문 문서를 불러오는 중…</p> : null}
      {current.status === "missing" ? (
        <p className="qp-unknown" role="note">
          문서 없음 — hk 에 이 key 의 문서가 아직 없습니다.
        </p>
      ) : null}
      {current.status === "forbidden" ? (
        <p className="qp-unknown" role="note">
          권한 없음 — 이 문서를 읽을 권한이 없습니다(로그인 세션이 만료됐을 수 있습니다).
        </p>
      ) : null}
      {current.status === "error" ? (
        <p className="qp-unknown" role="note">
          오류 — 본문 문서를 불러오지 못했습니다.
        </p>
      ) : null}
      {current.status === "unsupported" ? (
        <>
          <p className="qp-unknown" role="note">
            형식 미지원 — {REASON_TEXT[current.reason] ?? "인라인으로 렌더할 수 없는 문서입니다"}. 원문 텍스트만 보입니다.
          </p>
          {current.doc ? <RawText body={current.doc.body} /> : null}
        </>
      ) : null}
      {current.status === "ready" ? (
        <RendererBoundary key={`${ref.key}@${current.doc.sha256}`} body={current.doc.body}>
          <Suspense fallback={<p className="muted">본문 렌더러를 불러오는 중…</p>}>
            <DocMarkdown body={current.doc.body} section={ref.section} onSection={setSectionFound} />
          </Suspense>
        </RendererBoundary>
      ) : null}
    </div>
  );
}
