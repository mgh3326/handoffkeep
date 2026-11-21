package store

// The open-op reads on MySQL (hk 5584, hk 5878): two secondary indexes on
// yt_write_ops that lead with state, so the reads of the ops still open
// (the dispatcher's stale and due reads, and the stuck and rejected alert
// scans) are a range of those ops instead of a walk of the table.
//
// Why these: the production digests (findings/2026-11-21/hk5584-mysql-digests)
// show the five statements that read open ops examining about 48,000 rows
// per call, the whole table of ~47,000. Each filters on state and orders by
// id or by next_attempt_at. The plain indexes that exist on MySQL (created_at,
// id), (dispatched_at, id) and (next_attempt_at, id) lead with a time, and
// resolved ops keep their timestamps (next_attempt_at and created_at are NOT
// NULL), so a "<= now" range covers the table. PostgreSQL serves the same
// reads with partial indexes (yt_write_ops_due, yt_write_ops_dispatched,
// yt_write_ops_alert_scan), so this migration is MySQL-only.
//
// (state, id) serves every read that orders by id, and (state, next_attempt_at,
// id) serves the due read's order. Both are plain keys: MySQL has no partial
// index, and the state leads, so a read of one state walks only that state's
// rows.
//
// Expand-only: two secondary indexes, no column, table or constraint change.
// The previous release's reads and writes are unchanged. Its plans can only
// gain an index, and writes maintain two more keys. Locks: InnoDB builds a
// secondary index online, so concurrent reads and writes continue during the
// build; the statement takes a short metadata lock at its start and end.
// yt_write_ops holds about 46,400 rows on production, so the build's scan is
// short. Each step is restartable: mysqlIfNoIndex runs it only while the index
// is absent.
var mysqlYTWriteOpScanSteps = []string{
	mysqlIfNoIndex("yt_write_ops", "yt_write_ops_state_id", `CREATE INDEX yt_write_ops_state_id ON yt_write_ops(state, id)`),
	mysqlIfNoIndex("yt_write_ops", "yt_write_ops_state_due", `CREATE INDEX yt_write_ops_state_due ON yt_write_ops(state, next_attempt_at, id)`),
}

func init() {
	registerMySQLMigration(mysqlMigration{id: YTWriteOpScanMigration, after: []string{"20261118-mysql-yt-write-ops"}, steps: mysqlYTWriteOpScanSteps})
}

// YTWriteOpScanMigration is the migration's registered ID.
const YTWriteOpScanMigration = "20261121-mysql-yt-write-op-scan"
