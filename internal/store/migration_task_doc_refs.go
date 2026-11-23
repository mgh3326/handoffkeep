package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261124-task-doc-refs adds the task-to-document reference table (hk 6125).
// A reference lets a task cite a document it does not own; the binding
// (documents.task_id) is unchanged. No FOREIGN KEY: the store checks both keys
// in the writing transaction, which matches the other task_id columns that the
// schema document declares without one (hk 6130).
//
// Expand-only: one new table no previous release reads or writes. Locks:
// ACCESS EXCLUSIVE on task_doc_refs for the CREATE only; no existing table is
// touched.
func init() {
	registerMigration(migration{ID: "20261124-task-doc-refs", Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, []string{
			`CREATE TABLE IF NOT EXISTS task_doc_refs (
				task_id BIGINT NOT NULL,
				doc_key TEXT NOT NULL,
				created_at TIMESTAMPTZ NOT NULL,
				created_by TEXT NOT NULL,
				PRIMARY KEY (task_id, doc_key)
			)`,
		})
	}})
}
