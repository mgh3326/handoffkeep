package store

// 20261124-changes-merged-index is the MySQL side of the merged-change count
// behind deploy lag (hk 6188; PostgreSQL twin in migration_changes_merged_index.go):
//
//	SELECT COUNT(*) FROM changes WHERE repo=? AND state='merged' AND merged_at>?
//
// (deploy_lag_mysql.go, and the changes side of the lag join). The keys that
// exist on changes lead with repo but not state or merged_at: changes_repo_pr
// (repo, pr_number) reaches every change of the repo, which the production
// digest shows as ~409 rows examined per call. The new key is (repo, state,
// merged_at): two equalities and a range, so the count reads only the merged
// changes since the deploy.
//
// repo is VARCHAR(766) in utf8mb4, four bytes a character, so the full column
// with state and merged_at would be about 3,136 bytes, past InnoDB's 3,072-byte
// key limit. The key takes repo(191) instead (764 bytes). InnoDB rechecks the
// full repo on each candidate row, so the count is exact.
//
// Expand-only: one secondary index, no column, table or constraint change. The
// previous release's reads and writes are unchanged; its plans can only gain a
// key, and writes to changes maintain one more. Lock: ALGORITHM=INPLACE,
// LOCK=NONE, so InnoDB builds the key online and concurrent reads and writes
// continue; the statement takes a short metadata lock at its start and end.
// The explicit clauses make the build fail, not fall back to a locking copy,
// if the server cannot do it online. Each step is restartable: mysqlIfNoIndex
// runs it only while the index is absent.
var mysqlChangesMergedIndexSteps = []string{
	mysqlIfNoIndex("changes", "changes_repo_state_merged", `CREATE INDEX changes_repo_state_merged ON changes(repo(191), state, merged_at) ALGORITHM=INPLACE LOCK=NONE`),
}

func init() {
	registerMySQLMigration(mysqlMigration{id: "20261124-changes-merged-index", after: []string{"20261117-mysql-changes"}, steps: mysqlChangesMergedIndexSteps})
}
