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
//     Store.migrate returns before its first table lock when the schema
//     already records every version it writes (migrateCurrent), so opening
//     a store beside live writers takes no table lock. Each start used to
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
//     cycle with another holder. UpdateJob, AppendRelayEvent (work board on),
//     WrkAppend (completed or joined records, work board on) and PutDocument
//     (deploy records) take it first for that reason. Taking it again in the
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
//     task_subscribers, linear_board_tasks, linear_outbox, sync_decisions;
//     shared rows read FOR SHARE (labels).
//
//  6. At commit: the deferred sync allocator (handoffkeep_sync_allocate)
//     takes pg_advisory_xact_lock(824180067) and then the sync_state row,
//     after all other work of its transaction.
//
// Exceptions, none of which can close a cycle:
//
//   - PruneSync locks sync_state first, but takes no lock from levels 2-5.
//   - processNextLinearOutbox and ProcessNextLinearDecisionOp lock their
//     queue row before the task row; only the processor locks an existing
//     queue row, so no task writer waits on it.
//   - Other advisory locks (disposition origin, task comment insert, labels,
//     Linear board ownership, quota presence) are each the first lock of
//     their transaction, and no transaction requests one while it holds a
//     lock from levels 2-5.
