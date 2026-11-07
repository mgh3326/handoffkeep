package store

// Lock order.
//
// Every transaction in this package takes its locks in the order below,
// skipping the levels it does not need. Two transactions that both follow it
// cannot wait on each other in a cycle (SQLSTATE 40P01). The exceptions are
// listed at the end, each with the reason it cannot close a cycle. The
// lock_order_test.go tests drive the cycles this order removed.
//
//  1. Schema locks. DDL runs only in Store.migrate, MigrateWrk and their
//     version-gated steps, under pg_advisory_xact_lock(migrationLockID).
//     Open runs Store.migrate only when the schema is missing a version it
//     writes (migrateIfBehind, migrateCurrent), so opening a store beside
//     live writers takes no table lock. Each start used to
//     re-run CREATE INDEX IF NOT EXISTS (SHARE on tasks, then on task_events)
//     and ALTER TABLE ... IF NOT EXISTS (ACCESS EXCLUSIVE on task_events),
//     which deadlocked with any writer that touched task_events before it
//     updated tasks. DDL that must run (an upgrade) locks tables in statement
//     order, which no row order can match; Open retries it on 40P01
//     (retryDeadlock, migrateAttempts).
//
//  2. The task-mutation lock, pg_advisory_xact_lock(8241873001). A
//     transaction takes it first when it locks more than one task row, or
//     when it locks a task row and also a job lock from level 3. Holders run
//     one at a time, so the order in which a holder locks task rows (a child
//     before its parent in autoCloseTx and applySourceEventTx, ascending id
//     in applyDeployRecordTx and recordLinearBoardRelayTx) cannot form a
//     cycle with another holder. UpdateJob, AppendRelayEvent (work board on,
//     or an event that answers or ends a decision: relayTouchesDecisions),
//     WrkAppend (completed or joined records) and PutDocument (deploy
//     records) take it first for that reason. Taking it again in the
//     same transaction (applyJobRoleTx, applyDeployRecordTx) is a no-op:
//     advisory locks are re-entrant.
//
//  3. Job locks: wrk_job_locks rows in job_id order (WrkAppend sorts them),
//     then the jobs row (UpdateJob).
//
//  4. Task rows: SELECT ... FOR UPDATE, or an UPDATE of tasks. Without the
//     level-2 lock a transaction locks at most one task row and no job lock
//     (the single-task edits: area, milestone, labels, subscribers,
//     deadlines, placement, work-board reconcile).
//
//  5. Rows that belong to a locked task: task_events (its foreign key takes
//     KEY SHARE on the task row), task_labels, task_relations,
//     task_subscribers, task_origins, linear_board_tasks, linear_outbox,
//     sync_decisions, ticket_sets, decision_answer_attempts and
//     decision_resolutions (inserted by ResolveDecision after its task row,
//     under the level-2 lock; append-only and never locked by a reader), and
//     the work record: changes, change_tasks and reviews. ApplyTaskSourceEvent
//     writes the work record under the level-2 lock after its task row
//     (recordChangeTx); a revert locks two changes rows, the reverted PR's
//     first. Shared rows read FOR SHARE (labels).
//     A documents insert or update that sets task_id takes KEY SHARE on that
//     one task row through the foreign key (PutDocument). That is the level-4
//     single-task case and needs no level-2 lock: no task writer locks a
//     documents row while it holds a task row (IntakeTask inserts new keys;
//     applyDeployRecordTx runs under PutDocument's own level-2 lock, after the
//     document row). The dots writer (RecordDotsResult) is the same case: it
//     inserts or updates one documents row and one dots_results row, each
//     taking KEY SHARE on the one linked task row; SetDotsAdoption locks one
//     dots_results row and no task. decision_requests rows that name a
//     task or a job are updated only under the level-2 lock, several at a
//     time in id order (decision_records.go); the table has no foreign key.
//     FinishDeploy with state success takes the level-2 lock first, then its
//     deploys row (FOR UPDATE; no task writer locks a deploys row), then the
//     merged task rows linked to the attempt's changes in ascending id
//     (deployedTasksTx), then their rows through applySourceEventTx. A
//     failed or rolled_back finish locks only its deploys row.
//
//  6. At commit: the deferred sync allocator (handoffkeep_sync_allocate)
//     takes pg_advisory_xact_lock(824180067) and then the sync_state row,
//     after all other work of its transaction.
//
// Exceptions, none of which can close a cycle:
//   - Standing order writes lock one active standing_orders row and no other existing row.
//
//   - Inbox triage (operator_inbox.go) locks one operator_inbox_state row and
//     then, when the item is a decision request and the action is snooze,
//     that decision_requests row. Only this writer takes inbox state rows, so
//     the pair cannot close a cycle with a decision writer that locks only its
//     request.
//
//   - PruneSync locks sync_state first, but takes no lock from levels 2-5.
//   - IntakeTask and EmitTicketSet insert each body as the document
//     task-body/<id>, named after a task created in the same transaction
//     under the level-2 lock: a new key that no other writer holds.
//   - processNextLinearOutbox and ProcessNextLinearDecisionOp lock their
//     queue row before the task row; only the processor locks an existing
//     queue row, so no task writer waits on it.
//   - decision_requests rows outside level 2: an INSERT takes a new key and
//     waits on no existing row (CreateDisposition, a new chat question or
//     relay ask); chat rows are written only by the chat writers, after their
//     chat_questions row, and no task or job writer touches them; a snooze
//     locks one row and nothing else.
//   - ProcessNextDecisionNotice locks one decision_deliveries row (SKIP
//     LOCKED) and holds it while it sends; it reads the request and its task
//     without a lock. Writers only insert new decision_deliveries keys
//     (openDecisionTx), and decision_notify_digest is written by the worker
//     alone (decision_notify.go). Version 48 takes EXCLUSIVE on the retired
//     decision_notify table for its backfill, so an older worker's claim in
//     flight finishes first; nothing writes it afterwards.
//   - AckLedger locks one ledger_cursors row and reads sync_state without a
//     lock; nothing else writes ledger_cursors (ledger.go).
//   - CreateStatusUpdate locks its task_projects row FOR NO KEY UPDATE
//     first, then only reads tasks and status_updates and inserts a new
//     status_updates row. The project metadata writers lock that row and
//     nothing else, and the KEY SHARE a task or milestone write takes on it
//     does not conflict with FOR NO KEY UPDATE (status_updates.go).
//   - SetChangeMergeFlow (the operator's merge-flow write) takes its own
//     advisory lock first, so two writes cannot each pass the cycle check
//     against the other's uncommitted dependency, then locks one changes row
//     (FOR NO KEY UPDATE) and nothing else; the dependency chain is read without
//     locks. The foreign key's KEY SHARE on the target row does not conflict
//     with an event writer's upsert, and the event writers never take the
//     merge-flow lock, so neither side waits on the other while it holds
//     what the other needs.
//   - RecordVerification (verifications.go) locks at most one changes row
//     (the stub insert, ON CONFLICT DO NOTHING, for a PR hk has not seen) and then one verifications
//     row (insert, or FOR UPDATE on a replay), and takes no task row. No
//     other writer locks a verifications row, and the event writers that
//     lock changes rows never wait on one, so it cannot close a cycle.
//   - ApplyChangeBatch (change_state.go, mergewake's PR-state writes) locks one
//     changes row FOR UPDATE (after an insert that never updates an existing
//     row) and then writes only that change's change_checks, change_threads,
//     change_findings and reviews rows; it takes no task row and no level-2
//     lock. Those child rows are written only by a holder of their changes
//     row (recordChangeTx's insertReviewTx runs under upsertChangeTx's row
//     lock), so two writers of one PR queue on the parent row and no writer
//     waits on a child row while holding something another needs. The
//     sections of a batch run in the fixed order state, checks, threads,
//     findings, gate. GetChangeState reads one snapshot and locks nothing.
//   - StartDeploy locks no task row. It inserts its deploys row (a new
//     attempt_id key), takes KEY SHARE on the services row and on existing
//     changes rows through the foreign keys (an event writer's upsert takes
//     FOR NO KEY UPDATE, which does not conflict), reads existing changes
//     without a lock and inserts only missing changes keys, in (repo,
//     pr_number) order. Two starts therefore wait on new keys in one order.
//     A revert event inserting the same two new keys in the other order is
//     the one cycle left; Postgres aborts one side (40P01) and a start is
//     safe to retry with the same attempt_id.
//   - Deploy-service puts take the deploy_services_prefixes advisory lock
//     before reading or writing registry rows, to serialize prefix claims.
//   - Other advisory locks (disposition origin, task comment insert, labels,
//     Linear board ownership, quota presence) are each the first lock of
//     their transaction, and no transaction requests one while it holds a
//     lock from levels 2-5.
