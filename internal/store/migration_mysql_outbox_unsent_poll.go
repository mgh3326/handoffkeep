package store

// 20261125-outbox-unsent-poll mirrors the PostgreSQL migration of the same id
// (hk 6398): notification_outbox gains a secondary index serving the hub
// forwarder's two per-tick reads, the pre-cutoff settle (`created_at < ? AND
// sent_at IS NULL AND skipped_at IS NULL`) and the unsent poll (`sent_at IS
// NULL AND skipped_at IS NULL AND created_at >= ? ORDER BY id LIMIT ?`). The
// only secondary index leads with id (notification_outbox_unsent_live), so
// both statements walk the whole table: the production digests
// (findings/2026-11-25/hk5584-table-io) show 363 calls of each in 30 min and
// 644k row reads for a table of about 890 rows.
//
// notification_outbox_unsent_poll (sent_at, skipped_at, created_at, id) keeps
// the two NULL equalities as the leading equality prefix (MySQL has no partial
// index, so the NULLs are the key), ranges on created_at, and carries id for
// the poll's ORDER BY id LIMIT. InnoDB appends the primary key to the entry,
// so the poll's rows come out in id order and LIMIT stops the scan.
//
// Expand-only: one secondary index, no column, table or constraint change. The
// previous release's reads are unchanged and its plans can only gain a key,
// and outbox writes maintain one more key. Lock: InnoDB builds the secondary
// index online (ALGORITHM=INPLACE, LOCK=NONE), so reads and writes continue;
// the statement takes a short metadata lock at its start and end, and the
// build scans a table under 1 MB. The step is restartable: mysqlIfNoIndex runs
// it only while the index is absent.
var mysqlOutboxUnsentPollSteps = []string{
	mysqlIfNoIndex("notification_outbox", "notification_outbox_unsent_poll", `CREATE INDEX notification_outbox_unsent_poll ON notification_outbox(sent_at, skipped_at, created_at, id) ALGORITHM=INPLACE LOCK=NONE`),
}

func init() {
	registerMySQLMigration(mysqlMigration{id: "20261125-outbox-unsent-poll", after: []string{"20261118-mysql-relay-jobs"}, steps: mysqlOutboxUnsentPollSteps})
}
