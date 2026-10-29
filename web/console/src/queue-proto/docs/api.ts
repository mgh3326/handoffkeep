// Read-only document-browse client. Both routes are operator-only on the
// server (service principals get 403); a refusal surfaces as HttpError like
// every other console read.

import { HttpError } from "../../board/api";
import type { DocsPrefixesResponse, DocsResponse } from "./types";

async function getJSON<T>(path: string): Promise<T> {
  const response = await fetch(path);
  if (!response.ok) {
    throw new HttpError(response.status);
  }
  return (await response.json()) as T;
}

/** One page of document summaries, newest key first. An empty prefix lists
 * every document; before is the cursor from the previous page. */
export async function fetchDocs(prefix: string, before?: string, limit = 100): Promise<DocsResponse> {
  const params = new URLSearchParams();
  if (prefix !== "") {
    params.set("prefix", prefix);
  }
  if (before) {
    params.set("before", before);
  }
  params.set("limit", String(limit));
  const body = await getJSON<DocsResponse>(`/ui/api/docs?${params.toString()}`);
  // A payload without a documents array is a failure, never an empty list.
  if (typeof body !== "object" || body === null || !Array.isArray(body.documents)) {
    throw new Error("docs response has no documents array");
  }
  return body;
}

/** Top-level key prefixes and their whole-key-space counts. */
export async function fetchDocPrefixes(): Promise<DocsPrefixesResponse> {
  const body = await getJSON<DocsPrefixesResponse>("/ui/api/docs/prefixes");
  if (typeof body !== "object" || body === null || !Array.isArray(body.prefixes)) {
    throw new Error("prefixes response has no prefixes array");
  }
  return body;
}
