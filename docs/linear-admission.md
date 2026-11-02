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
current hk policy checks, counting and creation. Migration 31 adds reservations and the board ownership registry. A count requests up to 245 non-archived issues, without a
workflow-state filter: closed issues count too. Fewer than 245 plus a complete
page permits a create. At 245, unknown scope, failed reads, malformed responses,
or an incomplete count, creation stays pending. hk is the sole writer for this
serialization guarantee; independent external creates cannot share its lock.

Before the HTTP create, hk commits a reservation for its stable issue id.
Successful creation clears it. Ambiguous failure leaves it durable, including
across process restarts. Later admission first reads that exact id: an existing
issue recovers the lost response, even when the watermark has since been
reached. An unresolved reservation blocks new identities instead of assuming
the previous request did not reach Linear. A received 4xx or GraphQL rejection gets exact-id readback: absence clears the
reservation and permits retry, while an existing issue recovers the accepted
write. A failed rejection readback preserves a durable known-rejection record, so
a later successful absent read clears it automatically. Transport errors and 5xx retain
the reservation. There is no automatic destructive repair.

Board outbox failures remain pending with the existing backoff and `last_error`.
An old outbox payload is checked against current task policy at send time and
again inside capacity admission. Before skipping an ineligible unlinked task,
read-only recovery checks its deterministic issue id; an accepted issue is
linked and receives its current/final state, without fresh creation. A confirmed
missing issue settles as skipped only when no uncertain reservation remains. Asking a decision through the API retains its existing retry contract;
capacity failure returns unavailable without discarding the hk request.

## Ownership and projection versions

Before any board issue mutation, hk reserves one task-to-issue ownership pair.
Reusing a ROB title cannot take an issue owned by another task or a decision
request. Rebinding a task to a different issue is refused. Historical duplicate
links are retained for inspection and refused; they are not silently rewritten.
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

The Free plan has 250 non-archived issues and two teams ([pricing](https://linear.app/pricing)).
The create ceiling is 245, retaining five slots of safety margin. The archive
watermarks are 210 to start and 180 as the target.

The separate hk watermark archiver will implement copy-before-archive and
protect links to open work, decisions and ongoing assistant requests, recent
activity/references, and `keep` labels. It will default off pending confirmation
that archived issues remain usable by the operator and assistant. The existing
daily archival job remains the backstop. This admission change does not invoke
that job or archive issues.

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
