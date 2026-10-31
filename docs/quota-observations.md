# Shared account quota

All routes use the same bearer authentication as `/v1/bench/scores`. Account
keys are `<pool>:<16 lowercase hex fingerprint>`, using pacelever's existing
SHA-256 prefix of the account identity. Send no raw identity, tokens or host data.

- `GET /v1/quota/accounts/{key}/latest` returns `{observation, manual: []}`.
  An unseen account returns 200 with a null automatic observation. Reads are
  open: every authenticated client may read any account's latest. Each
  observation carries `confirmed_at`: the newest `measured_at` at which the
  latest value was republished (null on rows written before the column
  existed).
- `POST .../lease` accepts `{holder, ttl_s}`. Use a new random 32-hex holder
  per acquisition attempt. TTL defaults to 120 seconds, maximum 120. A 200
  response includes `acquired`, `holder`, `expires_at`; contention is not an
  error. Reuse the acquired holder to renew before expiry.
- `PUT .../observation` accepts `{holder, observation}`. Observation fields:
  `kind` (`auto` or `manual`), `measured_at` (RFC3339), optional `plan`, `buckets`.
  Each bucket contains `window`, `used_pct` (0–100), `scope` (`account`,
  `model`, or `group` for a provider metering group such as Antigravity's
  `gemini-5h`/`3p-weekly`, named in `model`), optional `model` and `resets_at`. Manual entries also require
  `window` and `expires_at`; one account-scoped bucket sets that window, an
  empty bucket list clears it. Readers enforce manual expiry. Automatic
  freshness is reader policy (default five minutes in pacelever). A new
  observation returns 201. Republishing a value identical to the current
  latest for the same `(account_key, kind, window_key)` — compared without
  `measured_at`, so kind, window, plan, expires_at and the full bucket set
  must match — appends nothing and returns the existing row with 200 while
  advancing that pointer's `confirmed_at` to the new `measured_at` (never
  backward). The lease-row lock serializes publishers, so concurrent
  identical publishes cannot double-append.
- Free bucket (Devin SWE-2 free profiles). `PUT .../observation` also accepts
  an optional `observation.free_bucket` of exactly `{"state","resets_at"}`.
  It is valid only on an `auto` observation of a `devin:` account key; any
  other key or kind returns 400 `invalid_quota_observation`. `state` is
  `available`, `exhausted` or `unknown`. `exhausted` requires a nonzero
  RFC3339 `resets_at`; `available` and `unknown` must omit it (an unreadable
  limit record is `{"state":"unknown"}`). Unknown fields inside it are
  refused like any other. A valid free-only automatic observation may send
  `"buckets": []` (or omit `buckets`); it is stored and returned as `[]`.
  Without `free_bucket`, an automatic observation still needs at least one
  percentage bucket, and manual observations are unchanged. The field is
  stored with the observation, returned unchanged by `latest`, and counts in
  the identical-republish comparison (a different `state` or `resets_at` is a
  new value). The server does not interpret it: expiry (an elapsed
  `resets_at` reads as available) and freshness stay reader policy, and it
  says nothing about paid credits.
- `DELETE .../lease` accepts `{holder}`; releasing a previous holder cannot
  remove a successor's lease. It returns 204, including when already released.

Write paths (observation PUT, lease POST, lease DELETE) are scoped to the
client's own account keys. The client-to-accounts mapping lives in the server
auth file next to the tokens: each `HANDOFFKEEP_QUOTA_<CLIENT>` line grants
that client a comma-separated list of account keys, bare pool names, or `*`.
An exact key (`claude:0123…`) scopes the one account; a bare pool name
(`claude,codex,devin`) scopes every account key of that pool; `*` scopes
every account. A bad quota line — an invalid key or pool, an empty client id,
or a client with no
`HANDOFFKEEP_TOKEN_<CLIENT>` — is logged to stderr and skipped, never a
startup error; the client simply has no quota scope. Token-line validation
is unchanged. A write to an unlisted or foreign `account_key` returns 403
`quota_account_forbidden`; clients with no grant lines may not write at all.
Ordering matters for the new forms: a server running a binary that predates
pool grants skips a grant line that contains a pool name or `*` — including
any exact keys on the same line — so every server must run the new binary
before pool or wildcard grants are added, and a rollback first restores
exact-key-only grant lines.

A write without a live lease belonging to both the authenticated client and the
holder returns 409. Acquisition and renewal use the database clock. Publishing
locks the lease row through commit, fencing expired probes after takeover.
Older measured-at values append to history but never move the latest pointer
backward. Manual pointers are per window, independently of automatic readings.

Postgres was chosen over Redis so fencing, append and pointer updates share one
transaction and one recovery domain. No extra service or Redis configuration is
needed. The additive startup migration creates `quota_observations`,
`quota_latest` and `quota_leases`, plus a trigger forbidding history mutation,
and records `schema_version` 23 — idempotent on servers that already ran the
unversioned quota migration. Deploy hk first: normal `store.Open` applies the
idempotent migration before serving requests. Then enable pacelever clients.
Read routes are unchanged; the quota write routes (publish, lease, release)
now need a `HANDOFFKEEP_QUOTA_<CLIENT>` grant. Add those lines when the shared
clients are enabled, not in the same restart as the deploy, so a binary
rollback stays possible. A malformed grant line is logged and skipped.

