// The console's one mirror of the server's inline media allowlist
// (internal/ui/files.go: inlineMediaTypes). /ui/files/<sha>?inline=1 answers
// with an inline disposition for exactly these types; every other type keeps
// the download disposition, so a page must link it instead of drawing a
// picture the browser cannot decode. The task page and the decision page both
// read that rule here.

/** Media above this size stays a link and its size: 25 MiB. */
export const MAX_INLINE_BYTES = 25 * 1024 * 1024;

const IMAGE_MIMES = new Set(["image/png", "image/jpeg", "image/webp", "image/gif"]);
const VIDEO_MIMES = new Set(["video/mp4", "video/webm"]);

export type MediaKind = "image" | "video";

/** The kind of media the server serves inline, or null when it serves the
 * file only as a download. An unknown type — including the empty one a
 * content address with no attachments row leaves — is null. */
export function mediaKind(mime: string | undefined): MediaKind | null {
  const base = (mime ?? "").split(";")[0].trim().toLowerCase();
  if (IMAGE_MIMES.has(base)) return "image";
  if (VIDEO_MIMES.has(base)) return "video";
  return null;
}

/** The inline URL the server answers with `Content-Disposition: inline` for
 * exactly the types above. */
export function inlineSrc(sha256: string): string {
  return `/ui/files/${sha256}?inline=1`;
}
