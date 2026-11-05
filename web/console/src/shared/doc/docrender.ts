// Renderer-side helpers for DocMarkdown: the URL policy and "#section"
// lookup. Only DocMarkdown (the lazy chunk) imports this module, so rollup
// keeps it inside that chunk. Anything the chunk imported from the queue
// entry's own graph would make the chunk import board.js by its bare URL —
// a second instance of the entry next to the page's stamped board.js?v=…

/** URL policy for links and image references inside a rendered document:
 * in-document "#fragment", same-origin paths, or absolute https. Everything
 * else — javascript:, data:, file:, http:, protocol-relative "//host",
 * backslash tricks, control characters — is dropped (undefined). */
export function safeDocUrl(raw: string): string | undefined {
  const value = raw.trim();
  if (value === "" || /[\u0000-\u001f\u007f]/.test(value)) {
    return undefined;
  }
  if (value.startsWith("#")) {
    return value;
  }
  if (/^[\\/][\\/]/.test(value)) {
    return undefined;
  }
  const scheme = /^([A-Za-z][A-Za-z0-9+.-]*):/.exec(value);
  if (scheme) {
    if (scheme[1].toLowerCase() !== "https") {
      return undefined;
    }
    try {
      const url = new URL(value);
      return url.protocol === "https:" ? url.href : undefined;
    } catch {
      return undefined;
    }
  }
  try {
    const url = new URL(value, window.location.origin);
    if (url.origin !== window.location.origin) {
      return undefined;
    }
    return url.pathname + url.search + url.hash;
  } catch {
    return undefined;
  }
}

function normalizeHeading(value: string): string {
  return value
    .normalize("NFC")
    .toLowerCase()
    .replace(/[^\p{L}\p{N}]+/gu, "");
}

/** Finds the heading a "#section" points at: an exact normalized match wins,
 * then the first heading that starts with the section text. */
export function findSectionHeading(root: ParentNode, section: string): HTMLElement | null {
  let wanted = section;
  try {
    wanted = decodeURIComponent(section);
  } catch {
    // keep the raw section text
  }
  const needle = normalizeHeading(wanted);
  if (needle === "") {
    return null;
  }
  const headings = [...root.querySelectorAll<HTMLElement>("h1,h2,h3,h4,h5,h6")];
  const normalized = headings.map((h) => normalizeHeading(h.textContent ?? ""));
  const exact = normalized.findIndex((text) => text === needle);
  if (exact !== -1) {
    return headings[exact];
  }
  const prefix = normalized.findIndex((text) => text.startsWith(needle));
  return prefix === -1 ? null : headings[prefix];
}
