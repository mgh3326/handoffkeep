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
| PUT | `/v1/bench/reps` | Body `{"reps":[...]}`; 1–1000 rows, response `{"upserted":N,"ids":[...]}` — `ids` carries the server id of every accepted row aligned to the input order |
| PATCH | `/v1/bench/reps/{id}` | Set-once outcome patch of the rep with server id `{id}` (below); response is the row |
| GET | `/v1/bench/grades` | `{"grades":[...]}`; ordered by `profile` |
| PUT | `/v1/bench/grades` | Body `{"grades":[...]}`; 1–1000 rows, response `{"upserted":N}` |
| GET | `/v1/bench/catalog?pool=&include_retired=` | `{"catalog":[...]}`; ordered by `pool`, grade (`S+`→`C`), `profile`, effort rung |
| PUT | `/v1/bench/catalog` | **Operator, operator-desk or panewire-design token only.** Body `{"catalog":[...]}`; 1–1000 rows, response `{"upserted":N}` |

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
is `(created_by, origin_id)`. `profile` and `recorded_at` are required; the
other carried fields are nullable and are returned as JSON `null` when
omitted.

Rep content is **insert-only**. Several hosts are expected to authenticate
under the same token, and local `origin_id` values are per-machine counters
that collide routinely, so `(created_by, origin_id)` alone cannot tell "the
same rep sent twice" apart from "a different host's rep that drew the same
number". A PUT row whose key is already taken is therefore accepted only when
every carried field is identical to the stored row — an idempotent resend
that returns the existing server id. Any differing field (including a set
field becoming `null` or vice versa, and `recorded_at` compared as an
instant) rejects the **entire batch** with `409 bench_rep_conflict`; the
response body names every colliding input row as
`{"conflicts":[{"index":…,"origin_id":…,"conflict_server_id":…},…]}`, where
`index` is the row's position in the request batch and `conflict_server_id`
is the id of the stored row that holds the key. Every rejection is logged
server-side with `created_by`, `index`, `origin_id`, and
`conflict_server_id`. No existing row is ever updated through this endpoint:
`created_at` always marks the first accepted write of a key, and the
content-vs-resend check is evaluated inside the `ON CONFLICT` row lock, so
two transactions racing on one key cannot both write — identical payloads
both keep the same id, different payloads let exactly one win.

The same rule applies inside one batch: a repeated `(created_by, origin_id)`
key is an idempotent resend only when the repeated rows are identical
(each returns the same server id), and differing repeats reject the whole
batch with `400 invalid_context` — the store error names the offending row
positions. A deadlock or serialization failure (SQLSTATE 40P01/40001)
inside the batch transaction aborts it instead: the loser of a lock cycle
gets `503 bench_reps_retryable`, writes nothing, and may retry the
identical batch.

Every attempted insert still consumes one `BIGSERIAL` candidate — including
attempts rejected by the conflict check — so gaps in `id` are expected,
harmless, and are the visible trace of refused collisions, not lost rows.
Ids never move backwards: a returned `id` permanently names the rep that
produced it and can never be reassigned to different content. Reps already
lost to the pre-fix overwrite are not tombstoned; they are recovered after
deploy by a one-time manifest re-add from the owning hosts (the server does
not scan prod for them).

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

Grades accept the ladder `S+`, `S`, `A+`, `A`, `B`, `C`, plus `unrated` for
rungs without a measured or operator-set grade. Every write must
include a non-blank `deviation_ref`; a missing, empty, or whitespace-only
reference rejects the whole batch. `decided_by` is server-set, and `decided_at`
is set to the server time when omitted. `codex-sol` and `kiro-sol` accept
any grade the other profiles accept. Client text in `provenance`, `notes`,
`task_ref`, `boundary_version`, and `deviation_ref` passes through the secret
guard before storage.

A grade write cannot change a scored Sol default catalog row to `unrated`.
It rejects the entire batch with `400 bench_catalog_scored_sol_unrated`, preserving the
score and both grade stores. To make that transition, an operator must explicitly
remove the score through the catalog write path first.

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
- `grade` uses the same values as grades. `gate` is one of `default`,
  `escalation`, `consult_only`.
- `score`, `gate_reason`, `benchmark_source`, `benchmark_annotation`, and
  `retired_at` are nullable. `score` must be a finite 0–100.
- Unlike the other PUTs, `decided_by` is **required caller-supplied
  provenance** (who or which decision produced the row) — a missing or blank
  value is a 400. The server does not overwrite it because the caller is
  always the operator.
- Validation requires a non-blank `deviation_ref`. Measured Sol placements
  (`codex-sol`, `kiro-sol`) carry any grade, like the other profiles. A grade
  write may not turn a scored Sol default row `unrated`. Within one profile, a higher known
  effort rung may not carry a worse grade. Unrated rows do not participate in
  this comparison. Violations roll back the whole batch.
- `GET /v1/bench/catalog` returns non-retired rows including `consult_only`.
  `?include_retired=1` also returns retired rows. `?pool=<pool>` returns the
  subscription ladder: that pool's non-retired, non-`consult_only` rows,
  ordered grade-descending, then `profile`, then effort rung.
