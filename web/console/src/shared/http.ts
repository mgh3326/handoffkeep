// The one fetch layer for console reads. A non-2xx answer rejects with
// HttpError so callers branch on the status (404 → "없음", 403 → no
// permission) without parsing a message; a malformed payload is the caller's
// shape check to reject, never rendered as an empty result.

/** HTTP failure with the status preserved. */
export class HttpError extends Error {
  readonly status: number;
  constructor(status: number) {
    super(`request failed: ${status}`);
    this.name = "HttpError";
    this.status = status;
  }
}

/** GETs path and parses the JSON body; rejects with HttpError on non-2xx. */
export async function getJSON<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await (init === undefined ? fetch(path) : fetch(path, init));
  if (!response.ok) {
    throw new HttpError(response.status);
  }
  return (await response.json()) as T;
}
