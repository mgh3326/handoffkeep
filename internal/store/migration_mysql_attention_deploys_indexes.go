package store

// 20261125-attention-deploys-indexes is the MySQL side of the console
// attention and deploys reads (hk 6397; the hk 5584 digests show each scanning
// the whole tasks table about every 86 s). Its PostgreSQL twin carries the
// same ID in migration_attention_deploys_indexes.go.
//
//   - tasks_awaiting_decision_id (awaiting_decision, id): the awaiting half of
//     the attention reads (attention.go, attention_mysql.go) filters
//     awaiting_decision = true, then JSON and triage predicates. The existing
//     tasks keys lead with lane or parent_lane, which these reads do not
//     filter on, so the half walked the table; the new key reads only the
//     awaiting rows. The stalled half needs no new key: it filters state IN
//     (...) and rides the existing tasks_state_id (20261124-mysql-read-indexes),
//     and the candidates' started half probes the primary key.
//   - task_events_kind_to_at_id (kind, `to`(32), at DESC, id DESC):
//     ListMergedTaskEvents (deploys.go) now drives from the merge transitions,
//     newest first, and stops after its limit. The per-task key
//     task_events_task_kind_to_at leads with task_id, which the outer read
//     does not filter on, so it walked task_events; the new key reads only
//     the newest merges in order. `to` is LONGTEXT, so the key takes a
//     32-character prefix; the equality is rechecked on the row.
//
// Expand-only: two secondary indexes, no column, table or constraint change.
// The previous release's reads and writes are unchanged; its plans can only
// gain a key, and writes to tasks and task_events maintain one more each.
// Lock: each build uses ALGORITHM=INPLACE, LOCK=NONE, so InnoDB builds the key
// online and concurrent reads and writes continue; each statement takes a
// short metadata lock at its start and end. The explicit clauses make the
// build fail, not fall back to a locking copy, if the server cannot do it
// online. tasks is small (about 6k rows), so its build is short. task_events
// grows with every transition and is append-only, so its build scan scales
// with the table; check its row count before deploy. Each step is
// restartable: mysqlIfNoIndex runs it only while the index is absent.
var mysqlAttentionDeploysIndexSteps = []string{
	mysqlIfNoIndex("tasks", "tasks_awaiting_decision_id", `CREATE INDEX tasks_awaiting_decision_id ON tasks(awaiting_decision, id) ALGORITHM=INPLACE LOCK=NONE`),
	mysqlIfNoIndex("task_events", "task_events_kind_to_at_id", "CREATE INDEX task_events_kind_to_at_id ON task_events(kind, `to`(32), at DESC, id DESC) ALGORITHM=INPLACE LOCK=NONE"),
}

func init() {
	registerMySQLMigration(mysqlMigration{id: "20261125-attention-deploys-indexes", after: []string{"20261118-mysql-tasks"}, steps: mysqlAttentionDeploysIndexSteps})
}