- `PUT /v1/bench/catalog` requires the operator credential — the bearer token
  whose auth-file client id is `operator` (`HANDOFFKEEP_TOKEN_operator`), the
  same hardened-boundary convention as panewire #68's `HUB_TOKEN_operator`,
  or the operator-desk session's client id `operator-desk`, which records the
  operator's catalog decisions on the operator's behalf, or the panewire design
  session's client id `panewire-design`, which owns the bench catalog (decision
  `hk:doc decision/2026-11-08/panewire-design-catalog-writer`). The area-exceptions
  PUT follows the same rule. Other tokens get 403
  `{"error":"operator_required"}`; deployments without the entries fail
  closed.

The `catalog` array in the PUT body is the JSON input format that
`pacelever bench push-catalog` consumes: one object per row with the fields
above (`retired_at` may be an RFC 3339 timestamp to retire, otherwise null or
omitted).

### Rollback compatibility

Migration v28 widens the grade constraints without changing existing rows.
Before the first `unrated` write, the previous binary can still read that data.
After publishing unrated grades, keep the v28-capable binary installed until an
operator restores reviewed pre-publication catalog and grade data with no
`unrated` rows and verifies the previous binary's Sol and monotonicity rules.
Restore data while the new binary is still running, then downgrade the binary.
The widened constraints can remain in place.

Do not mechanically convert `unrated` to C. A ladder of B, unrated, A is valid
here but B, C, A violates the previous monotonicity rule. Such rows require an
operator-reviewed restoration or removal. Schema migration has no automatic
data rollback because it cannot reconstruct the previous grade decisions.

## Errors

| Status | Body | Meaning |
|---|---|---|
| 400 | `{"error":"invalid_context"}` | Malformed body, invalid field, empty batch, batch over 1000, or a `(created_by, origin_id)` key repeated inside one rep batch with different content |
| 400 | `{"error":"deviation_ref_required"}` | Grade write has no usable deviation reference |
| 400 | `{"error":"decided_by_required"}` | Catalog write has no usable `decided_by` |
| 400 | `{"error":"bench_catalog_not_monotonic"}` | Catalog write would put a worse grade on a higher effort rung |
| 400 | `{"error":"bench_catalog_scored_sol_unrated"}` | Grade write would turn a scored Sol default row `unrated` |
| 400 | `{"error":"secret_like_content","pattern":"<name>"}` | Secret guard rejected client text |
| 401 | `{"error":"unauthorized"}` | Missing or unknown bearer token |
| 403 | `{"error":"operator_required"}` | Catalog write without the operator, operator-desk or panewire-design token |
| 404 | `{"error":"not_found"}` | Unknown path |
| 409 | `{"error":"bench_rep_conflict","conflicts":[{"index":I,"origin_id":N,"conflict_server_id":M}]}` | Rep write shares a `(created_by, origin_id)` key with a stored row whose content differs; the whole batch is rejected |
| 503 | `{"error":"bench_reps_retryable"}` | Rep batch aborted by a deadlock or serialization failure; nothing was written and the identical batch may be retried |

Schema version 9 is intentional. Version 8 is reserved for another additive
change, so this migration skips that number; schema-version rows are markers,
and the harmless gap prevents either change from accidentally satisfying the
other migration's gate. Schema version 12 adds `bench_catalog` and backfills
it from `bench_grades` with `ON CONFLICT DO NOTHING`; like v9 it is gated on
its schema-version marker so a replay never disturbs catalog-only columns.

## Outcome columns and the set-once patch

Schema 40 (hk 2886) adds nullable experiment and outcome columns to every rep.
NULL means unknown, never 0 or false:

| Columns | Type |
| --- | --- |
| `experiment_id`, `reviewer_profile`, `reviewer_effort` | text |
| `arm` | `baseline` or `arm` |
| `first_ci_green`, `merged`, `reverted_within_7d` | boolean |
| `critical_findings`, `high_findings`, `spawn_to_ready_s`, `spawn_to_merge_s` | integer, at least 0 |
| `reasoning_tokens`, `cache_read_tokens`, `cache_write_tokens` | integer, at least 0 |

- **On insert:** `PUT /v1/bench/reps` accepts these columns. They count as rep content, so a resend that changes one of them is `bench_rep_conflict`.
- **Late outcomes:** a merge, a revert within 7 days, or review findings arrive through `PATCH /v1/bench/reps/{id}`. Any node token may patch. The body is any subset of the columns above, plus `input_tokens` and `output_tokens`.
- **How a patch applies:**
  - It fills only columns that are still NULL. An identical value is a no-op.
  - A different value on a column that already holds one returns 409 `{"error":"bench_rep_outcome_conflict","field":"<column>"}` and writes nothing from that patch.
  - An empty patch or an unknown key returns 400.
  - An id with no rep returns 404 `not_found`.
- `experiment_id` has no foreign key until the experiment registry (hk 2885) exists.

