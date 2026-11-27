package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261127-change-check-sets records, per change and head, the observed_at of
// the newest complete check set applied to that head (hk 7056). applyChecksTx
// reads it so an older write cannot insert a check the newer complete set left
// out: such a check has no row for the set to mark gone, so the insert would
// read as present. Rows the set did not name are already covered by its gone
// update; the mark covers the rows that do not exist yet.
//
// The table is new and empty, and no existing table changes, so the statement
// takes no lock on an existing table beyond the foreign key's SHARE ROW
// EXCLUSIVE on changes, held until this migration commits. Change writers wait
// for that commit only. The previous release neither reads nor writes the
// table, so it keeps serving unchanged. Its complete sets leave no mark, so
// during a blue-green overlap the guard covers only the sets this release
// applies. Expand-only: nothing is dropped, renamed or narrowed.
func init() {
	registerMigration(migration{ID: "20261127-change-check-sets", Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, []string{
			`CREATE TABLE change_check_sets (
				change_id BIGINT NOT NULL REFERENCES changes(id),
				head_sha TEXT NOT NULL CHECK(head_sha ~ '^[0-9a-f]{40}$'),
				observed_at TIMESTAMPTZ NOT NULL,
				PRIMARY KEY(change_id, head_sha))`,
		})
	}})
}
