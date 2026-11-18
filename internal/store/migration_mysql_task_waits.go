package store

// The hand-wait table on MySQL (hk 5128, track B-4). It mirrors
// migration_task_waits.go: the same columns, nullability and unique key. The
// PostgreSQL partial index task_waits_open becomes a functional unique key of
// the same name, whose parts are NULL once a wait is cleared; MySQL admits any
// number of NULL keys, which is the partial index's behaviour (see
// migration_mysql_tasks.go). The table is new, so no writer waits on it.
const mysqlTaskWaitsKeyOpen = `CREATE UNIQUE INDEX task_waits_open ON task_waits((CASE WHEN cleared_at IS NULL THEN task_id END),(CASE WHEN cleared_at IS NULL THEN on_kind END),(CASE WHEN cleared_at IS NULL THEN on_ref END))`

var mysqlTaskWaitsSteps = []string{
	`CREATE TABLE IF NOT EXISTS task_waits (id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, task_id BIGINT NOT NULL, on_kind VARCHAR(16) NOT NULL, on_ref VARCHAR(256) NOT NULL DEFAULT '', reason LONGTEXT NOT NULL, since DATETIME(6) NOT NULL, source VARCHAR(160) NOT NULL, cleared_at DATETIME(6), cleared_by LONGTEXT NOT NULL DEFAULT (''), clear_note LONGTEXT NOT NULL DEFAULT (''), ` +
		`CONSTRAINT task_waits_kind_check CHECK(on_kind IN ('operator','lane','task','pr','ci','external')), CONSTRAINT task_waits_reason_check CHECK(reason<>''), CONSTRAINT task_waits_source_check CHECK(source<>''), CONSTRAINT task_waits_ref_check CHECK((on_kind='operator')=(on_ref='')), ` +
		`CONSTRAINT task_waits_task_fk FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE RESTRICT)` + mysqlTableOptions,
	mysqlIfNoIndex("task_waits", "task_waits_open", mysqlTaskWaitsKeyOpen),
}

func init() {
	registerMySQLMigration(mysqlMigration{
		id:    "20261118-task-waits",
		after: []string{"20261118-mysql-tasks"},
		steps: mysqlTaskWaitsSteps,
	})
}
