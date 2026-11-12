package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261113-yt-issue-intents adds yt_issue_intents (hk 3942, redone in hk
// 4318): a task whose issue hk must create on tenant, but whose project maps
// to no YouTrack project yet. Issue mode records it when it creates such a
// task, and when a task's unsent issue op is rejected project_changed for an
// unmapped project (it keeps that op's tenant). The write worker of that
// tenant prepares the issue_create op once the mapping exists
// (PrepareMappedYTIssues); a queued comment on the task waits for it
// (ytLaneBlockedSQL). The unreleased 20261112-yt-issue-intents of PR #335 had
// no tenant column; this ID replaces it, so no database that recorded the old
// ID keeps the old shape under the new name.
//
// Additive: a new table with no foreign key to tasks (lockorder.go: no lane
// holder touches a task row). CREATE TABLE takes no lock on an existing
// table, so the previous release keeps serving; it never reads this table.
func init() {
	registerMigration(migration{ID: "20261113-yt-issue-intents", Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, []string{
			`CREATE TABLE IF NOT EXISTS yt_issue_intents (
				task_id BIGINT PRIMARY KEY CHECK (task_id > 0),
				tenant TEXT NOT NULL CHECK (tenant <> ''),
				recorded_at TIMESTAMPTZ NOT NULL)`,
		})
	}})
}
