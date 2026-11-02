# Change feed

Bearer-authenticated readers can maintain an index of documents, tasks, task
comments and structured task decision requests. The sync endpoints use the same
read access as the existing document and task APIs.

## Delta

`GET /v1/sync/delta?after=0&limit=100`

`after` is a nonnegative last applied sync ID (default 0). `limit` defaults to
100 and accepts 1–1000. Invalid parameters return 400 `invalid_context`.
The response is `application/x-ndjson`, with ascending sync IDs:

```json
{"sync_id":1,"model":"document","model_id":"42","action":"I","at":"2026-11-02T00:00:00Z"}
{"sync_id":2,"model":"document","model_id":"42","action":"D","at":"2026-11-02T00:00:01Z"}
{"_metadata_":{"last_sync_id":2,"max_sync_id":2,"schema_version":30}}
```

Actions are insert (`I`), update (`U`) and delete (`D`). Timestamps are RFC3339
(with fractional seconds when present). Document, task and comment IDs are their
numeric database IDs encoded as strings. Decision IDs are
`tasks.refs.decision_request.id`. A request revision or resolution emits `U`;
replacement emits `D` for the previous ID and `I` for the replacement. Removing
the current request emits `D`. Tasks have no production hard-delete path: their
append-only event history prevents deletion. Closing a task with `merged` or
`dropped` retains the task and emits `task:U`. The decision bootstrap describes
current structured requests, including resolved requests; historical task event
snapshots, chat questions and the composite decision inbox are separate data.
Comments retain their existing append-only rule, so their current supported
mutation is insert. No new mutation or delete API is introduced.

The final metadata line is required even on an empty page. `last_sync_id` is the
last returned ID, or the supplied `after` on an empty page. `max_sync_id` is the
committed feed watermark in the page's read snapshot. Continue from
`last_sync_id` until it equals `max_sync_id`. IDs may have gaps after rollback.
Never advance a cursor after receiving a partial response without its trailer.
Fetch current model content through `/v1/sync/rows/{model}/{model_id}`.
A missing row means it was deleted after the change. Applying changes must be
idempotent.

An expired cursor returns HTTP 410 and JSON:

```json
{"reset":true,"min_sync_id":123}
```

Discard the index and bootstrap again. The reset boundary records the highest
pruned ID, surviving an entirely empty log. A cursor exactly at that boundary
can continue safely. Sequence gaps alone do not cause a reset. `min_sync_id` is
the oldest retained ID, or the committed watermark plus one when none remain.
Cursors ahead of the server watermark return 400.

## Bootstrap

`GET /v1/sync/bootstrap?model=document&limit=100&cursor=<opaque>`

Models: `document`, `task`, `task_comment`, `decision`. Limits match delta.
The response is JSON:

```json
{"rows":[{"id":"42","updated_at":"2026-11-02T00:00:00Z","key":"notes/example"}],"cursor":"opaque-next-page","max_sync_id":100,"schema_version":30}
```

Only documents have `key`. Comments expose their creation time as `updated_at`.
Omission of `cursor` signals the last page; empty results are `rows: []`.
Pass the returned cursor unchanged to the same model's next page. It binds the
model, initial watermark, schema version and last ID; it is not an authorization
token. Pages use deterministic ID ordering and preserve the first page's
watermark. Malformed and cross-model cursors return 400. Expired bootstrap
watermarks return 410.

Pages read current rows, rather than holding a database snapshot across HTTP
requests. Concurrent inserts, updates and deletes are reconciled by delta:

1. Start each model's bootstrap, saving its first `max_sync_id`.
2. Page that model into a new index generation.
3. Replay delta from the saved watermark, applying changes for that model.
4. Once caught up, publish the generation and continue polling delta.

For one global cursor across all models, use the minimum initial watermark of
all four models, replaying all models from there. Rows already seen during
bootstrap may appear again in delta. Re-fetch and upsert them. A delete removes
the ID whether or not it appeared in bootstrap. Restart on 410.

## Current raw rows

`GET /v1/sync/rows/{model}/{model_id}`

The endpoint uses the same bearer read authentication as delta and bootstrap.
All delta `model_id` values resolve through it while the model exists. Responses
are JSON objects, with no envelope:

- `document`: complete document database row, including `key` and `body`.
- `task`: complete task database row, including `title` and raw `refs`.
- `task_comment`: complete comment database row, including `body` and `author`.
- `decision`: complete current structured request, including `question` and
  `resolution` content when resolved, plus `task_id`, `updated_at`, and `options`.

A deleted, superseded, or unknown ID returns HTTP 404 with `{"deleted":true}`.
Unknown models and malformed numeric IDs return 400 `invalid_context`. Responses
use `Cache-Control: no-store`. A returned row reflects current state, so several
feed events for one ID may resolve to the same latest content. Decision document
references remain references; the inline question and resolution are included.

## Durability and retention

Row triggers write `sync_log` in the same transaction as each source row
mutation, covering all store writers and explicit deletes. Rollback removes both
the mutation and journal rows. Each journal row starts with a NULL `sync_id`,
a top-level `txid` (`pg_current_xact_id()`), and a unique `entry_id` that preserves
insertion order within its transaction. A deferred constraint trigger runs at
commit: it takes advisory transaction lock `824180067`, assigns sequence IDs to
all pending rows of that transaction in `entry_id` order, and updates the
watermark. Remaining trigger firings find no pending rows and return immediately.
The lock is released at commit. Ordinary source writes never acquire this lock;
a long transaction with pending rows allows unrelated writers to commit.
Readers return only assigned IDs. Bootstrap captures the committed watermark
from the same snapshot as its first page; pruning ignores unassigned rows.

Keep the `sync_allocate` constraint deferred; forcing it immediate with
`SET CONSTRAINTS` would acquire the allocation lock before the commit boundary.
The store does not change constraint timing. Existing rows are discovered by
bootstrap when the migration first installs the feed.

The server prunes on startup and hourly, in transactions of at most 10,000
rows. The default retention is 30 days. Configure a positive Go duration using
`HANDOFFKEEP_SYNC_RETENTION` (for example `720h` or `168h`). Invalid values fail
server startup. Pruning removes a prefix and atomically records the reset
boundary; it never removes source records. Slow or failed pruning retains more
history. The worker participates in normal server shutdown.

## CLI

`handoffkeep sync delta --after 100 --limit 1000`

Uses the usual URL/token configuration and optional `--url` and `--token` flags.
It validates the NDJSON page and prints one JSON object containing `changes`
and `_metadata_`. It fetches one page; it does not persist a cursor. HTTP 410
returns an error requiring bootstrap.
