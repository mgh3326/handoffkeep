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
//     in applyDeployRecordTx, propagateMergeTx and recordLinearBoardRelayTx) cannot form a
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
//     dots_results row and no task. ReplyDots reads and writes dots_run_tokens
//     rows only after it locks the request's task row (FOR UPDATE), so the
//     active run token cannot change under a reply's check. decision_requests rows that name a
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
//   - The task-thread poster (task_threads.go) locks its one
//     task_thread_cursors row, or one task_thread_deliveries row (SKIP
//     LOCKED), and reads task_events, tasks, changes, deploys and
//     decision_requests without a lock. It holds no row while it sends, and
//     nothing else writes either table.
//   - AckLedger locks one ledger_cursors row and reads sync_state without a
//     lock; nothing else writes ledger_cursors (ledger.go).
//   - CreateStatusUpdate locks its task_projects row FOR NO KEY UPDATE
//     first, then only reads tasks and status_updates and inserts a new
//     status_updates row. The project metadata writers lock that row and
//     nothing else, and the KEY SHARE a task or milestone write takes on it
//     does not conflict with FOR NO KEY UPDATE (status_updates.go).
//   - SetProjectDesignLane locks its one task_projects row FOR NO KEY UPDATE
//     and then inserts a project_owner_events row; no writer locks an owner
//     event row and no transaction that holds a task_projects lock waits on
//     one, so the pair cannot close a cycle (project_owner.go).
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
//     lock from levels 2-5, with two exceptions: ApplyYouTrackActivity and
//     ReplyDots take level 2, then taskCommentInsertLock, then the task row.
//     CreateTaskComment takes the comment lock first and reaches the task row
//     only through the foreign key's KEY SHARE, so it cannot wait on level 2
//     and the pair cannot cycle.
//
//   - The YouTrack write op log (yt_write_ops.go, hk 3697). Prepare and Claim
//     take one advisory lock, the entity lane lock
//     pg_advisory_xact_lock(hashtextextended('yt_write_entity:<kind>:<id>', 0)),
//     as the first lock of their transaction, then rows of yt_write_ops (and
//     yt_entity_map in Apply). Since slice b1a (hk 3739, yt_comments.go) the
//     comment writers are the one exception to "first lock": CreateTaskComment
//     and CreateMCPTaskComment take taskCommentInsertLock, insert the comment
//     (KEY SHARE on its task row; MCP also writes its audit row), and only
//     then take the lane lock of that task, read without a lock (mode on, hk
//     3810: the task's yt_task_bindings row, its task_origins, yt_entity_map,
//     yt_comment_canary and youtrack_projects rows) and insert new rows of
//     yt_write_ops, yt_comment_write_links and yt_entity_map. The comment
//     worker's catch-up (CatchUpYTComments, hk 3897) makes the same reads and
//     inserts for a committed comment, with the lane lock as the first lock
//     of its transaction and no comment lock. A pass first tries (never
//     waits for) the session-scoped `youtrack sync` lock, so it cannot wait
//     in a cycle with a sync run. RemoveYTCommentCanary
//     takes the task's lane lock first and then deletes its yt_comment_canary
//     row. The two writers that give an issue to a task, AddYTCommentCanary
//     and AddYTTaskBinding (yt_task_bindings.go, hk 3810), take the issue
//     ownership lock pg_advisory_xact_lock(8241873810) first, then FOR UPDATE
//     on the task row, then the task's lane lock, then FOR SHARE on its
//     youtrack_projects and task_origins rows, then read the other mappings
//     without a lock and insert their own new row. FOR UPDATE and not FOR
//     SHARE: a task with no task_origins row has no origin row to lock, and a
//     first origin insert (UpsertYouTrackMirror, AddTaskOrigin) reaches the
//     task row only through its foreign key's KEY SHARE, which FOR SHARE
//     allows and FOR UPDATE blocks; so no first mirror link commits between
//     an issue writer's checks and its commit. The comment worker's Claim
//     (YTTenantLedger) reads the task's canary row (mode canary) or binding
//     (mode on) under the same lane lock, which is what orders an enrollment,
//     a removal or a binding against a claim.
//     No cycle, lock by lock:
//       - The ownership lock: only those two writers take it, always first
//         and while holding nothing, so no transaction waits for it while
//         holding another lock.
//       - The task row: an issue writer waits for it holding only the
//         ownership lock, which nobody waits for while holding anything.
//         While an issue writer holds it, it waits only for the lane lock and
//         for FOR SHARE on the task's youtrack_projects and task_origins rows.
//         The transactions FOR UPDATE makes wait are the task writers (an
//         UPDATE or FOR UPDATE, as FOR SHARE did) and now also every insert
//         whose foreign key takes KEY SHARE on the task (task_comments,
//         task_events, task_origins, documents and the other level-5 rows).
//         None of them holds the lane lock or such a mapping row while it
//         waits for the task row: the cases follow.
//       - The comment writers (CreateTaskComment, CreateMCPTaskComment) take
//         the comment lock, then KEY SHARE on the task through the
//         task_comments foreign key, then the lane lock. Waiting for the task
//         row, one holds only the comment lock (and MCP its new audit row);
//         holding the lane lock, it already holds KEY SHARE, and the issue
//         writer's FOR UPDATE waits for that instead. Both orders, comment
//         lock -> task KEY SHARE -> lane lock and ownership lock -> task FOR
//         UPDATE -> lane lock, put the task row before the lane lock, so they
//         cannot cycle; the issue writers never request the comment lock.
//       - No other lane holder touches a task row: yt_write_ops,
//         yt_entity_map, yt_comment_canary, yt_comment_write_links and
//         yt_task_bindings have no foreign key to tasks, no holder of a lane
//         lock requests taskCommentInsertLock, the ownership lock, a task
//         row, a job lock or a level 1-4 lock, and a transaction holds at
//         most one lane lock.
//       - The writers of task_origins and youtrack_projects rows never hold
//         such a row while they wait for a task row, and never request a lane
//         lock or the ownership lock. UpsertYouTrackMirror and AddTaskOrigin
//         are one statement: a first insert takes KEY SHARE after its new
//         row, which the issue writer's FOR SHARE read cannot see and so never
//         waits for, and the conflict update keeps task_id and takes no task
//         lock. MarkYouTrackTaskSynced, UpdateYouTrackMirrorDisplay and
//         SetYouTrackProjectID are one UPDATE that keeps the key;
//         PutYouTrackProject then deletes only youtrack_attempts rows.
//         ApplyYouTrackActivity reaches task_origins only after FOR UPDATE on
//         its task row, or for a task it created in the same transaction.
//       - The level-4 task writers hold level-2 and level-3 locks, other task
//         rows and the level-5 rows of tasks they already hold; an issue
//         writer requests none of those. No task writer locks a
//         youtrack_projects row.
//     So the comment lock -> task row -> lane lock, ownership lock -> task
//     row -> lane lock and lane lock -> mapping row edges cannot close a
//     cycle. The comment writers that take level 2 (ApplyYouTrackActivity,
//     ReplyDots) never enqueue an op, so level 2 -> lane lock does not occur.
//     Issue mode (hk 3898, yt_issues.go) adds two lane holders. A task
//     creator (CreateTask, intake, ticket sets, dots, dispositions) in mode
//     on, after its own locks and the task insert, reads the task's
//     youtrack_projects row without a lock and takes the lane lock of the
//     task it just inserted, which no other transaction can hold yet, then
//     inserts the op row. Apply of an issue_create op takes the task's lane
//     lock first (as Claim does), then FOR UPDATE on the op row, then inserts
//     the task's yt_entity_map row and its yt_task_bindings row. That insert
//     can wait for an uncommitted binding of AddYTTaskBinding, which holds
//     its own task's lane lock and never waits for an op row or a second
//     lane lock, so the wait cannot close a cycle. Apply takes no ownership
//     lock and no task row: a canary enrollment racing it is caught by the
//     comment enqueue's ytBindingDisagreement, which skips that task's
//     comments.
//
//   - The YouTrack sync's single-run guard is a session-scoped
//     pg_try_advisory_lock(8241873404) on its own pooled connection. It is
//     taken before the run and released after it, and no other writer takes
//     that value, so it introduces no wait cycle.

// Serve's hub-forward and Linear-board workers hold session locks 8241873510
// and 8241873511 respectively on separate pooled connections. They take the
// lock before running and never request each other's lock, so they add no cycle.