## Host presence

Presence records which host last measured an account, so placement (runtug)
can prefer a host that already holds it. It is a separate table from the
observations, which still carry no host identity.

- `PUT /v1/quota/presence` accepts exactly
  `{"host_id":"host-<stable-id>","account_key":"<pool>:<fingerprint>","last_seen":"<RFC3339>"}`
  (pacelever publishes it after a successful shared measurement, best-effort).
  `host_id` is an opaque label of 1–64 characters `[A-Za-z0-9._-]` starting
  with a letter or digit — never a raw hostname; `account_key` uses the
  observation key format. Unknown fields, a second JSON value or a body over
  32 KiB return 400 `invalid_quota_request`; a bad field, a missing
  `last_seen`, one more than a minute ahead of the server clock or one older
  than the 30-day retention returns 400 `invalid_quota_presence`. The write
  uses the observation PUT's grant: an `account_key` not listed in the
  client's `HANDOFFKEEP_QUOTA_<CLIENT>` lines returns 403
  `quota_account_forbidden`. No lease is needed.
- One row is kept per `(host_id, account_key)`; the newest `last_seen` wins and
  an older one leaves the row unchanged. A `last_seen` inside the one-minute
  tolerance is clamped to the database clock, so no row sits in the future.
  The response is 200 with the stored record, which may be newer than the
  request.
- Presence is bounded per account: at most 64 hosts, each kept for 30 days
  after its `last_seen`. Every write first prunes that account's rows past
  retention. A host new to an account that already has 64 rows gets 409
  `quota_presence_full`; hosts already present keep updating, so invented
  host ids cannot evict them.
- `host_id` is bound to the client only by an optional
  `HANDOFFKEEP_HOSTS_<CLIENT>=<id>[,<id>]` auth-file line (ids match
  `^[a-z0-9-]{1,32}$`, a node's `HUB_MACHINE_ID`). A client with a line may
  publish presence only for the hosts it lists; any other `host_id` returns
  403 `quota_host_forbidden`, after the `account_key` grant is checked. A
  client with no line is still accepted, as before, and the server logs
  `api.host_grants_missing` once per client per process. Several lines for
  one client add up. A malformed line (a bad id, an empty entry, a missing `=`) is logged as
  `api.host_grants_skipped` and skipped, never a startup error, but it fails
  closed: its client still counts as having a line, with only the hosts its
  good lines list, so a client whose only line is bad is refused every host
  until the line is fixed. A line with no client id, or for a client with no
  token, is logged and dropped. Without a
  line, a granted client is trusted for the whole account and may write any
  host's row; the host's own next publish moves it forward again (a spoofed
  `last_seen` can be at most the server clock). Enforcement for clients
  without a line is a later change, once every client has one.
- `GET /v1/quota/presence` returns `{"records":[...],"truncated":false}`,
  newest `last_seen` first. `records` is always a list (`[]` when empty),
  never null. Optional `?host_id=` and `?account_key=` filters must be valid
  values (else 400 `invalid_quota_presence_filter`) and combine with AND. One
  response holds at most 1000 records; `truncated` is true when more exist.
  Rows past retention are never listed, even before a write prunes them, so
  one account contributes at most 64 records and an `account_key` filter is
  always complete. Reads are open to every authenticated client, like
  `latest`. Readers apply their own freshness TTL on top.

The startup migration adds `quota_presence` and records `schema_version` 24
with `IF NOT EXISTS` and `ON CONFLICT DO NOTHING`, so a restart is a no-op
and an older binary on the same database simply ignores the table. Presence
writes need the same grant lines as observation writes; no new auth-file
entries are required.

No schema change: `free_bucket` lives in the observation's existing JSONB
value, so there is no migration and no `schema_version` bump, and existing rows
are untouched. Rollout dependency: until this server is deployed, a client that
sends `free_bucket` is refused with 400 (unknown field), which pacelever treats
as a publish failure and falls back to its stale/degraded path; an older client
is unaffected after the deploy. `TestQuotaFreeBucketPostgres` covers the
accepted bodies, dedup and rejections.

## Retention

`quota_observations` rows age out after 90 days (by `received_at`, the server
clock). The only delete path is the `quota_prune_history()` SQL function —
`store.PruneQuotaHistory` wraps it — which sets a transaction-local flag the
`quota_history_immutable` trigger checks before allowing its own DELETE. The
prune never deletes a row that `quota_latest` points to, even when old.
Every other UPDATE, DELETE and TRUNCATE stays blocked. No scheduler or cron
is registered: cadence needs separate approval, so the function is called
manually or by a later approved job.

`TestQuotaPostgres`, `TestQuotaPresencePostgres` and
`TestQuotaPresenceAPIPostgres` use `HANDOFFKEEP_TEST_DB_URL` and the CI
Postgres service; they skip without that variable. Do not start a database locally for these tests.
