# O3 records and wrk transport

The server journal implements the fixed hk1873.v1 contract. Claims, grants,
rounds, evidence, joins, and lifecycle receipts are append-only records. A
completion report is an observation; a join requires the prescribed review
history or an explicit zero-round waiver. Imported events are untrusted archive
bytes and cannot establish a PASS or grant.

## Server configuration

WRK_RECORDS_API accepts off (the default), shadow, or enforce. Off with no O3
tables and no migration URL runs the existing legacy migrations only. Explicit
O3 provisioning uses HANDOFFKEEP_MIGRATE_DB_URL, separately from
HANDOFFKEEP_DB_URL. Once the four O3 tables exist, runtime safety is checked even
while off. Runtime must not own the immutable tables, inherit their owner role,
be a superuser, bypass RLS, or have UPDATE, DELETE, or TRUNCATE on records,
contents, or attempts. Credential issuance and legacy table ownership transfer
belong to the D1 desk operation.

Enabled startup requires WRK_RECORDS_POLICY_FILE, a distinct migration path,
and a configured WRK_RECORDS_STORE_GENERATION UUID. Generation is never created
at boot. The existing authentication file grammar is unchanged; duplicate
client ids or equal bearer values fail startup in every mode. The example
[wrk-policy.example.json](wrk-policy.example.json) contains only synthetic
nonsecret policy data and client ids, with no authentication material.

Policy snapshots are strict JSON validated against the closed eleven-client
schema. Their digest uses RFC 8785. ConfigureWrkEngine validates referenced
canonical lane edges under advisory lock 8241873001 before atomic publication;
a rejected replacement retains the old snapshot. There is no client policy
reload endpoint. Enabled writers must use the same policy digest.

WRK_RECORDS_RECOVERY_REQUIRED=true freezes authority operations. The four
operator decisions dr-1925-1=A are named in internal/wrkrecords/decisions.go:
central read-only recovery access, desk-owned migration credentials, subject
protection starting at activate, and P1 reviewer eligibility based on the
subject's phase. Reviewer lifecycle protection starts at its accepted claim.
Existing receipt fences continue to deny bypass attempts while the API is off.
Activation enrolls only the latest unsuperseded builder/worker claim's task;
superseded mistaken task bindings remain history. Ended predecessors preserve
their enrollment through succession. Review-role claims enroll no task.

For receipt-required jobs, completed/joined and their relay projection commit
together. The canonical upsert replaces any planted legacy tuple and resets
delivery. Every projection read derives its receipt and report digest from the
matching immutable record and content; a mismatch leaves the notification
visible without a receipt. A shadow subject's completion remains an observation
and cannot become a protected completion receipt after activation. Review-role
lifecycle remains receipt-required from claim.

## CLI

The installed handoffkeep configuration supplies the authenticated transport.
The namespace accepts no endpoint, token, token-path, or transport-selection
flag. Every redirect is refused. Success writes one validated JSON object and a
newline; failure writes no stdout and exactly `handoffkeep wrk: CODE` on stderr.

| Command after handoffkeep | Stdin | Response |
| --- | --- | --- |
| wrk capabilities | empty | Capabilities |
| wrk job JOB | empty | JobResponse |
| wrk records JOB [--after-seq N] [--snapshot-seq S] [--limit L] | empty | RecordsResponse |
| wrk content JOB SEQ | empty | ContentResponse |
| wrk append JOB --kind K --idempotency-key U --expected-seq N --claim-seq C | exact Input_K JSON | WriteResponse |

One invocation makes at most one network attempt. The caller keeps the original
key and body after an uncertain outcome. C1 owns canonical binary selection,
environment scrubbing, durable intents, retained-receipt comparisons, retries,
and local event/cleanup projection; this server does not implement them.

A successful replay returns the original record and committed summary, evaluated
at that record's received_at even after a grant expires. Fetch the current job
before the next decision. An unreadable stored timestamp fails closed.

## Verification

The unchanged M1 pack drives the pure transition engine, the real HTTP/store
paths, and the built CLI. PostgreSQL tests run only in CI and fail if CI has no
test database. Local test invocations use isolated temporary homes and no test
DB URL. The per-source inventory and C1-only observable split are pinned in
internal/wrkrecords/pack_coverage_test.go and cli_vectors_test.go. Independent
design-derived tests cover replay ordering, assignment freezing, seal integrity,
cap/grant accounting, import trust, lifecycle receipts, and policy scope.
