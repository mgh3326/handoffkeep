# Linear issue admission

hk saves every task, board projection and execution event locally. A task does
not receive a new Linear issue merely because it was claimed or placed.

## Mirror eligibility

`store.LinearMirrorReason` admits nonterminal tasks for one of these reasons:

- Priority is explicitly 1 or 2. Zero is the existing unset default.
- A structured decision request is open.
- `refs.linear.mirror_reason` is `dots`, `operator_visible`, or `linear_opened`.
- The legacy explicit `refs.linear.sync=true` opt-in is operator-visible.

The authenticated request producer records `dots`; explicit operator-visible
requests record `operator_visible`; operator-only Linear intake records
`linear_opened` after its actor check. Free text, labels, task kind, a PR, a
placement or an issue identifier alone do not grant eligibility. Label-based
configuration comes in a later unit. Partial refs patches preserve the reason.

Existing linked issues may still receive status/final updates after eligibility
changes; missing or archived issues are not replaced with new issues. A terminal
unlinked task never creates an issue. Intake with `linear_opened` reuses its
recorded issue id. Relation writers must use existing endpoint issues only.

## Serialized capacity admission

All production creates use `LinearAPI.CreateLinearIssue`. Its inputs include
`TaskID`, a stable issue `ID`, and, for decisions, the current `RequestID`.
Configure the shared API client with the store as `Admission`.

Before deployment enables new creation, verify that this credential can see the
whole workspace scope used for the plan limit, including private teams. Set
`HANDOFFKEEP_LINEAR_CAPACITY_SCOPE=workspace` only after that check. Unset or
unknown scope leaves creates pending; reads and existing-issue updates continue.
This assertion is necessary because an API-visible issue list cannot prove that
no inaccessible private issues exist. Do not set it for a team-restricted token.

The client holds a PostgreSQL session advisory lock across identity recovery,
current hk policy checks, counting and creation. Migration 31 adds reservation,
capacity-hold and board-ownership records. Count pages contain 100 non-archived
issues and advance to the creation ceiling; no workflow state is excluded.
Closed, backlog, triage and sub-issues therefore count too. Unknown scope, failed
reads, malformed/non-advancing pages, and the configured ceiling leave creates
pending. hk is the sole writer for this serialization guarantee.

Defaults follow the operator-tested workspace entitlement: limit **275**, create
ceiling **270**, archive start **230**, archive target **200**. The published
[Free plan](https://linear.app/pricing) still lists 250 issues / two teams; that
published number and the observed workspace entitlement are separate evidence.
The creation margin is five slots. Configure limits without a code change:

| Variable | Default |
|---|---|
| `HANDOFFKEEP_LINEAR_ISSUE_LIMIT` | `275` |
| `HANDOFFKEEP_LINEAR_ARCHIVE_HIGH_WATERMARK` | `230` |
| `HANDOFFKEEP_LINEAR_ARCHIVE_LOW_WATERMARK` | `200` |

Setting the issue limit to 250 changes the create ceiling to 245. Invalid policy
or inconsistent thresholds pause new creates. A vendor capacity rejection records
a durable hold even if the visible count appears below the configured ceiling.
It releases when count reaches the archive target, or after an explicitly lower
configured entitlement plus a verified count below that new ceiling. Rate limits
and ordinary field validation do not become capacity holds.

Before the HTTP create, hk commits a reservation for its stable issue id.
Board creates retain it until the signed Issue webhook establishes the active
mapping. They make no automatic identity search/readback. An ambiguous create
waits for webhook redelivery or explicit one-task repair. Other issue producers
retain their exact-id rejection/recovery contract. See
[the event-driven board runbook](linear-board-efficiency.md) for webhook setup,
manual metadata refresh, missed-webhook repair and shared rate-limit handling.

Old outbox payloads are checked against current task policy at send time and
again inside capacity admission. An excluded, unlinked task with no reservation
settles as skipped without a Linear call. A board create reservation waits for
the webhook even after task eligibility changes. Existing linked issues retain
maintenance updates; archive/remove webhooks stop delivery and drop active links.
Asking a decision retains its existing retry contract; capacity failure returns
unavailable without discarding the hk request.

## Ownership and projection versions

Before any board issue mutation, hk reserves one task-to-issue ownership pair.
Reusing a ROB title cannot take an issue owned by another task or a decision
request. Rebinding a task to a different issue is refused. Migration 32 quarantines conflicting legacy active links before adding unique
reverse indexes; every removed row is retained intact in
`linear_issue_link_conflicts` for inspection.
Backfill reserves the latest unambiguous issue per task by updated time, created
time, then issue id. An issue shared by tasks stays unclaimed. Every skipped
distinct pair is recorded in `linear_board_ownership_conflicts`; inspect these
rows before enabling affected writers. Existing conflicting links require
explicit repair even when the registry selected one candidate.
Ownership conflict suspends the outbox row as failed.

Each saved board projection increments its version under the task/projection
locks. Delivery uses the latest committed projection's fields, even for a late
old snapshot. Old event comments retain their own deterministic identity. The
existing per-task outbox order also serializes remote issue updates.

## Archive boundary

Terminal transitions enqueue the state and final comment. They do not enqueue
an immediate archive. Previously queued `terminal_archive` operations settle as
`skipped` before any remote callback executes. Keep closed issues until the
watermark owner admits archival.

The following archive PR implements copy-before-archive and protects links to
open work, decisions and ongoing assistant requests, recent activity/references,
and `keep` labels. The desk confirmed the archive design after the archived-read
investigation; it still ships flag-off and is enabled by the desk after merge.
It uses a short idle period for eligible closed issues and 230→200 as the hard
watermark backstop. The daily archival job remains a backstop.

Historical paginated reads explicitly request `includeArchived:true`. Exact-id
issue/comment reads have no such argument in the schema; their nested history
collections include archived resources. Active quota counting and candidate
selection deliberately request non-archived inventory. Copies retain relation
endpoints/types and parent identity; view filtering does not prove relation loss.

## Rollout, verification and rollback

1. Apply migration 31 using the normal deployment process. No task rows change.
2. Stop older writer instances before enabling creates with the new version;
   older binaries do not take this admission lock. Verify credential scope
   before setting the scope assertion. Without it,
   creation pauses safely. Inspect existing pending operations and any durable
   reservations before enabling a writer.
3. With a controlled eligible task, verify one issue and its stable hk link.
   With an ordinary priority-3 task, verify its console projection exists and
   no create operation or issue is produced.
4. Verify current non-archived count and pending behavior at the watermark.
   Do not create hundreds of fixture issues to test production limits.
5. If delivery misbehaves, disable the work-board writer. Preserve its outbox,
   reservations and links for inspection. Do not roll back to an unbounded writer
   or an immediate terminal archiver. The new table may remain during rollback.

For an uncertain reservation, inspect `linear_create_admissions.issue_id` and
read that exact issue with a credential of verified scope. If it exists, a normal
retry recovers it. If its acceptance cannot be determined, keep the reservation.
Only after independently establishing that no request can still create that id
may an authorized repair remove the reservation and retry the same stable id.
Never blindly clear the table or retry with a different id.
