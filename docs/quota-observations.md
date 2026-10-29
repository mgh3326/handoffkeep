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
  Each bucket contains `window`, `used_pct` (0–100), `scope` (`account` or
  `model`), optional `model` and `resets_at`. Manual entries also require
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
- `DELETE .../lease` accepts `{holder}`; releasing a previous holder cannot
  remove a successor's lease. It returns 204, including when already released.

Write paths (observation PUT, lease POST, lease DELETE) are scoped to the
client's own account keys. The client-to-accounts mapping lives in the server
auth file next to the tokens: each `HANDOFFKEEP_QUOTA_<CLIENT>` line grants
that client a comma-separated list of account keys. A bad quota line — an
invalid key, an empty client id, or a client with no
`HANDOFFKEEP_TOKEN_<CLIENT>` — is logged to stderr and skipped, never a
startup error; the client simply has no quota scope. Token-line validation
is unchanged. A write to an unlisted or foreign `account_key` returns 403
`quota_account_forbidden`; clients with no grant lines may not write at all.

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

`TestQuotaPostgres` uses `HANDOFFKEEP_TEST_DB_URL` and the CI Postgres service;
it skips without that variable. Do not start a database locally for these tests.
