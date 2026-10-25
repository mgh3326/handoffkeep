# O3 wrk journal (`/v1/wrk`, `handoffkeep wrk`)

The server authority for wrk claims, review rounds, verdicts, grants and
joins (hk 1873 design r4, `hk:doc designs/hk1873/b1873a-sol-r4`, body
sha256 `ce4f6efb…`; S1 is hk 1925). The design and the M1 contract pack
under `testdata/o3/` are the specification; this page covers operation.

## Flags and startup

| Setting | Meaning |
|---|---|
| `WRK_RECORDS_API` | `off` (default), `shadow` or `enforce`. Off: every `/v1/wrk` route answers `503 wrk_unavailable` after authentication and the legacy routes behave as before. Shadow: journal writes run as observations; `activate` needs `enforce`. |
| `WRK_RECORDS_RECOVERY_REQUIRED` | `true` freezes the journal: every write answers `503 wrk_recovery_required`; only director-1/operator-desk reads stay available (operator answer dr-1925-1). |
| `HANDOFFKEEP_WRK_POLICY_FILE` | The nonsecret policy file (below). Required when the API is enabled; a missing or invalid file stops startup. |
| `WRK_STORE_GENERATION` | Canonical lowercase UUID of the current store history. Required when the API is enabled; set a fresh one after any point-in-time restore. Never generated at boot. |
| `HANDOFFKEEP_DB_MIGRATE_URL` | Connection for the journal owner role, distinct from `HANDOFFKEEP_DB_URL` (the runtime role). Only this path creates the O3 tables (schema version 20) and grants the runtime role `SELECT, INSERT` on them. Required when the API is enabled; there is no single-URL fallback. The credential is a desk deploy step and is never printed or committed. |

Startup rules:

- With no migrate URL and no journal tables, only the legacy migrations
  run and no O3 table is created.
- Once the journal tables exist, every startup (any mode) refuses a runtime
  role that is a superuser or `BYPASSRLS` role, owns (or is a member of the
  owner of) `wrk_records`, `wrk_contents` or `wrk_attempts`, or holds
  `UPDATE`, `DELETE` or `TRUNCATE` on them.
- Every startup refuses an auth file with a repeated client id or two
  clients sharing one token value. The auth file grammar is unchanged.

## Policy file

The policy names the closed list of eleven client ids, their permission
lists (each within the fixed per-client ceiling), the root lane map and the
two required feature switches. It contains no token material. Its digest
(`policy_sha256` in capabilities and in every claim) is SHA-256 over the
RFC 8785 form of the whole object. `docs/o3-policy.example.json` is the full
ceiling policy; attenuate by removing permissions.

## CLI

`handoffkeep wrk` uses the installed remote configuration only (environment
or `~/.config/handoffkeep/config.env`); no flag selects a token, URL or
transport, and no redirect is ever followed.

```text
handoffkeep wrk capabilities
handoffkeep wrk append JOB --kind K --idempotency-key U --expected-seq N --claim-seq C   < Input_K payload
handoffkeep wrk job JOB
handoffkeep wrk records JOB [--after-seq N] [--snapshot-seq S] [--limit L]
handoffkeep wrk content JOB SEQ
```

Success prints exactly one JSON object and a newline. Every failure prints
nothing on stdout and exactly one line `handoffkeep wrk: CODE` on stderr, with
the design §6b exit map (for example `round_cap` 78; `wrk_unavailable` and
transport/protocol codes 75; `scope_denied`/`host_mismatch` 77; request
errors 65; business refusals 73; argument errors 2). The durable intent
file and retry scheduling belong to the wrk client (C1, hk 1926).

## Legacy fences

For a task bound to an activated canonical subject, the legacy task routes
refuse any job binding, claimant, lane or project change
(`409 wrk_receipt_required`), and a state change needs director-1 or
operator-desk with a `wrk_receipt` (`job_id`, `seq`, `record_hash`,
`store_generation`) naming the subject's current claim, its `joined` (for
`join`/`merged`) or its `end` (for `dropped`). Terminal relay publications
(`job.completed`, `job.joined`, `job.revoked`) for a receipt-required job
need the job's host and a matching receipt; a canonical `completed` or
`joined` already wrote its relay projection, which the legacy retry
deduplicates to. Unrelated tasks and jobs keep their behaviour.
