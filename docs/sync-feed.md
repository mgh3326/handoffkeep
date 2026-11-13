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
`tasks.refs.decision_request.id`, except `record:<id>`, a write of the
`decision_requests` row `<id>` (the YouTrack decision projection reads it);
a reader of requests ignores it. A request revision or resolution emits `U`;
replacement emits `D` for the previous ID and `I` for the replacement. Removing
the current request emits `D`. Tasks have no production hard-delete path: their
append-only event history prevents deletion. Closing a task with `merged` or
`dropped` retains the task and emits `task:U`. The decision projection describes
current structured requests, including resolved requests; historical task event
snapshots, chat questions and the composite decision inbox are separate data.
Comments retain their existing append-only rule, so their current supported
mutation is insert. No new mutation or delete API is introduced.

The final metadata line is required even on an empty page. `last_sync_id` is the
last returned ID, or the supplied `after` on an empty page. `max_sync_id` is the
committed feed watermark in the page's read snapshot. Continue from
`last_sync_id` until it equals `max_sync_id`. IDs may have gaps after rollback.
Never advance a cursor after receiving a partial response without its trailer.
Delta carries IDs, not bodies: fetch current content from the owning API.
Applying changes must be idempotent.

An expired cursor returns HTTP 410 and JSON:

```json
{"reset":true,"min_sync_id":123}
```

Discard the index and start again from the reset boundary. The reset boundary records the highest
pruned ID, surviving an entirely empty log. A cursor exactly at that boundary
can continue safely. Sequence gaps alone do not cause a reset. `min_sync_id` is
the oldest retained ID, or the committed watermark plus one when none remain.
Cursors ahead of the server watermark return 400.

## Starting from nothing

There is no bootstrap or row endpoint. A new reader starts at `after=0` and
follows delta to `max_sync_id`; a 410 reset restarts it from the reset
boundary. Delta rows carry the model and ID, and a reader that needs a current
body fetches it from the API that owns the model.

## Durability and retention

Row triggers write `sync_log` in the same transaction as each source row
mutation, covering all store writers and explicit deletes. Rollback removes both
the mutation and journal rows. Each journal row starts with a NULL `sync_id`,
a top-level `txid` (`pg_current_xact_id()`), and a unique `entry_id` that preserves
insertion order within its transaction. A deferred constraint trigger runs at
commit: it takes advisory transaction lock `824180067`, assigns sequence IDs to
all pending rows of that transaction in `entry_id` order using a PL/pgSQL loop,
and updates the watermark. Remaining trigger firings find no pending rows and return immediately.
The lock is released at commit. Ordinary source writes never acquire this lock;
a long transaction with pending rows allows unrelated writers to commit.
Readers return only assigned IDs; pruning ignores unassigned rows.

Keep the `sync_allocate` constraint deferred; forcing it immediate with
`SET CONSTRAINTS` would acquire the allocation lock before the commit boundary.
The store does not change constraint timing. Existing rows are discovered by
the migration when it first installs the feed. If legacy tasks share a
decision request ID, backfill selects the newest `updated_at`, breaking ties by
the highest task ID.

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
returns an error; restart from the reset boundary.

## Desk ledger

`GET /v1/ledger?consumer=<name>` reads this feed for its "changed since last
ack" section; it is not a second feed. The consumer's cursor is stored in
`ledger_cursors` and moves only on `POST /v1/ledger/ack`. A cursor older than
the reset boundary gets the same 410 `{"reset":true,"min_sync_id":N}` as
delta. See README, "Desk ledger and project status updates".
