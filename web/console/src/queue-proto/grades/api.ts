import { HttpError } from "../../board/api";
import type { CatalogResponse } from "./types";

export type CatalogQuery = { pool?: string; includeRetired?: boolean };

/** Read-only fetch of the grade catalog through the console BFF. The query
 * contract mirrors GET /v1/bench/catalog: ?pool= selects one subscription
 * ladder (the server drops consult_only rows in that view) and
 * include_retired=1 adds retired rows. No write path exists here — catalog
 * writes stay on the operator-token PUT /v1/bench/catalog. */
export async function fetchCatalog(query: CatalogQuery = {}): Promise<CatalogResponse> {
  const params = new URLSearchParams();
  if (query.pool) {
    params.set("pool", query.pool);
  }
  if (query.includeRetired) {
    params.set("include_retired", "1");
  }
  const suffix = params.toString();
  const response = await fetch(`/ui/api/bench/catalog${suffix === "" ? "" : `?${suffix}`}`);
  if (!response.ok) {
    throw new HttpError(response.status);
  }
  const body = (await response.json()) as CatalogResponse;
  // A malformed payload must fail loudly — rendering a partial shape as a
  // complete catalog would present missing rows as "no assignments".
  if (typeof body !== "object" || body === null || !Array.isArray(body.catalog)) {
    throw new Error("catalog response has no catalog array");
  }
  return body;
}
