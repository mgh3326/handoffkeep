// Wire types for GET /ui/api/bench/catalog. BenchCatalogEntry mirrors
// store.BenchCatalogEntry field-for-field — the same shape the deployed
// GET /v1/bench/catalog (hk f0681e5, #592) emits. The fixture and this
// interface are pinned to the Go type by internal/ui/catalog_test.go.
export type BenchCatalogEntry = {
  profile: string;
  /** "" is the profile-default row — rendered as "기본", never blank. */
  effort: string;
  model_id: string;
  pool: string;
  grade: string;
  score: number | null;
  gate: string;
  gate_reason: string | null;
  benchmark_source: string | null;
  benchmark_annotation: string | null;
  boundary_version: string;
  deviation_ref: string;
  decided_at: string;
  decided_by: string;
  retired_at: string | null;
};

/** The BFF envelope adds generated_at (server read time) — the /v1 body has
 * no clock of its own and the screen must say how fresh the table is. */
export type CatalogResponse = {
  generated_at: string;
  catalog: BenchCatalogEntry[];
};
