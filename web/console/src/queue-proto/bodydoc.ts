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
  for (const m of title.matchAll(/hk:doc\s+`?([A-Za-z0-9._\-/]+)/g)) {
    // A trailing sentence period is punctuation, not part of the key.
    const key = m[1].replace(/\.+$/, "");
    if (validDocKey(key) && !out.includes(key)) {
      out.push(key);
    }
  }
  return out;
}
