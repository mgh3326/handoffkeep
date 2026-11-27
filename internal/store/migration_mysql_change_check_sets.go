package store

// 20261127-change-check-sets on MySQL, the twin of the PostgreSQL migration of
// the same ID (hk 7056): one table keyed (change_id, head_sha) holding the
// observed_at of the newest complete check set for that head. It depends on
// changes, the foreign key target, so it runs after the changes migration.
// The table is new and empty; the foreign key takes a metadata lock on
// changes for the statement. Expand-only: no existing table changes.
func init() {
	registerMySQLMigration(mysqlMigration{
		id:    "20261127-change-check-sets",
		after: []string{"20261117-mysql-changes"},
		steps: []string{
			`CREATE TABLE change_check_sets (
				change_id BIGINT NOT NULL, head_sha VARCHAR(64) NOT NULL, observed_at DATETIME(6) NOT NULL,
				PRIMARY KEY(change_id,head_sha),
				CONSTRAINT change_check_sets_change_fk FOREIGN KEY(change_id) REFERENCES changes(id) ON DELETE RESTRICT
			) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`,
		},
	})
}
