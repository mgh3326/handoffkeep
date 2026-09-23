# Benchmark canonical store

The benchmark API is the canonical PostgreSQL store for scores, repetitions,
and grade decisions. It is served below `/v1/bench/` and uses the same bearer
token map as the other `/v1` routes. Every write records the authenticated
client ID; client-supplied `updated_by`, `created_by`, and `decided_by` values
are ignored.

## Endpoints

All requests and responses use `Content-Type: application/json`.

| Method | Path | Result |
|---|---|---|
| GET | `/v1/bench/scores?model_id=&source=&limit=` | `{"scores":[...]}`; exact optional filters, ordered by `source, metric, model_id, effort, harness` |
| PUT | `/v1/bench/scores` | Body `{"scores":[...]}`; 1–1000 rows, response `{"upserted":N}` |
| GET | `/v1/bench/reps?profile=&grade=&effort=&limit=` | `{"reps":[...]}`; ordered by `id DESC` |
| PUT | `/v1/bench/reps` | Body `{"reps":[...]}`; 1–1000 rows, response `{"upserted":N}` |
| GET | `/v1/bench/grades` | `{"grades":[...]}`; ordered by `profile` |
| PUT | `/v1/bench/grades` | Body `{"grades":[...]}`; 1–1000 rows, response `{"upserted":N}` |
| GET | `/v1/bench/catalog?pool=&include_retired=` | `{"catalog":[...]}`; ordered by `pool`, grade (`S+`→`C`), `profile`, effort rung |
| PUT | `/v1/bench/catalog` | **Operator token only.** Body `{"catalog":[...]}`; 1–1000 rows, response `{"upserted":N}` |

GET limits default to 1000 and are capped at 5000. Each PUT is one database
transaction: a validation or storage error rejects the complete batch.

Scores use the unique key `(model_id, effort, harness, source, metric)`:

```json
{
  "model_id": "model",
  "effort": "high",
  "harness": "runner",
  "source": "source",
  "metric": "metric",
  "score": 13.4,
  "rank": 18,
  "captured_at": "2026-07-31T00:00:00Z",
  "time_per_task_min": 13.4,
  "cost_per_task_usd": 3.8,
  "provenance": "operator-approved import",
  "updated_by": "ignored",
  "updated_at": "2026-09-07T04:30:00Z"
}
```

`effort`, `harness`, and `provenance` are stored as `''` when absent, never as
JSON `null`. PostgreSQL treats NULL values as distinct inside a UNIQUE key, so
nullable key fields would allow duplicate logical rows and break idempotent
upsert. Nullable measurements remain JSON `null`. `rank` is computed by the
client; the server stores it and does not compare or re-rank values from
different source/metric pairs.

Repetitions retain the existing client columns unchanged in name and meaning:
`profile`, `model_id`, `task_ref`, `tier`, `role`, `rounds`, `blockers_found`,
`completed`, `input_tokens`, `output_tokens`, `notes`, `recorded_at`, `effort`,
`grade`, and `table_grade`. `id` is a server-assigned `BIGSERIAL`. The client
row identity is carried separately as required `origin_id`, and the upsert key
is `(created_by, origin_id)`. Local rowids are per-machine and can collide, so
including the authenticated client identity keeps two machines' row 7 apart.
`profile` and `recorded_at` are required; the other carried fields are nullable
and are returned as JSON `null` when omitted.

The repetition wire row also includes the server-owned identity fields:

```json
{
  "id": 1,
  "origin_id": 568,
  "profile": "profile",
  "model_id": "model",
  "task_ref": "PR#42",
  "tier": "T2",
  "role": "impl",
  "rounds": 1,
  "blockers_found": 0,
  "completed": 1,
  "input_tokens": 120000,
  "output_tokens": 8000,
  "notes": "note",
  "recorded_at": "2026-09-01T10:00:00Z",
  "effort": "max",
  "grade": "A+",
  "table_grade": "A+",
  "created_by": "bench-client",
  "created_at": "2026-09-07T04:30:00Z"
}
```

Grades use the closed ladder `S+`, `S`, `A+`, `A`, `B`, `C`. Every write must
include a non-blank `deviation_ref`; a missing, empty, or whitespace-only
reference rejects the whole batch. `decided_by` is server-set, and `decided_at`
is set to the server time when omitted. `codex-sol` and `kiro-sol` accept only
`S+`; any other grade is rejected with `400 bench_catalog_sol_grade`, matching
the catalog invariant. Client text in `provenance`, `notes`,
`task_ref`, `boundary_version`, and `deviation_ref` passes through the secret
guard before storage.

