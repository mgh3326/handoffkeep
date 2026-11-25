package store

// Lock order.
//
// Every transaction in this package takes its locks in the order below,
// skipping the levels it does not need. Two transactions that both follow it
// cannot wait on each other in a cycle (SQLSTATE 40P01). The exceptions are
// listed at the end, each with the reason it cannot close a cycle. The
// lock_order_test.go tests drive the cycles this order removed.
//
//  1. Schema locks. DDL runs only in Store.migrate and its
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
//     when it locks a task row and also the jobs row from level 3. Holders run
//     one at a time, so the order in which a holder locks task rows (a child
//     before its parent in autoCloseTx and applySourceEventTx, ascending id
//     in applyDeployRecordTx and propagateMergeTx,
//     task_id then each of tasks in the order sent in ApplyTaskSourceEvent,
//     then ascending id in propagateRevertTx) cannot form a
//     cycle with another holder. A relation write (addRelationTx,
//     RemoveTaskRelation, archiveLiveRelationsTx; hk 4637) locks the edge's
//     src row first and then updates both endpoints' rows in one statement
//     (bumpRelationEndpointsTx), so it locks the second endpoint after src;
//     every caller (AddTaskRelation, RemoveTaskRelation, TransitionTaskWith,
//     CreateTaskChecked, EmitTicketSet) holds this lock first for that
//     reason (TestLockOrderRelationEndpointsVersusRelationWrite). UpdateJob, AppendRelayEvent (an event that
//     answers or ends a decision: relayTouchesDecisions) and PutDocument
//     (deploy records) take it first for that reason. Taking it again in the
//     same transaction (applyJobRoleTx, applyDeployRecordTx) is a no-op:
//     advisory locks are re-entrant.
//
//  3. Job locks: the jobs row (UpdateJob).
//
//  4. Task rows: SELECT ... FOR UPDATE, or an UPDATE of tasks. Without the
//     level-2 lock a transaction locks at most one task row and no job lock
//     (the single-task edits: area, milestone, placement, work-board
//     reconcile).
//
//  5. Rows that belong to a locked task: task_events (its foreign key takes
//     KEY SHARE on the task row), task_relations, task_origins,
//     linear_board_tasks, linear_outbox,
//     sync_decisions, ticket_sets, task_orders (CreateTaskOrder, AckTaskOrder and
//     WithdrawTaskOrder lock the level-2 lock, the task row, then the one order
//     row; an ack also takes taskCommentInsertLock after level 2, as
//     CloseTaskMerged does), decision_answer_attempts and
//     decision_resolutions (inserted by ResolveDecision after its task row,
//     under the level-2 lock; append-only and never locked by a reader), and
//     task_spawn_records and task_spawn_notices (PutTaskSpawn: one task row
//     under its key share lock; a terminal write takes the comment insert
//     lock before that row, as ReplyDots does, and its
//     comment is a savepoint so a YouTrack refusal skips only the comment),
//     the work record: changes, change_tasks, change_gate_events and reviews. ApplyTaskSourceEvent
//     writes the work record under the level-2 lock after its task row
//     (recordChangeTx); a revert locks two changes rows, the reverted PR's
//     first. Shared rows read FOR SHARE (labels).
//     A documents insert or update that sets task_id takes KEY SHARE on that
//     one task row through the foreign key (PutDocument). That is the level-4
//     single-task case and needs no level-2 lock: no task writer locks a
//     documents row while it holds a task row (IntakeTask inserts new keys;
//     applyDeployRecordTx runs under PutDocument's own level-2 lock, after the
//     document row). AddYTArticleBinding takes the document's YouTrack lane
//     lock first (an advisory lock the feed and an article op's Apply take
//     before any row, and that no holder of a documents row waits for), then
//     the same order with FOR SHARE: the documents row, then its task row, then that project's
//     youtrack_projects row (which no writer holds while it locks a task or
//     a document), then inserts its new yt_article_bindings row. The dots writer (RecordDotsResult) is the same case: it
//     inserts or updates one documents row and one dots_results row, each
//     taking KEY SHARE on the one linked task row; SetDotsAdoption locks one
//     dots_results row and no task. ReplyDots reads and writes dots_run_tokens
//     rows only after it locks the request's task row (FOR UPDATE), so the
//     active run token cannot change under a reply's check. AnswerDotsQuestion
//     closes one dots_results row under that same level-2 lock and task row;
//     it waits on at most one SetDotsAdoption row, which never waits on a task.
//     decision_requests rows that name a
//     task or a job are updated only under the level-2 lock, several at a
//     time in id order (decision_records.go); the table has no foreign key.
//     SettleMerged takes the level-2 lock first, then the merged task rows it
//     settles in ascending id, then their rows through applySourceEventTx. (The
//     attempt writers StartDeploy and FinishDeploy were removed in hk 5913.)
//
//  6. At commit: the deferred sync allocator (handoffkeep_sync_allocate)
//     takes pg_advisory_xact_lock(824180067) and then the sync_state row,
//     after all other work of its transaction.
//
// Exceptions, none of which can close a cycle:
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
//     A revert event inserting the same two new keys in the other order is
//     the one cycle left; Postgres aborts one side (40P01) and a start is
//     safe to retry with the same attempt_id.
//   - Deploy-service puts take the deploy_services_prefixes advisory lock
//     before reading or writing registry rows, to serialize prefix claims.
//   - Other advisory locks (disposition origin, task comment insert, labels,
//     quota presence) are each the first lock of
//     their transaction, and no transaction requests one while it holds a
//     lock from levels 2-5, with one exception: ReplyDots takes level 2, then
//     taskCommentInsertLock, then the task row.
//     CreateTaskComment takes the comment lock first and reaches the task row
//     only through the foreign key's KEY SHARE, so it cannot wait on level 2
//     and the pair cannot cycle.
//   - The family lane (yt_family_roots.go, hk 4797): LockFamily takes a
//     session advisory lock,
//     pg_try_advisory_lock(hashtextextended('yt_family_root:<tenant>:<project>:<family>', 0)),
//     on a connection of its own and releases it before the caller returns.
//     It is never taken inside a transaction, and never while the caller holds
//     a transaction's lock or a row: the article_create send holds it across
//     its remote calls only. A process that dies drops its connection and with
//     it the lock. RecordFamilyRoot and FamilyRoot are single statements over
//     yt_family_roots and take no other lock. MySQL has no advisory locks:
//     the lane is GET_LOCK on the same held connection under the name
//     mysqlSessionLockName("yt_family_root", key) (hk: prefix, 64 bytes) and
//     RELEASE_LOCK releases it; the order is unchanged.
//   - The family create lock (ytArticleFamilyGateTx, hk 5265, hk 5761) is a
//     transaction-scoped entity lock, lockYTEntity(yt_article_family,
//     <tenant>\x1f<project>\x1f<family>), distinct from the family lane above.
//     Every transaction that may insert a root-capable article_create takes it
//     after all its other locks but the insert's own: the backfill
//     (PrepareYTArticleBackfill) after AddYTArticleBinding's locks (the
//     document's lane lock, the documents authority row FOR SHARE, the
//     document, task and youtrack_projects rows FOR SHARE); the catch-up
//     (catchUpYTArticleOnce) after its one lane lock; a feed pass of
//     document_articles (ytDocsHandler) after the feed lock and every lane
//     lock of the pass, which it takes before deciding any document; the
//     read-only requeue (requeueYTReadOnlyOpOnce) of a root-capable create
//     after the op's lane lock and its authority row FOR SHARE, and before
//     FOR UPDATE on that op's row (a row only a holder of the same lane
//     locks). Under it
//     the holder reads yt_family_roots and yt_write_ops without a lock and
//     inserts its op (prepareYTOpTx: the lane lock it already holds, then the
//     authority row FOR SHARE, which a switch holds while waiting for no lane
//     or family lock). A backfill or catch-up holds one family lock and waits
//     for nothing after it but that insert; only a feed pass, one at a time,
//     holds several. A holder of a family lock therefore never waits for a
//     lane lock, and no cycle runs through it, on PostgreSQL or on MySQL
//     (an hk_locks row there, as every entity lock). The backfill's second
//     call in prepareYTArticleTx re-takes the lock it holds.
//
//   - The YouTrack write op log (yt_write_ops.go, hk 3697). Prepare and Claim
//     take one advisory lock, the entity lane lock
//     pg_advisory_xact_lock(hashtextextended('yt_write_entity:<kind>:<id>', 0)),
//     as the first lock of their transaction, then rows of yt_write_ops (and
//     yt_entity_map in Apply). Since slice b1a (hk 3739, yt_comments.go) the
//     comment writers are the one exception to "first lock": CreateTaskComment
//     and CreateMCPTaskComment take taskCommentInsertLock, insert the comment
//     (KEY SHARE on its task row; MCP also writes its audit row; a dots-authored
//     MCP comment also reads its task and project with plain selects and
//     inserts its dots_note notification_outbox row, hk 6307, taking no lock
//     beyond the insert's own row), and only
//     then take the lane lock of that task, read without a lock (mode on, hk
//     3810: the task's yt_task_bindings row, its task_origins, yt_entity_map,
//     yt_comment_canary and youtrack_projects rows) and insert new rows of
//     yt_write_ops, yt_comment_write_links and yt_entity_map. The comment
//     worker's catch-up (CatchUpYTComments, hk 3897) makes the same reads and
//     inserts for a committed comment, in its own transaction and with no
//     comment lock: the comments authority row FOR SHARE first (hk 4489), then
//     the lane lock. A pass first tries (never waits for) the legacy sync lock
//     8241873404, which since the sync's removal (hk 4426) only an authority
//     switch also takes, so it cannot wait in a cycle with a switch.
//     RemoveYTCommentCanary
//     takes the task's lane lock first and then deletes its yt_comment_canary
//     row. The two writers that give an issue to a task, AddYTCommentCanary
//     and AddYTTaskBinding (yt_task_bindings.go, hk 3810), take the issue
//     ownership lock pg_advisory_xact_lock(8241873810) first, then FOR UPDATE
//     on the task row, then the task's lane lock, then FOR SHARE on its
//     youtrack_projects and task_origins rows, then read the other mappings
//     without a lock and insert their own new row (a binding published after
//     an unbind also inserts one task row into sync_log, hk 4539, which takes
//     no lock before the level 6 allocator at commit). FOR UPDATE and not FOR
//     SHARE: a task with no task_origins row has no origin row to lock, and a
//     first origin insert (UpsertYouTrackMirror, AddTaskOrigin) reaches the
//     task row only through its foreign key's KEY SHARE, which FOR SHARE
//     allows and FOR UPDATE blocks; so no first mirror link commits between
//     an issue writer's checks and its commit. The comment worker's Claim
//     (YTTenantLedger) reads the task's canary row (mode canary) or binding
//     (mode on) under the same lane lock, which is what orders an enrollment,
//     a removal or a binding against a claim. Claim also reads, without a
//     lock, the binding and issue_create op of a comment op queued with no
//     target (hk 4263, claimYTCommentTargetTx) before it updates its own op
//     row; the enqueue that queued it (queueYTCommentTx) makes the same plain
//     reads under the lane lock it already holds.
//     The writer that takes an issue away, UnbindYTTaskBinding
//     (yt_binding_history.go, hk 4490), takes the same three locks in the same
//     order, then reads the comments authority row FOR SHARE (as prepareYTOpTx
//     does after its lane lock), rejects the task's prepared yt_write_ops rows
//     (ops of the task's lane, which no other holder of that lane lock can
//     change meanwhile), deletes the task's yt_task_bindings row, its yt_issue_intents
//     row and its yt_entity_map row when that names the binding's issue (hk 4536:
//     every writer of the map row holds the lane lock) and inserts a
//     yt_binding_history row. AddYTTaskBinding inserts its history row the same
//     way; nothing else locks a history row.
//     No cycle, lock by lock:
//       - The ownership lock: only those three writers take it, always first
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
//         most one lane lock, except a feed pass (below), which only one
//         transaction at a time can run.
//       - The writers of task_origins and youtrack_projects rows never hold
//         such a row while they wait for a task row, and never request a lane
//         lock or the ownership lock. UpsertYouTrackMirror and AddTaskOrigin
//         are one statement: a first insert takes KEY SHARE after its new
//         row, which the issue writer's FOR SHARE read cannot see and so never
//         waits for, and the conflict update keeps task_id and takes no task
//         lock. SetYouTrackProjectID is one UPDATE that keeps the key, and
//         PutYouTrackProject is one upsert that keeps it too.
//       - The level-4 task writers hold level-2 and level-3 locks, other task
//         rows and the level-5 rows of tasks they already hold; an issue
//         writer requests none of those. No task writer locks a
//         youtrack_projects row.
//     So the comment lock -> task row -> lane lock, ownership lock -> task
//     row -> lane lock and lane lock -> mapping row edges cannot close a
//     cycle.
//     ReplyDots takes level 2 and its task row FOR UPDATE, and enqueues only
//     while comments are canonical in YouTrack (hk 4122): then the authority
//     row FOR SHARE and the task's lane lock follow. No lane holder requests
//     a level-2 lock or a task row (AddYTTaskBinding takes the task row before
//     the lane lock), and a switch requests neither, so level 2 -> task row ->
//     lane lock cannot close a cycle.
//     Issue mode (hk 3898, yt_issues.go) adds two lane holders. A task
//     creator (CreateTask, intake, ticket sets, dots, dispositions) in mode
//     on, after its own locks and the task insert, reads the task's
//     youtrack_projects row without a lock and takes the lane lock of the
//     task it just inserted, which no other transaction can hold yet, then
//     inserts the op row. Apply of an issue_create op takes the task's lane
//     lock first (as Claim does), then FOR UPDATE on the op row, then inserts
//     the task's yt_entity_map row and its yt_task_bindings row (an issue_move
//     op takes the same lane lock and updates that binding's project). That insert
//     can wait for an uncommitted binding of AddYTTaskBinding, which holds
//     its own task's lane lock and never waits for an op row or a second
//     lane lock, so the wait cannot close a cycle. Apply takes no ownership
//     lock and no task row: a canary enrollment racing it is caught by the
//     comment enqueue's ytBindingDisagreement, which skips that task's
//     comments.
//     A task's issue op (ensureYTIssueTx, claimYTIssueTargetTx; hk 3909,
//     3942, 4318): the task creators above, and SetTaskProject after its
//     level-1 lock and FOR UPDATE on the task row (task row before lane
//     lock, as AddYTTaskBinding), take the task's lane lock, then read
//     yt_issue_intents, the ownership rows (the task's latest
//     yt_binding_history row among them, hk 4490) and youtrack_projects
//     without a lock, then insert its yt_write_ops row, its
//     yt_issue_comment_bounds row (reading task_comments without a lock;
//     hk 4301) and insert or delete its yt_issue_intents row (no foreign
//     key, nobody else locks them). RecordAdoption (hk 4570) takes FOR SHARE
//     on one op row, then inserts its yt_write_op_adoptions row; it takes no
//     lane lock and holds nothing else. PlanLinks and SettleLinks of an
//     issue_links op (yt_link_plans.go, hk 4638) take the op's lane lock
//     first, then FOR UPDATE on its op row, then write yt_link_owned,
//     yt_link_unowned, yt_link_plans and yt_link_steps rows, which no other
//     writer locks, and read task_relations and yt_task_bindings without a
//     lock; they take no task row. MarkLinkStep takes only FOR UPDATE on the
//     op row, then inserts one yt_link_steps row; LeaseLinkRecovery is one
//     UPDATE of the op row.
//     PlanSplit and SettleSplit of an article_split op (yt_article_split.go,
//     hk 5085) take the op's lane lock first, then FOR UPDATE on its op row,
//     then write a yt_link_plans or yt_link_steps row and the op row;
//     MarkSplitStep takes only FOR UPDATE on the op row, then inserts one
//     yt_link_steps row and may set the op row's yt_result_id.
//     PrepareMappedYTIssues takes the lane lock as the first lock of each
//     task's transaction (as backfillYTIssue), then reads the task row
//     without a lock. Claim of an issue_create op, under the lane lock and
//     after FOR UPDATE on its op row, reads the task row and
//     youtrack_projects without a lock. None of them holds a second lane
//     lock or locks a task row after a lane lock.
//
//   - The YouTrack change feed (yt_feed.go, hk 3997). A consumer's pass
//     takes pg_try_advisory_xact_lock(8241873997) as the first lock of its
//     transaction, only tried: a pass that does not get it reads nothing (on
//     MySQL the same id's hk_locks row FOR UPDATE NOWAIT, seeded by
//     20261117-mysql-locks). It
//     then inserts or locks FOR UPDATE its one yt_feed_cursors row, reads
//     sync_state and sync_log without a lock, and runs the consumer's handler,
//     which reads model rows without a lock and prepares ops: per entity the
//     lane lock, then that entity's yt_snapshot_state row
//     (prepareYTSnapshotTx), then yt_write_ops rows. It may take several lane
//     locks, in the order of its rows; since the feed lock admits one pass at
//     a time, every other lane holder holds one lane lock and never waits for
//     a second one, so the lanes cannot close a cycle. Nothing but a pass
//     locks a cursor or snapshot-state row, and a handler takes no task row,
//     no level 2-5 lock and no comment lock. The pruned alert inserts a new
//     notification_outbox key (ON CONFLICT DO NOTHING).
//     The op alert pass (EnqueueYTWriteOpAlerts, hk 4056) takes the advisory
//     lock hashtextextended('yt_write_op_alerts', 0) first (an hk_locks row
//     on MySQL, ytAlertPassLock), then FOR SHARE on
//     the yt_write_ops rows it reads, then inserts notification_outbox and
//     yt_write_op_alerts rows (ON CONFLICT DO NOTHING); it takes no lane lock.
//     The issue_fields backfill (BackfillYTFieldsOps, hk 4022) runs one
//     task per transaction: that task's lane lock first, then what a handler
//     does for it (its snapshot-state row, then yt_write_ops rows). It holds
//     one lane lock, like every lane holder but a pass, and takes no feed
//     lock, cursor, task row or level 2-5 lock.
//     PutTranslations (translations.go) inserts sync_log rows for the task
//     titles it writes after its translations rows and locks no task row; its
//     commit takes level 6 like any feed writer. Every decision_requests
//     write inserts its sync_log row from a row trigger (hk 4054,
//     migration_decision_record_feed.go) and so takes level 6 at commit too.
//     The issue-link projection (yt_links.go, yt_links_sweep.go, hk 4639) adds
//     three lane holders and no new lock order. The task_links consumer is a
//     feed handler like task_fields: it takes the lane lock of each changed
//     task in ascending id order, then that task's yt_snapshot_state row and
//     the yt_write_ops rows. Its cursor creation, and an off-mode pass that
//     read a row, upsert the one yt_links_sweep row (bumpYTLinksSweepTx)
//     after the feed lock and the cursor row and before any lane lock of the
//     pass. A sweep slice (RunYTLinksSweep) runs each task in its own
//     transaction, one lane lock and what a handler takes after it, and
//     updates the yt_links_sweep row in a separate transaction that holds no
//     lane lock, so the row is never held across a lane wait. A binding's
//     links hook (ytLinksOnBindTx, in AddYTTaskBinding and the issue_create
//     bind) takes no lane lock beyond the bound task's own, which the
//     publication holds already: the writers that point at the new target
//     (its children, the duplicates of it, the lower id of each related edge)
//     are not locked there, because only a feed pass may hold more than one
//     lane lock. It inserts one task 'U' sync_log row for each of them
//     (level 6 at commit), and the consumer prepares their snapshots under
//     its own lane locks in a later pass. UnbindYTTaskBinding does the same
//     for the writers that point at the unbound task (ytLinksOnUnbindTx,
//     hk 4711) and does not update their yt_write_ops rows: a feed pass may
//     hold a writer's lane lock and those rows while it waits for the unbound
//     task's lane lock. Their stale ops (ytLinksStaleSQL) are rejected under
//     the writer's own lane lock instead, by the consumer before it prepares
//     the next snapshot and by Claim. Claim takes FOR KEY SHARE on the target
//     tasks' yt_task_bindings rows in task id order, after its lane lock and
//     op row, and holds them to commit; it may insert the writer's task 'U'
//     sync_log row (level 6 at commit). The unbind's DELETE of its one binding
//     row waits for such a claim, and the unbind then reads the other lanes'
//     in-flight links ops without a lock (ytLinksInFlightToTx); it waits for
//     no lane lock or op row a claim holds, and a claim waits only on binding
//     rows, so no cycle. SetYTAuthority reads yt_links_sweep without a lock,
//     after the authority rows.
//
//   - Decision mode (yt_decisions.go, hk 4054). Its lanes are decisions
//     (yt_write_entity:decision:<id>), which no task writer or comment
//     writer takes. The feed handler (decision_issues) takes them as above,
//     each before it reads that decision's binding and create op.
//     Apply of a decision_create op takes the decision's lane lock first,
//     then FOR UPDATE on the op row, then inserts the decision's
//     yt_entity_map and yt_decision_bindings rows and may prepare its first
//     snapshot (the same lane lock again, its yt_snapshot_state row, a new
//     op row). BindYTDecision (`youtrack decisions-backfill`) takes the
//     decision's lane lock first, reads its ops and binding without a lock,
//     inserts the binding and may prepare the snapshot the same way. Both
//     hold one lane lock; a binding insert can wait only for another
//     decision's uncommitted binding of the same issue, whose holder waits
//     for no second lane lock, so the wait cannot close a cycle. Neither
//     takes a task row, a decision_requests row or a level 1-5 lock.
//
//   - The authority rows (yt_authority.go, hk 4122). A switch to youtrack
//     (SetYTAuthority) first tries the legacy sync lock 8241873404 at
//     transaction scope, never waiting (a few tries, then it refuses); a
//     rollback to hk skips it. Then it locks yt_authority rows FOR UPDATE in
//     kind order, reads yt_write_ops, yt_feed_cursors and sync_log without a
//     lock and updates only its locked rows. Every op preparation
//     (prepareYTOpTx, after its lane lock), UnbindYTTaskBinding (the comments
//     row, after its lane lock) and the comment writers and the
//     audit enqueue (after the comment's task KEY SHARE, before the lane
//     lock; ReplyDots after its task row) read their kind's row FOR SHARE,
//     first inserting it ON CONFLICT DO NOTHING if it is missing
//     (ytAuthorityShareTx), which can wait only for a switch's own insert. A holder of FOR SHARE may wait
//     for a switch; a switch, once it holds the sync lock, waits only for
//     those rows, and a FOR SHARE holder never waits for the sync lock (sync,
//     adopt and the comment catch-up only try it, before any row lock), so
//     no cycle closes.
//
//   - Held-op recovery (RecoverHeldYTOps, yt_binding_recovery.go, hk 4491).
//     Its candidate scan is one statement and locks nothing. It then decides
//     each candidate in its own transaction: the task's lane lock first, then
//     the op row FOR UPDATE, then the kind's authority row FOR SHARE
//     (ytAuthorityShareTx, as op preparation does after its lane lock), then
//     plain reads of yt_task_bindings and the kind gates, then one UPDATE of
//     its op row. It holds one lane lock and takes no task row, level 1-5
//     lock or ownership lock. A switch holds only authority rows and waits
//     for nothing a recovery transaction holds (it reads yt_write_ops
//     without a lock), and the alert pass holds op rows FOR SHARE and waits
//     for no lane or authority row, so neither can close a cycle with it. A
//     switch that waits for the authority row counts the op as resolved once
//     the recovery commits, and a recovery that starts after a switch sees
//     youtrack and keeps the op.
//
//   - The moved-comment requeue (RequeueYTMovedComments,
//     yt_comment_requeue.go, hk 4569). Its candidate scan is one statement
//     and locks nothing. Each candidate is its own transaction, in the
//     catch-up's order: the comments authority row FOR SHARE, then the task's
//     lane lock, then plain reads of yt_comment_write_links, the rejected op,
//     yt_task_bindings, the binding's sources, the lane's unresolved
//     issue_move ops and the comment, then a new
//     yt_write_ops row (prepareYTOpTx, whose lane lock is re-entrant and whose
//     authority FOR SHARE is already held) and an UPDATE of the comment's
//     yt_comment_write_links row. It takes no task row, no level 1-5 lock and
//     no sync lock, so it adds no edge the catch-up does not already have.
//
//   - The automatic binder (yt_binding_binder.go, yt_binding_publish.go,
//     hk 4492). Its candidate scan takes the yt_binding_cursor row FOR UPDATE,
//     reads task_comments, yt_task_bindings and yt_issue_intents without a
//     lock, and inserts new yt_binding_intents rows. Its publication
//     (PublishYTAutoBinding) takes AddYTTaskBinding's locks in the same order,
//     then the comments authority row FOR SHARE as UnbindYTTaskBinding does,
//     and inside the lane lock it already holds prepares the comment ops of
//     the evidence it copied (yt_write_ops, yt_comment_write_links,
//     yt_entity_map) as ytCommentOnOpTx does. Settling an intent is one UPDATE
//     or DELETE of its own row, and YTBindingExcluded one plain read of
//     yt_binding_history. The ownership lock is held for the whole publication
//     and bounds the copied comments (YTAutoBindCopyMax); no holder takes a
//     task row, a lane lock, the ownership lock or a level 1-5 lock beyond
//     those AddYTTaskBinding already takes, and no other transaction locks a
//     cursor or intent row, so the binder adds no wait cycle.
//
//   - The YouTrack sync's single-run guard is a session-scoped
//     pg_try_advisory_lock(8241873404) on its own pooled connection. It is
//     taken before the run and released after it, and no other writer takes
//     that value, so it introduces no wait cycle.
//
//   - The bench grade mirror has its own table order: bench_grades before
//     bench_catalog. UpsertBenchGrades and UpsertBenchCatalog acquire both
//     ROW EXCLUSIVE locks in that order; the grade-scale migration takes
//     ACCESS EXCLUSIVE in the same order with a 5s lock_timeout. No other
//     transaction combines these tables with task or job locks.

// Serve's hub-forward and task-thread workers hold session locks 8241873510
// and 8241873512 respectively on separate pooled connections. They take the
// lock before running and never request each other's lock, so they add no cycle.
