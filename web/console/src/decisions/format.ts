// Display helpers for the decision list: the one-line question summary, the
// relative age and the remaining time to a due date. They are kept next to the
// page that uses them; "now" is always the server's generated_at, so two rows
// never disagree about the current time.

const MAX_SUMMARY = 90;

// The tsconfig lib is ES2020, which predates Intl.Segmenter; declare the part
// used here. Without it (an old browser) code points are the fallback.
type SegmenterCtor = new (
  locale: string,
  options: { granularity: "grapheme" },
) => { segment(text: string): Iterable<{ segment: string }> };

function graphemes(text: string): string[] {
  const Segmenter = (Intl as unknown as { Segmenter?: SegmenterCtor }).Segmenter;
  if (!Segmenter) return Array.from(text);
  return Array.from(new Segmenter("ko", { granularity: "grapheme" }).segment(text), (s) => s.segment);
}

function stripMarkdown(line: string): string {
  return line
    .replace(/!?\[([^\]]*)\]\([^)]*\)/g, "$1")
    .replace(/^\s{0,3}(#{1,6}\s+|>\s*|[-*+]\s+|\d+[.)]\s+)/, "")
    .replace(/[`*_~]+/g, "")
    .replace(/\s+/g, " ")
    .trim();
}

/** The first thing worth reading in a question: markdown marks removed, then
 * cut at the earliest of the first sentence end (`. ? !`, so "다." and "요?"
 * too), the first colon, or the first line end, and capped at 90 characters
 * with an ellipsis. The full text lives on the decision page. */
export function summarizeQuestion(text: string): string {
  const line = text
    .split(/\r?\n/)
    .map(stripMarkdown)
    .find((l) => l !== "");
  if (!line) return "";
  let end = line.length;
  const sentence = /[.?!](?=\s|$)/.exec(line);
  if (sentence) end = Math.min(end, sentence.index + 1);
  const colon = line.indexOf(":");
  if (colon > 0) end = Math.min(end, colon);
  const cut = line.slice(0, end).trim() || line;
  const parts = graphemes(cut);
  return parts.length > MAX_SUMMARY ? `${parts.slice(0, MAX_SUMMARY).join("").trimEnd()}…` : cut;
}

function parse(iso: string | null | undefined): number | null {
  if (!iso) return null;
  const t = Date.parse(iso);
  return Number.isFinite(t) ? t : null;
}

/** "방금 / 12분 / 3시간 / 2일" between a past time and now; null when either
 * time cannot be read. */
export function formatAge(nowIso: string, iso: string): string | null {
  const now = parse(nowIso);
  const then = parse(iso);
  if (now === null || then === null) return null;
  const minutes = Math.floor((now - then) / 60_000);
  if (minutes < 1) return "방금";
  if (minutes < 60) return `${minutes}분`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}시간`;
  return `${Math.floor(hours / 24)}일`;
}

/** "5시간" left until a due time (rounded up), or null when it is already
 * past or unreadable. */
export function formatRemaining(nowIso: string, dueIso: string): string | null {
  const now = parse(nowIso);
  const due = parse(dueIso);
  if (now === null || due === null || due <= now) return null;
  const minutes = Math.ceil((due - now) / 60_000);
  if (minutes < 60) return `${minutes}분`;
  const hours = Math.ceil(minutes / 60);
  if (hours < 24) return `${hours}시간`;
  return `${Math.ceil(hours / 24)}일`;
}
