package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// YTIssueAdoptionMigration adds two records an issue_create op keeps beside
// its yt_write_ops row:
//
//   - yt_write_op_adoptions (hk 4570): the dispatch that adopted an existing
//     YouTrack thing instead of writing records so, with its attempt and the
//     adopted id, before the adoption is applied (YTWriteLedger.
//     RecordAdoption). Reconcile applies a lost adoption settlement only on
//     this record; without it the op stays adoption_unconfirmed for the
//     operator. A separate table, because the yt_write_ops guard lets a
//     dispatched row change only with its state.
//   - yt_issue_comment_bounds (hk 4301): the task's highest comment id when
//     its issue_create op was prepared. An op that adopts an issue starts
//     the binding's comments_from there, so comments written while the op
//     was pending, which nothing copied, are caught up (bindYTIssueTx).
//
// Additive: two new tables with no foreign key (a key to yt_write_ops would
// lock that table while the previous release serves). CREATE TABLE takes no
// lock on an existing table; the previous release never reads either table,
// and an op it prepared or dispatched has no row, which reads as before
// (adoption_unconfirmed; comments_from at the settlement).
const YTIssueAdoptionMigration = "20261114-yt-issue-adoption"

func init() {
	registerMigration(migration{ID: YTIssueAdoptionMigration, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, []string{
			`CREATE TABLE IF NOT EXISTS yt_write_op_adoptions (
				op_id BIGINT PRIMARY KEY CHECK (op_id > 0),
				attempts INT NOT NULL CHECK (attempts > 0),
				yt_id TEXT NOT NULL CHECK (yt_id <> ''),
				recorded_at TIMESTAMPTZ NOT NULL)`,
			`CREATE TABLE IF NOT EXISTS yt_issue_comment_bounds (
				op_id BIGINT PRIMARY KEY CHECK (op_id > 0),
				task_id BIGINT NOT NULL CHECK (task_id > 0),
				comments_from BIGINT NOT NULL CHECK (comments_from >= 0),
				recorded_at TIMESTAMPTZ NOT NULL)`,
		})
	}})
}
