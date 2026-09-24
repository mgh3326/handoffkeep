// Task body pointers. body_doc is "key" or the transitional "key#section"
// (hk:doc decision/2026-09-21/task536-body-storage-approved). This module is
// plain string handling — it never imports the markdown renderer, so the
// queue entry can use it without pulling the renderer into the list bundle.

/** The document key alphabet the console can fetch and link — the same shape
 * the server enforces (internal/ui docKeyRE, store.ValidBodyDoc). */
const DOC_KEY_RE = /^[A-Za-z0-9._\-/]{1,512}$/;

export function validDocKey(key: string): boolean {
  return DOC_KEY_RE.test(key) && !key.startsWith("/") && !key.includes("..");
}

export type BodyDocRef = { key: string; section: string | null };

/** Splits body_doc into its document key and scroll section. null when the
 * value is not key-shaped — the caller shows "형식 미지원", never a fetch. */
export function parseBodyDoc(value: string): BodyDocRef | null {
  const hash = value.indexOf("#");
  const key = hash === -1 ? value : value.slice(0, hash);
  const section = hash === -1 ? null : value.slice(hash + 1);
  if (!validDocKey(key)) {
    return null;
  }
  if (section !== null && (section === "" || section.includes("#") || /[\s\p{Cc}]/u.test(section))) {
    return null;
  }
  return { key, section };
}

/** Same-origin link to the escaped raw document page. Only key-shaped values
 * reach here, and every allowed character is URL-safe. */
export function docPageHref(key: string): string {
  return `/ui/doc/${key}`;
}

/** Document keys written into a title as "hk:doc <key>" (the transitional
 * habit before body_doc existed). These are shown as links with their source
 * named — never rendered inline and never written back as body_doc. */
export function titleDocKeys(title: string): string[] {
  const out: string[] = [];
  for (const m of title.matchAll(HK_DOC_RE)) {
    // A trailing sentence period is punctuation, not part of the key.
    const key = m[1].replace(/\.+$/, "");
    if (validDocKey(key) && !out.includes(key)) {
      out.push(key);
    }
  }
  return out;
}

const HK_DOC_RE = /hk:doc\s+`?([A-Za-z0-9._\-/]+)`?/g;

/** The explicit body marker: "본문" (optionally "본문 문서", an optional
 * Korean particle, optional punctuation) directly ahead of hk:doc <key>.
 * A bare hk:doc citation elsewhere in the title is not a body candidate —
 * and an hk:doc followed by other words before the key never matches. */
const BODY_MARKER_RE = /본문(?:\s*문서)?\s*[은는이가을를]?\s*[:：=·\-—~]?\s*hk:doc\s+`?([A-Za-z0-9._\-/]+)`?/g;

/** An all-numeric citation ("hk:doc 2299") is a document ID, not a key —
 * IDs and keys are never interchanged, so it is neither fetched nor linked. */
const NUMERIC_ID_RE = /^\d+$/;

export type TitleDocScan = {
  /** Unique keys marked "본문 hk:doc <key>", in order of appearance. */
  body: string[];
  /** Unique plain hk:doc <key> citations that are not body candidates —
   * shown as related-document links only. */
  related: string[];
  /** Unique numeric hk:doc citations — document IDs, never key-linked. */
  ids: string[];
};

/** Classifies every hk:doc reference in a title: explicit "본문" body
 * candidates, plain related citations, and numeric document IDs. A body
 * candidate's span is consumed, so its key never reappears as a related
 * link. Nothing here fetches or writes. */
export function scanTitleDocs(title: string): TitleDocScan {
  const body: string[] = [];
  const spans: [number, number][] = [];
  for (const m of title.matchAll(BODY_MARKER_RE)) {
    const key = m[1].replace(/\.+$/, "");
    if (!validDocKey(key) || NUMERIC_ID_RE.test(key)) {
      // An unusable "본문" key is not a candidate; the inner hk:doc token
      // still reaches the citation pass below (numeric → ids, invalid →
      // dropped, as with any other unusable reference).
      continue;
    }
    spans.push([m.index, m.index + m[0].length]);
    if (!body.includes(key)) {
      body.push(key);
    }
  }
  const related: string[] = [];
  const ids: string[] = [];
  for (const m of title.matchAll(HK_DOC_RE)) {
    if (spans.some(([start, end]) => m.index >= start && m.index < end)) {
      continue;
    }
    const key = m[1].replace(/\.+$/, "");
    if (NUMERIC_ID_RE.test(key)) {
      if (!ids.includes(key)) {
        ids.push(key);
      }
    } else if (validDocKey(key) && !related.includes(key) && !body.includes(key)) {
      related.push(key);
    }
  }
  return { body, related, ids };
}
