# Event-driven Linear board

hk owns tasks and their execution history. The Linear board stays enabled as a
bounded active-issue view. `LinearBoardWorker` drains hk's transactional outbox;
it never schedules a full task reconcile or scans Linear for inbound changes.
The hourly timer reports request counts only. Existing signed webhooks handle
inbound changes. No metadata cache, TTL, periodic refresh or SQL mirror filter
is introduced here.

## Metadata in hk

The first board delivery fetches the ROB team, its workflow states and paginated
labels once. A database transaction serializes this initialization and records
success durably. Later deliveries read `linear_metadata`. Failures do not mark
initialization complete. Label creation also records its returned metadata.

Subscribe the existing `/hooks/linear` receiver to Issue and IssueLabel events,
using `HANDOFFKEEP_LINEAR_WEBHOOK_SECRET`. IssueLabel webhooks update labels;
Issue webhooks update nested state/team metadata when those fields are present.
Newer entity timestamps win over delayed snapshots. Removed metadata remains a
tombstone. Standalone team/state rename webhooks are not promised by Linear's
[published webhook support](https://linear.app/developers/webhooks).

For a rename or other metadata change that has no webhook, explicitly run:

```sh
handoffkeep linear metadata refresh
```

This uses the usual hk URL, bearer token and optional Access configuration. It
calls authenticated `POST /v1/linear/metadata/refresh` with `{}`. It performs one
snapshot, updates the database without overwriting newer webhook values, and
resets failed label attempts and the board's missing-label circuit after success.
It does not clear a vendor rate-limit pause. No timer calls this endpoint.

## Webhook-owned links

A board create uses `LinearDecisionUUID("work-board:task:<id>")` and embeds
`<!-- work-board:task:<id> -->` in the description. Outbound code commits only a
create reservation; it does not populate `linear_issues`, perform identity
searches, or acknowledge the initial delivery before the webhook.

The signed Issue create/update webhook checks the marker and committed create
reservation (or previously reserved ownership for an explicitly reused issue),
then writes `linear_issues(task_id, issue_id, identifier)`, updates the task's
references and completes the reservation in one transaction. The task key,
issue id and identifier are unique. Outbound updates reserve one-to-one ownership
before mutation, but ownership is separate from the active webhook mapping.

Task lookup reads hk's mapping. A known Linear id or identifier uses one issue
fetch; the board does not use content search. Creation still uses serialized
capacity admission. Accepted creates awaiting webhooks also reserve capacity
slots until acknowledgement, so delayed inventory visibility cannot undercount
new issues; a visible issue may be conservatively counted twice briefly. A successful create waits for its webhook; a transport or
ambiguous GraphQL failure retains the reservation without retrying a duplicate
create. An unresolved ambiguous reservation holds new creates conservatively.
It never triggers an automatic issue search.

For a missed webhook, prefer redelivery of the original signed event. To repair
one deterministic board issue explicitly:

```sh
handoffkeep linear board reconcile --task 123
```

Authenticated `POST /v1/linear/board/reconcile` takes `{"task_id":123}`. It reads
that single issue, verifies the active issue/team/ownership, retains a reservation
and changes only hk's description marker. The resulting signed Issue webhook
establishes the mapping. The command does not create an issue or write the link.
It refuses an archived issue or a task suspended by archive/remove. Never infer
that an issue was deleted from its absence in a list, timeout or failed fetch.

Only non-archived issues are mapped and synced. An archive/remove webhook drops
the active mapping and task reference, retains an event-time receipt, and
suspends that task's delivery so it cannot create a replacement. Delayed events
cannot restore the mapping. Unarchive/replacement is a separate decision. Archive
content stays on the existing archive-to-hk document path; this PR does not add
an archiver or alter archive eligibility.

## Backlog, labels and rate limits

Migration 32 marks all but the latest pending `work_board` snapshot per task as
`skipped`, retaining rows and the supersession reason. It resets the retained
pending rows' retry times once. Sent history and other outbox kinds are unchanged.
A superseded event's comment is not replayed; hk retains the canonical event.
Normal per-task ordering and projection versions remain in force.

The migration adds unique reverse indexes. If legacy active links conflict, a
registry-owned pair wins, then newest update/create time, then task id. Removed
live-link rows are copied intact into `linear_issue_link_conflicts`; inspect that
audit before operating affected tasks. No issue is changed by this repair.
The previously added ownership-conflict audit also remains available.

A missing required label is created once under a database lock. A definitive
create/readback failure is durable, opens one board circuit and logs one error.
Other task rows are left pending. A matching label webhook or explicit metadata
refresh supplies a recovery path; an explicit successful refresh permits another
creation attempt. Transport failures retain normal backoff instead of being
misclassified as a missing label.

HTTP 429 and GraphQL rate-limit errors pause the whole worker and shared Linear
client. `Retry-After` seconds or HTTP-date is honored. Without a valid value the
pause backs off from one minute to one hour. The current row retains its retry
deadline; no other row is attempted during the pause. No sleep holds a DB lock.

## Verification and rollback

Deploy the current writer and webhook handler together; stop old writer instances
so their hourly sweep cannot continue. Migration 32 locks `linear_issues` while
quarantining conflicts and creating indexes, locks superseded outbox rows, and
adds a nullable task reference on create reservations. It performs no Linear
calls. The coordinating deployment owner runs migrations and rollout.

Verify one controlled create remains unmapped until its signed webhook, then
updates through the stored link. Check pending count after the one-time collapse,
metadata initialization, webhook subscriptions and rejected/ambiguous reservations.
Use `linear_board.calls elapsed_seconds=... calls=... calls_per_hour=...` to measure
actual post-deploy traffic; it counts the shared hk Linear HTTP client, including
decision calls, and excludes other processes using the same OAuth account.

Fake runs record request counts and a simulated idle hour; PostgreSQL CI verifies
migration, link ownership and the closed-task zero-call case. These are not a
measurement of production traffic. Do not generate production capacity fixtures.

Retain migration tables, mappings, reservations, skipped rows and conflict audits
on rollback. Restoring an old writer also restores its sweep and outbound mapping
behavior; coordinate rollback with the deployment owner rather than running old
and new writers concurrently. No automatic unarchive or destructive cleanup is
part of rollback.
