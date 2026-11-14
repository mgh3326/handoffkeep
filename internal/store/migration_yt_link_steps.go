package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261115-yt-link-steps (hk 4638 S3, review of PR #425) adds yt_link_steps,
// the executor record of each issue_links plan: one row per step of a plan
// (ytwrite.ExecuteOrder) marked in flight before its request, and the
// outcome of the in-flight step written by the settlement in the same
// transaction as the release of the adds that were not made. Recovery reads
// it to tell an add that was never sent, refused, ambiguous or lost apart;
// the plan rows alone cannot (yt_link_plans is append-only, so the record
// is its own table). Nothing writes it until the issue_links kind is
// admitted (S4).
//
// No foreign key, as for the other link tables: a key to yt_write_ops would
// lock its rows at run time. Expand-only: CREATE TABLE on a new, empty table
// takes no lock on any existing one, and the previous release names it
// nowhere.
func init() {
	registerMigration(migration{ID: YTLinkStepsMigration, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, ytLinkStepsStatements)
	}})
}

// YTLinkStepsMigration is the migration's registered ID.
const YTLinkStepsMigration = "20261115-yt-link-steps"

var ytLinkStepsStatements = []string{
	`CREATE TABLE IF NOT EXISTS yt_link_steps (
		op_id BIGINT NOT NULL CHECK(op_id > 0),
		attempt INT NOT NULL CHECK(attempt >= 0),
		recover INT NOT NULL CHECK(recover >= 0),
		step INT NOT NULL CHECK(step >= 0),
		marked_at TIMESTAMPTZ NOT NULL,
		outcome TEXT CHECK(outcome IN ('ok','definite','ambiguous')),
		PRIMARY KEY(op_id, attempt, recover, step))`,
}