Grade rows have this shape:

```json
{
  "profile": "profile",
  "grade": "S",
  "boundary_version": "2026-09-07",
  "deviation_ref": "deviation-ref",
  "decided_at": "2026-09-07T04:30:00Z",
  "decided_by": "bench-client"
}
```

## Catalog

`bench_catalog` is the canonical (profile, effort)-keyed grade catalog; the
legacy `bench_grades` table and its `/v1/bench/grades` routes are a
compatibility projection over the catalog's profile-default (`effort=""`)
rows. Both PUT paths mirror each other: a grades write upserts the catalog's
default row, and a catalog default-row write upserts `bench_grades` (or
deletes the legacy row when the catalog row is retired), so pre-catalog
clients keep working during version skew.

Catalog rows have this shape:

```json
{
  "profile": "codex-sol",
  "effort": "max",
  "model_id": "gpt-5.6-sol",
  "pool": "codex",
  "grade": "S+",
  "score": 67.0,
  "gate": "default",
  "gate_reason": null,
  "benchmark_source": "AA-agent",
  "benchmark_annotation": null,
  "boundary_version": "2026-09-23",
  "deviation_ref": "decision/2026-09-23/dispatch-...",
  "decided_at": "2026-09-23T00:00:00Z",
  "decided_by": "ops-review",
  "retired_at": null
}
```

- `profile` and `effort` form the row key. `effort=""` is the profile-default
  row; known rungs are `low < medium < high < xhigh < max`. Unknown effort
  strings are allowed but exempt from the monotonicity rule below.
- `grade` uses the same closed ladder as grades. `gate` is one of `default`,
  `escalation`, `consult_only`.
- `score`, `gate_reason`, `benchmark_source`, `benchmark_annotation`, and
  `retired_at` are nullable. `score` must be a finite 0–100.
- Unlike the other PUTs, `decided_by` is **required caller-supplied
  provenance** (who or which decision produced the row) — a missing or blank
  value is a 400. The server does not overwrite it because the caller is
  always the operator.
- Validation: `deviation_ref` non-blank (as with grades); Sol profiles
  (`codex-sol`, `kiro-sol` — the scopefuel `_SOL_PROFILES` set) accept only
  `S+`; and within one profile the merged post-write state must be monotonic —
  a higher known effort rung may not carry a worse grade. Violations roll back
  the whole batch.
- `GET /v1/bench/catalog` returns non-retired rows including `consult_only`.
  `?include_retired=1` also returns retired rows. `?pool=<pool>` returns the
  subscription ladder: that pool's non-retired, non-`consult_only` rows,
  ordered grade-descending, then `profile`, then effort rung.
- `PUT /v1/bench/catalog` requires the operator credential — the bearer token
  whose auth-file client id is `operator` (`HANDOFFKEEP_TOKEN_operator`), the
  same hardened-boundary convention as panewire #68's `HUB_TOKEN_operator`.
  Other tokens get 403 `{"error":"operator_required"}`; deployments without
  the entry fail closed.

The `catalog` array in the PUT body is the JSON input format that
`scopefuel bench push-catalog` consumes: one object per row with the fields
above (`retired_at` may be an RFC 3339 timestamp to retire, otherwise null or
omitted).

## Errors

| Status | Body | Meaning |
|---|---|---|
| 401 | `{"error":"unauthorized"}` | Missing or unknown bearer token |
| 403 | `{"error":"operator_required"}` | Catalog write without the operator token |
| 400 | `{"error":"invalid_context"}` | Malformed body, invalid field, empty batch, or batch over 1000 |
| 400 | `{"error":"deviation_ref_required"}` | Grade write has no usable deviation reference |
| 400 | `{"error":"decided_by_required"}` | Catalog write has no usable `decided_by` |
| 400 | `{"error":"bench_catalog_not_monotonic"}` | Catalog write would put a worse grade on a higher effort rung |
| 400 | `{"error":"bench_catalog_sol_grade"}` | Sol profile graded other than `S+` |
| 400 | `{"error":"secret_like_content","pattern":"<name>"}` | Secret guard rejected client text |
| 404 | `{"error":"not_found"}` | Unknown path |

Schema version 9 is intentional. Version 8 is reserved for another additive
change, so this migration skips that number; schema-version rows are markers,
and the harmless gap prevents either change from accidentally satisfying the
other migration's gate. Schema version 12 adds `bench_catalog` and backfills
it from `bench_grades` with `ON CONFLICT DO NOTHING`; like v9 it is gated on
its schema-version marker so a replay never disturbs catalog-only columns.
