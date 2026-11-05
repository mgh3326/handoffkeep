// Reads one hk document through the console's read-only JSON route. The
// payload is checked field by field: a malformed answer throws, it is never
// rendered as an empty document.

import { getJSON } from "../http";

export type DocPayload = {
  key: string;
  kind: string;
  sha256: string;
  updated_at: string;
  format: "markdown" | "unsupported";
  reason?: string;
  body: string;
};

export class MalformedDocError extends Error {
  constructor(field: string) {
    super(`malformed document payload: ${field}`);
    this.name = "MalformedDocError";
  }
}

export function parseDoc(value: unknown): DocPayload {
  if (typeof value !== "object" || value === null) {
    throw new MalformedDocError("not an object");
  }
  const v = value as Record<string, unknown>;
  for (const field of ["key", "kind", "sha256", "updated_at", "body"]) {
    if (typeof v[field] !== "string") {
      throw new MalformedDocError(field);
    }
  }
  if (v.format !== "markdown" && v.format !== "unsupported") {
    throw new MalformedDocError("format");
  }
  if (v.reason !== undefined && typeof v.reason !== "string") {
    throw new MalformedDocError("reason");
  }
  return v as DocPayload;
}

/** Rejects with HttpError(404) for a missing document. */
export async function fetchDoc(key: string): Promise<DocPayload> {
  return parseDoc(await getJSON<unknown>(`/ui/api/board/doc?key=${encodeURIComponent(key)}`));
}
