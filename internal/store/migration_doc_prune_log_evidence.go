package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261125-doc-prune-log-evidence keeps a deleted document's job and task on
// its doc_prune_log row (hk 6289, review follow-up of hk 6280). The merge link
// (doc_retention_merge.go) reads a review job's task from the job's documents
// under the four mwr- families; once another leg has deleted one of them
// (the hk 5057 screens/* job-end leg, the review leg, or hk 3639
// PruneDocuments) the live rows alone would lose a witness, or hide a second
// task that made the job ambiguous. Every pruner now writes the document's
// job and task_id with its log row, and the merge link counts those rows as
// evidence beside the live documents. Rows logged before this migration keep
// NULL in both and are no evidence either way.
//
// job matches documents.job (TEXT) and task_id documents.task_id (BIGINT),
// both nullable and without a foreign key: the log outlives the task row's
// meaning and must never block a task change.
//
// doc_prune_log_evidence serves the merge link's grouped read: it is a
// range scan of the four mwr- key ranges, like the documents side reads
// documents_key_c, and only rows with a task are evidence, so the index is
// partial on task_id IS NOT NULL and stays as small as the evidence.
//
// Expand-only: two nullable columns with no default and one new index. The
// previous release names its doc_prune_log columns on INSERT, so its rows get
// NULL in both. Locks: ADD COLUMN takes ACCESS EXCLUSIVE on doc_prune_log for
// the catalog update only (no rewrite: nullable, no default); CREATE INDEX
// takes SHARE on doc_prune_log for the build. At migration time no row has a
// task_id, so the build scans the small log table and indexes nothing; the
// previous release's retention inserts wait only for that scan.
func init() {
	registerMigration(migration{ID: "20261125-doc-prune-log-evidence", After: []string{"20261118-doc-retention"}, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, []string{
			`ALTER TABLE doc_prune_log ADD COLUMN IF NOT EXISTS job TEXT, ADD COLUMN IF NOT EXISTS task_id BIGINT`,
			`CREATE INDEX IF NOT EXISTS doc_prune_log_evidence ON doc_prune_log (document_key COLLATE "C") WHERE task_id IS NOT NULL`,
		})
	}})
}
