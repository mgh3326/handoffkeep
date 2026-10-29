// /ui/docs — the console document browser (hk 2245). Three panes: the
// top-level prefix tree, the document list for the selected prefix, and the
// selected document rendered through the shared markdown renderer. Read-only:
// the list comes from GET /ui/api/docs, prefixes from /ui/api/docs/prefixes,
// and a selected body from the existing /ui/api/board/doc.
//
// policy/* documents are pinned at the top of the list and in the tree: they
// are the operator's standing policy set and must not rank below newer
// reports just because their keys sort earlier.

import { lazy, Suspense, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { fetchBoardDoc } from "../../board/api";
import type { BoardDoc } from "../../board/types";
import { RendererBoundary } from "../RendererBoundary";
import { fetchDocPrefixes, fetchDocs } from "./api";
import type { DocPrefix, DocSummary } from "./types";

// The renderer is the shared lazy chunk (src/queue-proto/DocMarkdown), loaded
// on first document read. The docs entry keeps its own dynamic edge so the
// queue's lazy renderer boundary is untouched.
const DocMarkdown = lazy(() => import("../DocMarkdown"));

const PINNED_PREFIX = "policy/";
const PINNED_LIMIT = 20;
const PAGE_LIMIT = 100;

export type DocsLocation = { prefix: string; key: string | null };

/** Reads the deep-link state from a query string. Unknown values still name a
 * prefix — the server answers with an empty list rather than an error. */
export function parseDocsQuery(search: string): DocsLocation {
  const params = new URLSearchParams(search);
  const key = params.get("key");
  return { prefix: params.get("prefix") ?? "", key: key === "" ? null : key };
}

/** The page URL for a prefix/document selection; prefix stays out when empty
 * so the plain /ui/docs URL is stable. */
export function docsHref(prefix: string, key: string | null): string {
  const params = new URLSearchParams();
  if (prefix !== "") {
    params.set("prefix", prefix);
  }
  if (key) {
    params.set("key", key);
  }
  const suffix = params.toString();
  return suffix === "" ? "/ui/docs" : `/ui/docs?${suffix}`;
}

/** policy/ first, then the server's prefix order. */
export function orderPrefixes(prefixes: DocPrefix[]): DocPrefix[] {
  return [...prefixes].sort((a, b) => {
    const ap = a.prefix === PINNED_PREFIX ? 0 : 1;
    const bp = b.prefix === PINNED_PREFIX ? 0 : 1;
    if (ap !== bp) {
      return ap - bp;
    }
    return a.prefix.localeCompare(b.prefix);
  });
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

function formatSize(bytes: number): string {
  if (bytes < 1024) {
    return `${bytes} B`;
  }
  if (bytes < 1024 * 1024) {
    return `${(bytes / 1024).toFixed(1)} KB`;
  }
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

/** Clipboard write with a synchronous fallback for browsers that refuse the
 * async API; false means the operator must copy the URL from the address bar. */
export async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch {
    // fall through to the selection-based copy
  }
  try {
    const area = document.createElement("textarea");
    area.value = text;
    area.setAttribute("readonly", "");
    area.style.position = "fixed";
    area.style.opacity = "0";
    document.body.appendChild(area);
    area.select();
    const copied = document.execCommand?.("copy") ?? false;
    document.body.removeChild(area);
    return copied;
  } catch {
    return false;
  }
}

function dedupe(items: DocSummary[]): DocSummary[] {
  const seen = new Set<string>();
  return items.filter((item) => {
    if (seen.has(item.key)) {
      return false;
    }
    seen.add(item.key);
    return true;
  });
}

function DocRow({
  doc,
  prefix,
  selected,
  onSelect,
}: {
  doc: DocSummary;
  prefix: string;
  selected: boolean;
  onSelect: (key: string) => void;
}) {
  return (
    <a
      href={docsHref(prefix, doc.key)}
      className={`dx-row${selected ? " on" : ""}`}
      aria-current={selected ? "true" : undefined}
      data-doc-key={doc.key}
      onClick={(event) => {
        if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey || event.button !== 0) {
          return;
        }
        event.preventDefault();
        onSelect(doc.key);
      }}
    >
      <span className="dx-row-title">{doc.title === "" ? doc.key : doc.title}</span>
      <span className="dx-row-key">{doc.key}</span>
      <span className="dx-row-meta">
        <span>{doc.kind}</span>
        {doc.session !== "" ? <span>{doc.session}</span> : null}
        <span>{formatSize(doc.size)}</span>
        <span>{formatDay(doc.updated_at)}</span>
      </span>
    </a>
  );
}

type ListState = { prefix: string; items: DocSummary[]; hasMore: boolean; next: string | undefined };

const REASON_TEXT: Record<string, string> = {
  json: "JSON 문서는 본문으로 렌더하지 않습니다",
  too_large: "인라인 렌더 한도(256KiB)를 넘는 문서입니다",
  not_text: "텍스트가 아닌 문서입니다",
  invalid_key: "문서 key 모양이 아닙니다",
};

type BodyState =
  | { status: "loading" }
  | { status: "ready"; doc: BoardDoc }
  | { status: "missing" }
  | { status: "forbidden" }
  | { status: "error" }
  | { status: "unsupported"; reason: string; body: string };

/** One document body through the same read-only BFF the task overview uses.
 * Only a markdown body reaches the renderer; everything else stays raw text. */
function DocBody({ docKey }: { docKey: string }) {
  const [state, setState] = useState<BodyState>({ status: "loading" });

  useEffect(() => {
    let cancelled = false;
    setState({ status: "loading" });
    fetchBoardDoc(docKey).then(
      (doc) => {
        if (cancelled) {
          return;
        }
        setState(
          doc.format === "markdown"
            ? { status: "ready", doc }
            : { status: "unsupported", reason: doc.reason ?? "", body: doc.body },
        );
      },
      (err) => {
        if (cancelled) {
          return;
        }
        const status = (err as { status?: number } | null)?.status;
        setState(
          status === 404
            ? { status: "missing" }
            : status === 401 || status === 403
              ? { status: "forbidden" }
              : status === 400
                ? { status: "unsupported", reason: "invalid_key", body: "" }
                : { status: "error" },
        );
      },
    );
    return () => {
      cancelled = true;
    };
  }, [docKey]);

  if (state.status === "loading") {
    return <p className="dx-empty">본문을 불러오는 중…</p>;
  }
  if (state.status === "missing") {
    return (
      <p className="dx-empty" role="note">
        문서 없음 — hk 에 이 key 의 문서가 없습니다.
      </p>
    );
  }
  if (state.status === "forbidden") {
    return (
      <p className="dx-empty" role="note">
        권한 없음 — 이 문서를 읽을 권한이 없습니다.
      </p>
    );
  }
  if (state.status === "error") {
    return (
      <p className="dx-error" role="alert">
        본문을 불러오지 못했습니다.
      </p>
    );
  }
  if (state.status === "unsupported") {
    return (
      <>
        <p className="dx-warn" role="note">
          형식 미지원 — {REASON_TEXT[state.reason] ?? "인라인으로 렌더할 수 없는 문서입니다"}. 원문 텍스트만 보입니다.
        </p>
        <pre className="qp-doc-raw">{state.body}</pre>
      </>
    );
  }
  return (
    <RendererBoundary key={`${state.doc.key}@${state.doc.sha256}`} body={state.doc.body}>
      <Suspense fallback={<p className="dx-empty">본문 렌더러를 불러오는 중…</p>}>
        <DocMarkdown body={state.doc.body} section={null} />
      </Suspense>
    </RendererBoundary>
  );
}

export function DocsApp() {
  const initial = parseDocsQuery(typeof window === "undefined" ? "" : window.location.search);
  const [prefix, setPrefix] = useState(initial.prefix);
  const [selectedKey, setSelectedKey] = useState<string | null>(initial.key);
  const [query, setQuery] = useState("");

  const [prefixes, setPrefixes] = useState<DocPrefix[] | null>(null);
  const [prefixesFailed, setPrefixesFailed] = useState(false);
  const [pinned, setPinned] = useState<DocSummary[]>([]);
  const [pinnedFailed, setPinnedFailed] = useState(false);
  const [list, setList] = useState<ListState | null>(null);
  const [listLoading, setListLoading] = useState(true);
  const [listFailed, setListFailed] = useState(false);
  const [copy, setCopy] = useState<"idle" | "copied" | "failed">("idle");
  const listSeq = useRef(0);

  useEffect(() => {
    let cancelled = false;
    fetchDocPrefixes().then(
      (body) => {
        if (!cancelled) {
          setPrefixes(body.prefixes);
        }
      },
      () => {
        if (!cancelled) {
          setPrefixesFailed(true);
        }
      },
    );
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    let cancelled = false;
    fetchDocs(PINNED_PREFIX, undefined, PINNED_LIMIT).then(
      (page) => {
        if (!cancelled) {
          setPinned(page.documents);
        }
      },
      () => {
        if (!cancelled) {
          setPinnedFailed(true);
        }
      },
    );
    return () => {
      cancelled = true;
    };
  }, []);

  const load = useCallback(async (forPrefix: string, before?: string) => {
    const mine = ++listSeq.current;
    setListLoading(true);
    setListFailed(false);
    try {
      const page = await fetchDocs(forPrefix, before, PAGE_LIMIT);
      if (mine !== listSeq.current) {
        return;
      }
      setList((previous) =>
        before !== undefined && previous !== null && previous.prefix === forPrefix
          ? {
              prefix: forPrefix,
              items: dedupe([...previous.items, ...page.documents]),
              hasMore: page.has_more,
              next: page.next_before,
            }
          : { prefix: forPrefix, items: page.documents, hasMore: page.has_more, next: page.next_before },
      );
    } catch {
      if (mine === listSeq.current) {
        setListFailed(true);
        if (before === undefined) {
          setList(null);
        }
      }
    } finally {
      if (mine === listSeq.current) {
        setListLoading(false);
      }
    }
  }, []);

  useEffect(() => {
    void load(prefix);
  }, [prefix, load]);

  useEffect(() => {
    if (copy === "idle") {
      return;
    }
    const timer = setTimeout(() => setCopy("idle"), 2000);
    return () => clearTimeout(timer);
  }, [copy]);

  const selectPrefix = (next: string) => {
    setPrefix(next);
    setList(null);
    window.history.replaceState(null, "", docsHref(next, selectedKey));
  };

  const selectDoc = (key: string) => {
    setSelectedKey(key);
    setCopy("idle");
    window.history.replaceState(null, "", docsHref(prefix, key));
  };

  const copyLink = async () => {
    if (selectedKey === null) {
      return;
    }
    const href = new URL(docsHref(prefix, selectedKey), window.location.origin).toString();
    setCopy((await copyText(href)) ? "copied" : "failed");
  };

  const needle = query.trim().toLowerCase();
  const matches = (doc: DocSummary) =>
    needle === "" || doc.key.toLowerCase().includes(needle) || doc.title.toLowerCase().includes(needle);

  const pinnedVisible = pinned.filter(matches);
  const showPinned = !prefix.startsWith(PINNED_PREFIX) && pinnedVisible.length > 0;
  const pinnedKeys = new Set(showPinned ? pinnedVisible.map((doc) => doc.key) : []);
  const currentItems = list !== null && list.prefix === prefix ? list.items : [];
  const listVisible = currentItems.filter((doc) => matches(doc) && !pinnedKeys.has(doc.key));

  const known = useMemo(() => {
    const map = new Map<string, DocSummary>();
    for (const doc of [...pinned, ...currentItems]) {
      map.set(doc.key, doc);
    }
    return map;
  }, [pinned, currentItems]);
  const selected = selectedKey === null ? undefined : known.get(selectedKey);

  const orderedPrefixes = prefixes === null ? [] : orderPrefixes(prefixes);
  const total = prefixes === null ? null : prefixes.reduce((sum, item) => sum + item.count, 0);

  return (
    <div className="dx-root">
      <header className="dx-head">
        <h1>문서 — Documents</h1>
        <p className="dx-sub">
          hk 문서를 key prefix 로 찾아 읽습니다. policy 는 고정, 본문은 읽기 전용입니다.
        </p>
      </header>

      <nav className="dx-rail" aria-label="문서 분류">
        <p className="dx-rail-h">분류</p>
        <button
          type="button"
          className={`dx-rail-item${prefix === "" ? " on" : ""}`}
          aria-current={prefix === "" ? "page" : undefined}
          onClick={() => selectPrefix("")}
        >
          <span>전체</span>
          {total !== null ? <span className="dx-count">{total}</span> : null}
        </button>
        {orderedPrefixes.map((item) => (
          <button
            key={item.prefix}
            type="button"
            className={`dx-rail-item${prefix === item.prefix ? " on" : ""}${
              item.prefix === PINNED_PREFIX ? " pinned" : ""
            }`}
            aria-current={prefix === item.prefix ? "page" : undefined}
            onClick={() => selectPrefix(item.prefix)}
          >
            <span className="dx-rail-name">{item.prefix}</span>
            <span className="dx-count">{item.count}</span>
          </button>
        ))}
        {prefixesFailed ? (
          <p className="dx-warn" role="note">
            분류를 불러오지 못했습니다 — 전체 목록은 계속 볼 수 있습니다.
          </p>
        ) : null}
      </nav>

      <section className="dx-list" aria-label="문서 목록">
        <div className="dx-list-head">
          <input
            className="dx-search"
            aria-label="문서 검색"
            placeholder="key 또는 제목 검색"
            value={query}
            onChange={(event) => setQuery(event.target.value)}
          />
          <span className="dx-list-count" role="status">
            {listLoading ? "불러오는 중…" : `${pinnedKeys.size + listVisible.length}건`}
          </span>
        </div>
        {listFailed && currentItems.length === 0 ? (
          <p className="dx-error" role="alert">
            문서 목록을 불러오지 못했습니다 — 다시 시도해 주세요.
          </p>
        ) : null}
        {showPinned ? (
          <div className="dx-group" data-group="pinned">
            <p className="dx-group-h">고정 · policy</p>
            {pinnedVisible.map((doc) => (
              <DocRow key={doc.key} doc={doc} prefix={prefix} selected={doc.key === selectedKey} onSelect={selectDoc} />
            ))}
          </div>
        ) : null}
        {pinnedFailed && prefix === "" ? (
          <p className="dx-warn" role="note">
            고정 policy 목록을 불러오지 못했습니다.
          </p>
        ) : null}
        <div className="dx-group" data-group="prefix">
          <p className="dx-group-h">{prefix === "" ? "모든 문서" : prefix}</p>
          {listVisible.length === 0 && !listLoading ? (
            <p className="dx-empty">
              {needle === "" ? "이 분류에 문서가 없습니다." : "불러온 페이지 안에 일치하는 문서가 없습니다."}
            </p>
          ) : null}
          {listVisible.map((doc) => (
            <DocRow key={doc.key} doc={doc} prefix={prefix} selected={doc.key === selectedKey} onSelect={selectDoc} />
          ))}
        </div>
        {list !== null && list.prefix === prefix && list.hasMore ? (
          <button type="button" className="hk-btn dx-more" onClick={() => void load(prefix, list.next)}>
            {listLoading ? "불러오는 중…" : "더 보기"}
          </button>
        ) : null}
      </section>

      <section className="dx-reader" aria-label="문서 본문">
        {selectedKey === null ? (
          <p className="dx-empty">목록에서 문서를 선택하세요.</p>
        ) : (
          <>
            <div className="dx-reader-head">
              <h2>{selected?.title || selectedKey}</h2>
              <div className="dx-reader-actions">
                <button type="button" className="hk-btn" onClick={() => void copyLink()}>
                  {copy === "copied" ? "복사됨" : copy === "failed" ? "복사 실패" : "링크 복사"}
                </button>
                <a className="hk-btn" href={`/ui/doc/${selectedKey}`}>
                  원문
                </a>
              </div>
            </div>
            {selected !== undefined ? (
              <p className="dx-reader-meta">
                <span className="dx-mono">{selected.key}</span>
                <span>{selected.kind}</span>
                {selected.session !== "" ? <span>{selected.session}</span> : null}
                <span>{formatSize(selected.size)}</span>
                <span>{formatDay(selected.updated_at)}</span>
              </p>
            ) : null}
            <DocBody key={selectedKey} docKey={selectedKey} />
          </>
        )}
      </section>
    </div>
  );
}
