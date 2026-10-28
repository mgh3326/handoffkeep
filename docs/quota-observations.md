# Shared account quota

All routes use the same bearer authentication as `/v1/bench/scores`. Account
keys are `<pool>:<16 lowercase hex fingerprint>`, using pacelever's existing
SHA-256 prefix of the account identity. Send no raw identity, tokens or host data.

- `GET /v1/quota/accounts/{key}/latest` returns `{observation, manual: []}`.
  An unseen account returns 200 with a null automatic observation.
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
  freshness is reader policy (default five minutes in pacelever).
- `DELETE .../lease` accepts `{holder}`; releasing a previous holder cannot
  remove a successor's lease. It returns 204, including when already released.

A write without a live lease belonging to both the authenticated client and the
holder returns 409. Acquisition and renewal use the database clock. Publishing
locks the lease row through commit, fencing expired probes after takeover.
Older measured-at values append to history but never move the latest pointer
backward. Manual pointers are per window, independently of automatic readings.

Postgres was chosen over Redis so fencing, append and pointer updates share one
transaction and one recovery domain. No extra service or Redis configuration is
needed. The additive startup migration creates `quota_observations`,
`quota_latest` and `quota_leases`, plus a trigger forbidding history mutation.
Deploy hk first: normal `store.Open` applies the idempotent migration before
serving requests. Then enable pacelever clients. Existing routes are unchanged.

`TestQuotaPostgres` uses `HANDOFFKEEP_TEST_DB_URL` and the CI Postgres service;
it skips without that variable. Do not start a database locally for these tests.
