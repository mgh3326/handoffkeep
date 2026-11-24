package store

// 20261124-mysql-read-indexes serves three scan-heavy MySQL reads (hk 6188,
// hk 5584 digests). Each index is named for the predicate it serves; each
// read's rows examined per call is the digest figure.
//
//   - tasks_state_id (state, id): ListMergedTaskEvents filters
//     tasks.state = 'merged' (deploys.go). No key on tasks leads with state, so
//     the read walks the whole tasks table (the digest's "no index used"). The
//     key reads only the merged tasks.
//
//   - task_events_task_kind_to_at (task_id, kind, to, at, id): the same read
//     joins task_events on task_id, kind = 'transition' and to = 'merged', then
//     ranks each task's rows by at, id. task_events_task_at (task_id, at, id)
//     reads every event of a merged task and filters kind and to afterwards;
//     with the new key the join reads only that task's merged transitions.
//     to is LONGTEXT, so the key takes a 32-character prefix; the equality is
//     rechecked on the row. task_events is append-only, so this key is the one
//     write cost added to it.
//
//   - yt_write_ops_state_tenant_due (state, tenant, next_attempt_at, id): both
//     arms of the due read (ytDueMySQLSQL, yt_write_ops.go) filter state
//     and tenant = ?, then order by next_attempt_at, id under a LIMIT.
//     yt_write_ops_state_due (state, next_attempt_at, id) serves the order but
//     not the tenant equality, so each arm reads ops of other tenants before
//     it reaches its limit. tenant is VARCHAR(255) (1,020 bytes in utf8mb4), so
//     the key is about 1,100 bytes.
//
// Not added here: the attachment read in ListTaskAttachmentsByRefs (store.go).
// Its WHERE and JOIN are already served by attachment_refs_target
// (ref_kind, ref_id), which covers the refs side (sha256 is its primary key
// suffix). Its ranking needs every ref row of the listed tasks, and no key on
// attachments can cut that set, so an index would not lower the rows examined.
// The plan is left for EXPLAIN after deploy.
//
// Expand-only: three secondary indexes, no column, table or constraint change.
// The previous release's reads and writes are unchanged. Its plans can only
// gain a key, and writes to tasks, task_events and yt_write_ops maintain one
// more each. Lock: each build uses ALGORITHM=INPLACE, LOCK=NONE, so InnoDB
// builds the key online and concurrent reads and writes continue; each
// statement takes a short metadata lock at its start and end. tasks is small
// (about 6k rows) and yt_write_ops about 46,400 rows, so those two builds are
// short. task_events grows with every transition and is append-only, so its
// build scan scales with the table; check its row count before deploy. Each
// step is restartable: mysqlIfNoIndex runs it only while the index is absent.
var mysqlReadIndexSteps = []string{
	mysqlIfNoIndex("tasks", "tasks_state_id", `CREATE INDEX tasks_state_id ON tasks(state, id) ALGORITHM=INPLACE, LOCK=NONE`),
	mysqlIfNoIndex("task_events", "task_events_task_kind_to_at", "CREATE INDEX task_events_task_kind_to_at ON task_events(task_id, kind, `to`(32), at, id) ALGORITHM=INPLACE, LOCK=NONE"),
	mysqlIfNoIndex("yt_write_ops", "yt_write_ops_state_tenant_due", `CREATE INDEX yt_write_ops_state_tenant_due ON yt_write_ops(state, tenant, next_attempt_at, id) ALGORITHM=INPLACE, LOCK=NONE`),
}

func init() {
	registerMySQLMigration(mysqlMigration{id: "20261124-mysql-read-indexes", after: []string{"20261118-mysql-tasks", "20261118-mysql-yt-write-ops"}, steps: mysqlReadIndexSteps})
}
