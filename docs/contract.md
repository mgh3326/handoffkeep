# Checkpoint contract

Write a checkpoint every 30 minutes, before a risky operation, and before a
session ends. A resumed session starts with `ctx recent` for its source
session. Checkpoints are short, factual handoff records: state, decision,
next action, and safe references to normal Git or job artifacts.

The service rejects credential-shaped text before it is persisted. Never place
tokens in checkpoint, memory, or document bodies. Each write records the
authenticated client ID; callers cannot choose that value.

Use repeatable `--ref key=value` flags to attach safe references. They are
stored and returned as JSON arrays (for example `"prs":["26"]` and
`"jobs":["hk-v0"]`) in checkpoint and checkpoint-search output. Memory push
and pull preserve the complete Markdown file byte-for-byte for new records,
including frontmatter, body, and final newline.

Limits are 64 KiB per checkpoint/memory field, the newest 500 checkpoints per
session, and 2,000 memories per agent. Documents are 512 KiB, use SHA-256
idempotency, and are not a substitute for source control.

# Task snapshot export

`GET /v1/tasks/export` returns one consistent snapshot of the task queue as a
single bounded JSON document, and `handoffkeep tasks export` prints that
document to stdout byte-for-byte. The route reuses the existing `/v1/tasks`
bearer authentication and the same lane, state, and parent_lane filter
validation as the task list reads; it creates no new credential class and no
read-only credential tier. This change ships code only — it does not deploy
anything.

All snapshot data is read inside exactly one PostgreSQL transaction opened as
`REPEATABLE READ READ ONLY`. `snapshot_id` is `pg_current_snapshot()` rendered
as text inside that transaction, `db_time` is `now()` read inside the
transaction and serialized in UTC, `watermark.task_event_max_id` and
`watermark.relay_event_max_id` are `MAX(id)` over `task_events` and
`relay_events` (an empty table yields `0`), and `counts.total`,
`counts.by_state`, and `counts.by_lane` are authoritative counts for the
filtered scope inside the same snapshot. `by_state` carries an entry for every
canonical state, including `merged` and `dropped`; `by_lane` carries an entry
for every lane present in the filtered set. `tasks` are the filtered rows in
ascending numeric ID order with the same projected fields as `/v1/tasks`
rows; task events are never embedded — the two watermarks bound the event
streams at the snapshot but do not prove event contents.

`source.vcs_revision` is the serving binary's embedded VCS revision, or the
explicit string `"unknown"` when the binary has no VCS stamp; a revision is
never invented. `scope` echoes the normalized `{lane, state, parent_lane,
limit}` actually applied; empty strings mark unset filters. `limit` defaults
to 1000 with a hard maximum of 10,000. `truncated` is exactly
`counts.total > limit`; `complete` is its inverse, so a complete export always
returns every counted row (`rows_returned == counts.total`) and
`counts.total == limit` is still complete.

Each export is one independent snapshot and is not resumable: two sequential
exports may honestly differ when commits land between them, but within one
document every field describes the same instant. Rows are never silently
clipped — truncation is always announced.

Digest byte definition, reproducible in any language. `ids_sha256` is the
SHA-256 over, for each returned row in ascending ID order, the row's decimal
ASCII ID followed by `\n`. `rows_sha256` is the SHA-256 over, for each
returned row in ascending ID order, the deterministic JSON encoding of the
projected task row followed by `\n`. The deterministic encoding is byte-
identical to the row's element inside this document's `tasks` array — Go
`encoding/json` compact form, fixed struct field order, RFC3339Nano UTC
timestamps, `refs` as nested JSON — so a verifier may either re-encode the
projected fields or hash the raw element bytes as received. An empty result
set yields the SHA-256 of empty input
(`e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`) for both
digests.

Error contract: an absent or invalid bearer token returns the unchanged 401
`unauthorized`; an invalid `lane`, `state`, `parent_lane`, or `limit`
(including non-numeric, below 1, or above 10,000) returns 400
`invalid_export_query`; a transaction, snapshot, or encode failure returns
500 `export_unavailable`. Failure bodies carry only the error code — never
task content, scope data, or credentials — and never claim `complete`. The
export response is marked `Cache-Control: no-store`.
