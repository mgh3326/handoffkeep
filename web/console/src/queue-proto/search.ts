// Console-wide task search client (/ui/api/search, 2410 §5 AC4). The BFF
// forwards the store's title and snippet verbatim: the snippet carries
// ts_headline's "<b>"/"</b>" markers around unescaped source text. Nothing
// here ever turns either into markup — highlightParts only splits on the two
// exact marker strings and every part is rendered as a React text node.

import { HttpError } from "../board/api";

export type SearchHit = {
  id: number;
  state: string;
  lane: string;
  kind: string;
  title: string;
  snippet: string;
  /** The id-lookup hit for a "#<n>" / "<n>" query; always first. */
  exact?: boolean;
};

export type SearchPage = {
  query: string;
  results: SearchHit[];
  /** The page was cut at the cap: the rows shown are not every match. */
  hasMore: boolean;
  limit: number;
};

export type SearchFn = (query: string, signal: AbortSignal) => Promise<SearchPage>;

type SearchResponse = {
  query: string;
  scope: string;
  results: SearchHit[] | null;
  has_more: boolean;
  limit: number;
};

/** Live search: rejects with HttpError on a non-2xx answer so the caller can
 * tell "no permission" and "bad query" from a failed lookup. */
export const fetchTaskSearch: SearchFn = async (query, signal) => {
  const response = await fetch(`/ui/api/search?scope=tasks&q=${encodeURIComponent(query)}`, { signal });
  if (!response.ok) {
    throw new HttpError(response.status);
  }
  const body = (await response.json()) as SearchResponse;
  if (!Array.isArray(body.results) || typeof body.has_more !== "boolean") {
    throw new Error("malformed search response");
  }
  return { query: body.query, results: body.results, hasMore: body.has_more, limit: body.limit };
};

export type HighlightPart = { text: string; hit: boolean };

const MARKER = /(<b>|<\/b>)/;

/** Splits a store snippet into plain and highlighted runs. Only the literal
 * "<b>" and "</b>" markers toggle the highlight; every other character —
 * tags, entities, quotes — stays text. */
export function highlightParts(snippet: string): HighlightPart[] {
  const parts: HighlightPart[] = [];
  let hit = false;
  for (const piece of snippet.split(MARKER)) {
    if (piece === "<b>") {
      hit = true;
    } else if (piece === "</b>") {
      hit = false;
    } else if (piece !== "") {
      parts.push({ text: piece, hit });
    }
  }
  return parts;
}

export function stripMarkers(snippet: string): string {
  return snippet.split(MARKER).filter((p) => p !== "<b>" && p !== "</b>").join("");
}

/** Path ids only from a strict shape — a result id never reaches a URL
 * unchecked. */
export function safeTaskId(id: unknown): number | null {
  return typeof id === "number" && Number.isSafeInteger(id) && id > 0 && /^[1-9][0-9]{0,14}$/.test(String(id)) ? id : null;
}

const TEXT_INPUT_TYPES = new Set(["", "text", "search", "email", "url", "tel", "password", "number", "date", "datetime-local", "month", "time", "week"]);

/** True when a key press belongs to whatever the viewer is typing into:
 * text inputs, textareas, selects and contenteditable regions. Global
 * shortcuts never fire there. */
export function isTypingTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) {
    return false;
  }
  if (target.isContentEditable || target.closest("[contenteditable]:not([contenteditable='false'])") !== null) {
    return true;
  }
  if (target instanceof HTMLTextAreaElement || target instanceof HTMLSelectElement) {
    return true;
  }
  if (target instanceof HTMLInputElement) {
    return TEXT_INPUT_TYPES.has(target.type.toLowerCase());
  }
  return false;
}

/** "/" or ⌘K / Ctrl+K, outside typing targets and IME composition. */
export function isSearchShortcut(event: KeyboardEvent): boolean {
  if (event.defaultPrevented || event.isComposing || event.keyCode === 229 || isTypingTarget(event.target)) {
    return false;
  }
  if (event.key === "/" && !event.metaKey && !event.ctrlKey && !event.altKey) {
    return true;
  }
  return (event.metaKey || event.ctrlKey) && !event.altKey && !event.shiftKey && event.key.toLowerCase() === "k";
}
