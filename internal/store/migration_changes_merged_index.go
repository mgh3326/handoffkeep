package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261124-changes-merged-index is the PostgreSQL side of the merged-change
// count that deploy lag reads (hk 6188): count(*) of changes with repo = $1,
// state = 'merged' and merged_at > $2 (deploy_lag.go). Its MySQL counterpart
// carries the same ID in migration_mysql_changes_merged_index.go. The index
// takes SHARE on changes for its scan, which blocks writes to changes until the
// build ends. Production is MySQL, so this pause is paid by the test and
// development databases only.
func init() {
	registerMigration(migration{ID: "20261124-changes-merged-index", Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, []string{
			`CREATE INDEX IF NOT EXISTS changes_repo_state_merged ON changes(repo, state, merged_at)`,
		})
	}})
}
