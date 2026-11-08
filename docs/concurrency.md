# Serve concurrency

`serve` starts the workers below in each process. The two session locks use
separate pooled connections. A standby retries every five seconds and starts
its worker after the active server releases its lock. The active worker probes
the lock session every second and stops if the probe fails. Advisory locks are
scoped to a PostgreSQL database, not a schema.

| Loop | Writes and concurrent behavior | Guard | Test |
| --- | --- | --- | --- |
| Chat retention | Deletes expired terminal chat rows. Concurrent deletes cannot delete a row twice. | PostgreSQL row locks and delete predicates. | No dedicated prune test |
| Placement retention | Deletes expired placement rows. Concurrent deletes cannot delete a row twice. | PostgreSQL row locks and delete predicate. | `TestWorkBoardCommittedEventsAndPlacementRetention` |
| Sync retention | Deletes an old prefix and advances the reset boundary. | `sync_state` row lock serializes pruning. | `TestSyncPruneReset` |
| Backlog sweep, when enabled | Moves idle tasks and writes source events. A concurrent sweep rechecks eligibility after locking the task. | Task mutation advisory lock and task row lock. | `TestBacklogSweep` |
| Last activity repair, once at startup | Raises stale task activity clocks and emits sync rows. The guarded update prevents a second update at the same timestamp. | Conditional `UPDATE` under the task row lock. | `TestRepairLastActivity` |
| Linear board, when configured | Drains board outbox rows and periodically rebuilds projections and queues deliveries. Two reconciliations would queue duplicate refreshes. | Session lock `LinearBoardWorkerLock` for the worker; outbox rows also use `FOR UPDATE SKIP LOCKED`. | `TestSingletonServeWorkers/linear_board`, `TestWorkBoardCommittedEventsAndPlacementRetention` |
| Linear decision ops, when configured | Sends Linear comments, state changes, and Discord messages; settles queued ops. | `FOR UPDATE SKIP LOCKED` holds each op row through the send. | `TestLinearDecisionOpsWorkerSendsEachKind` |
| Hub notification forwarder, when configured | Posts outbox events to the hub and records receipts. The hub deduplicates event IDs, but simultaneous posts otherwise occur. | Session lock `HubForwardWorkerLock`; hub deduplication also covers retries after a lost receipt. | `TestSingletonServeWorkers/hub_forwarder`, `TestForwardLostReceiptDoesNotDeliverTwice` |
| Decision notifier | Posts or edits Discord notices and posts a daily digest. | Delivery and digest rows use `FOR UPDATE SKIP LOCKED` through the send. The digest insert also serializes the first claim of a day. | `TestDecisionDigestOncePerUTCDay`, `TestDecisionNoticeSettlesAfterCancel` |

The HTTP listeners, shutdown coordinator, per-request SSE poll and heartbeat,
and Cloudflare Access key refresh do not run shared write jobs. The optional
Sentry client sends telemetry for each process's own requests. YouTrack sync
is a CLI command, not a `serve` worker; its session lock is
`YouTrackSyncLock`. Translation and SLA operations are request or CLI work,
and the reap endpoint reads candidates. The legacy Linear drain is not
started by `serve`; its CLI uses `LinearDrainAdvisoryLock`.

These guards prevent concurrent processing while the database session or row
transaction remains alive. If a lock session fails, a standby can acquire the
lock before the next probe cancels the former owner. They do not guarantee exactly-once delivery across
a process or database failure after a remote service accepts a request but
before the local receipt commits. The hub deduplicates event IDs; Discord's
notice sender treats an unknown post outcome as a refusal to retry.
