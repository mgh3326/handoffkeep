// Header search box (2410 §5 AC4): one per page, "/" or ⌘K to focus. It asks
// the server (/ui/api/search, every state including merged and dropped) —
// unlike the queue toolbar input, which only narrows the rows already loaded.
// Results are listed with ↑↓ / Enter / Esc; picking one hands the id to the
// host (the queue opens its panel, other pages navigate to /ui/tasks/<id>).

import { useCallback, useEffect, useId, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from "react";
import { stateLabel, StateIcon } from "./StateIcon";
import { fetchTaskSearch, highlightParts, isSearchShortcut, safeTaskId, stripMarkers, type SearchFn, type SearchHit } from "./search";

type Lookup =
  | { kind: "idle" }
  | { kind: "loading"; query: string }
  | { kind: "ok"; query: string; results: SearchHit[]; hasMore: boolean }
  | { kind: "error"; query: string; reason: "forbidden" | "invalid" | "failed" };

const DEBOUNCE_MS = 150;

function failureReason(err: unknown): "forbidden" | "invalid" | "failed" {
  const status = (err as { status?: number } | null)?.status;
  if (status === 401 || status === 403) {
    return "forbidden";
  }
  return status === 400 ? "invalid" : "failed";
}

/** The store snippet as text runs; only its own markers become <mark>. */
function Highlighted({ snippet }: { snippet: string }) {
  return (
    <>
      {highlightParts(snippet).map((part, index) =>
        part.hit ? (
          <mark key={index} className="qp-gs-hit">
            {part.text}
          </mark>
        ) : (
          <span key={index}>{part.text}</span>
        ),
      )}
    </>
  );
}

function ResultRow({ hit }: { hit: SearchHit }) {
  // A title match comes back as the highlighted title itself; anything else
  // (a comment match, a headline cut from a long title) keeps the title as
  // the row and shows the matching excerpt beneath it.
  const titleMatch = stripMarkers(hit.snippet) === hit.title;
  return (
    <>
      <StateIcon state={hit.state} />
      <span className="qp-gs-main">
        <span className="qp-gs-title">
          <span className="qp-gs-id">#{hit.id}</span>{" "}
          {titleMatch ? <Highlighted snippet={hit.snippet} /> : hit.title}
        </span>
        {titleMatch ? null : (
          <span className="qp-gs-snippet muted">
            <Highlighted snippet={hit.snippet} />
          </span>
        )}
      </span>
      <span className="qp-gs-meta muted">
        {stateLabel(hit.state)} · {hit.lane}
      </span>
    </>
  );
}

export function GlobalSearch({
  onPick,
  search = fetchTaskSearch,
  debounceMs = DEBOUNCE_MS,
}: {
  onPick: (id: number, input: HTMLInputElement) => void;
  search?: SearchFn;
  debounceMs?: number;
}) {
  const [value, setValue] = useState("");
  const [open, setOpen] = useState(false);
  const [lookup, setLookup] = useState<Lookup>({ kind: "idle" });
  const [active, setActive] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const seqRef = useRef(0);
  const abortRef = useRef<AbortController | null>(null);
  const timerRef = useRef(0);
  // Enter pressed before the answer for the current text arrived: pick the
  // first row of that answer — never of an older one.
  const pendingEnterRef = useRef<string | null>(null);
  const listId = useId();

  const pick = useCallback(
    (hit: SearchHit) => {
      const id = safeTaskId(hit.id);
      if (id === null || inputRef.current === null) {
        return;
      }
      setOpen(false);
      onPick(id, inputRef.current);
    },
    [onPick],
  );

  const run = useCallback(
    (query: string) => {
      window.clearTimeout(timerRef.current);
      abortRef.current?.abort();
      const mine = ++seqRef.current;
      const controller = new AbortController();
      abortRef.current = controller;
      setLookup({ kind: "loading", query });
      search(query, controller.signal).then(
        (page) => {
          if (mine !== seqRef.current) {
            return;
          }
          setLookup({ kind: "ok", query, results: page.results, hasMore: page.hasMore });
          setActive(0);
          if (pendingEnterRef.current === query) {
            pendingEnterRef.current = null;
            if (page.results.length > 0) {
              pick(page.results[0]);
            }
          }
        },
        (err) => {
          if (mine !== seqRef.current || controller.signal.aborted) {
            return;
          }
          pendingEnterRef.current = null;
          setLookup({ kind: "error", query, reason: failureReason(err) });
        },
      );
    },
    [search, pick],
  );

  const change = (next: string) => {
    setValue(next);
    setOpen(true);
    pendingEnterRef.current = null;
    window.clearTimeout(timerRef.current);
    const query = next.trim();
    if (query === "") {
      seqRef.current++;
      abortRef.current?.abort();
      setLookup({ kind: "idle" });
      return;
    }
    timerRef.current = window.setTimeout(() => run(query), debounceMs);
  };

  useEffect(
    () => () => {
      window.clearTimeout(timerRef.current);
      abortRef.current?.abort();
    },
    [],
  );

  // Global shortcut. isSearchShortcut refuses typing targets (inputs,
  // textareas incl. the comment form, selects, contenteditable) and IME
  // composition, and there the key is left entirely to the page.
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (!isSearchShortcut(event)) {
        return;
      }
      event.preventDefault();
      inputRef.current?.focus();
      inputRef.current?.select();
      setOpen(true);
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);

  const current = value.trim();
  const settled = lookup.kind === "ok" && lookup.query === current ? lookup : null;
  const results = settled?.results ?? [];

  const onKeyDown = (event: ReactKeyboardEvent<HTMLInputElement>) => {
    if (event.nativeEvent.isComposing) {
      return;
    }
    switch (event.key) {
      case "ArrowDown":
      case "ArrowUp": {
        event.preventDefault();
        setOpen(true);
        if (results.length > 0) {
          const step = event.key === "ArrowDown" ? 1 : -1;
          setActive((i) => (i + step + results.length) % results.length);
        }
        return;
      }
      case "Enter": {
        event.preventDefault();
        if (current === "") {
          return;
        }
        if (settled !== null) {
          const hit = results[Math.min(active, results.length - 1)];
          if (hit !== undefined) {
            pick(hit);
          }
          return;
        }
        // Answer for this text not in yet: flush the debounce and pick its
        // first row when it lands.
        pendingEnterRef.current = current;
        setOpen(true);
        if (!(lookup.kind === "loading" && lookup.query === current)) {
          run(current);
        }
        return;
      }
      case "Escape": {
        // Esc belongs to the search first: closing the list, then clearing
        // the text, never also closes an open task panel (which listens at
        // the document). Only an empty, closed box lets Esc through.
        pendingEnterRef.current = null;
        if (showPanel || value !== "") {
          event.preventDefault();
          event.nativeEvent.stopPropagation();
        }
        if (showPanel) {
          setOpen(false);
        } else {
          if (value !== "") {
            change("");
          }
          setOpen(false);
          inputRef.current?.blur();
        }
        return;
      }
    }
  };

  const showPanel = open && current !== "";
  const activeId = settled !== null && results.length > 0 ? `${listId}-opt-${Math.min(active, results.length - 1)}` : undefined;

  let status: { text: string; tone: "muted" | "warn" | "danger"; kind: string } | null = null;
  if (showPanel) {
    if (settled === null && lookup.kind === "error" && lookup.query === current) {
      status =
        lookup.reason === "forbidden"
          ? { text: "권한 없음 — 이 계정으로는 검색할 수 없습니다", tone: "danger", kind: "forbidden" }
          : lookup.reason === "invalid"
            ? { text: "질의를 처리할 수 없습니다 — 512바이트 이하로 입력하세요", tone: "warn", kind: "invalid" }
            : { text: "조회 실패 — 결과가 없다는 뜻이 아닙니다. 잠시 뒤 다시 입력하세요", tone: "danger", kind: "failed" };
    } else if (settled === null) {
      status = { text: "검색 중…", tone: "muted", kind: "loading" };
    } else if (results.length === 0) {
      status = { text: `결과 없음 — “${current}” 와 맞는 태스크가 없습니다`, tone: "muted", kind: "empty" };
    }
  }

  return (
    <div className="qp-gs" role="search">
      <input
        ref={inputRef}
        id="qp-global-search"
        type="search"
        role="combobox"
        aria-label="전체 검색"
        aria-autocomplete="list"
        aria-expanded={showPanel}
        aria-controls={listId}
        aria-activedescendant={showPanel ? activeId : undefined}
        placeholder="전체 검색 — 제목 · #id  ( / · ⌘K )"
        autoComplete="off"
        spellCheck={false}
        value={value}
        onChange={(event) => change(event.target.value)}
        onFocus={() => setOpen(true)}
        onBlur={(event) => {
          if (!event.currentTarget.parentElement?.contains(event.relatedTarget as Node | null)) {
            setOpen(false);
          }
        }}
        onKeyDown={onKeyDown}
      />
      {showPanel ? (
        <div className="qp-gs-panel" onMouseDown={(event) => event.preventDefault()}>
          <p className="qp-gs-scope muted">전체 검색 · 닫힌 태스크(merged · dropped) 포함 · 서버 조회</p>
          {status !== null ? (
            <p className={status.tone === "muted" ? "qp-gs-status muted" : "qp-gs-status qp-status-warn"} role={status.tone === "danger" ? "alert" : "status"} data-search-status={status.kind}>
              {status.text}
            </p>
          ) : null}
          <ul id={listId} role="listbox" aria-label="검색 결과" className="qp-gs-list">
            {results.map((hit, index) => (
              <li
                key={hit.id}
                id={`${listId}-opt-${index}`}
                role="option"
                aria-selected={index === Math.min(active, results.length - 1)}
                className="qp-gs-opt"
                data-task-id={hit.id}
                onMouseEnter={() => setActive(index)}
                onClick={() => pick(hit)}
              >
                <ResultRow hit={hit} />
              </li>
            ))}
          </ul>
          {settled !== null && results.length > 0 ? (
            <p className={settled.hasMore ? "qp-gs-foot qp-status-warn" : "qp-gs-foot muted"} data-search-status={settled.hasMore ? "more" : "complete"}>
              {settled.hasMore
                ? `더 있음 — 앞의 ${results.length}건만 보입니다. 질의를 좁혀 주세요`
                : `${results.length}건 · 맞는 태스크 전부`}
            </p>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}
