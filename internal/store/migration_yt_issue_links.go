package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261114-yt-issue-links (hk 4637, design/2026-11-13/yt-issue-links §5.1, slice
// S2) adds the four tables of the YouTrack issue-link projection. Nothing in
// this release reads or writes them: the issue_links kind (S3) and its
// consumer and sweep (S4) do, and the projection's mode stays off until S5.
//
//   - yt_link_plans: one append-only row per send attempt or recovery pass of
//     an issue_links op, holding the read, the diff it planned and the links
//     it left alone (I5).
//   - yt_link_owned: the links hk created, recorded in the plan transaction
//     before the POST that creates them (I6). hk removes a link only while an
//     unreleased row names it, and only from that row's writer task (I1, I2).
//     link_type is the logical type, not the instance's type id, so renaming a
//     type in YouTrack does not orphan ownership. One unreleased row per
//     canonical link: subtask (child issue, parent issue), duplicate
//     (duplicate issue, original issue), relates (writer issue, other issue).
//   - yt_link_unowned: links on a managed type that hk did not create,
//     recorded and left alone (I7).
//   - yt_links_sweep: the singleton generation counter of the catch-up sweep
//     (I13). No row means generation 0, done 0: nothing to catch up before
//     the consumer's cursor first exists. The writer that bumps it (S4)
//     creates the row, so the table keeps the one clock that writer uses.
//
// No foreign keys: a key to yt_write_ops or tasks would take SHARE ROW
// EXCLUSIVE on that table during this migration and KEY SHARE on its rows at
// run time, and both are append-only for these purposes (an op row is never
// deleted, a task row is never deleted). Expand-only: CREATE TABLE, CREATE
// INDEX and CREATE TRIGGER touch only the new, empty tables, and the migration
// writes no row. The previous release names none of them.
func init() {
	registerMigration(migration{ID: YTIssueLinksMigration, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, ytIssueLinksStatements)
	}})
}

// YTIssueLinksMigration is the migration's registered ID.
const YTIssueLinksMigration = "20261114-yt-issue-links"

var ytIssueLinksStatements = []string{
	`CREATE TABLE IF NOT EXISTS yt_link_plans (
		id BIGSERIAL PRIMARY KEY,
		op_id BIGINT NOT NULL CHECK(op_id > 0),
		attempt INT NOT NULL CHECK(attempt >= 0),
		recover INT NOT NULL DEFAULT 0 CHECK(recover >= 0),
		plan JSONB NOT NULL,
		created_at TIMESTAMPTZ NOT NULL,
		UNIQUE(op_id, attempt, recover))`,
	// Append-only, as yt_binding_history.
	`CREATE OR REPLACE FUNCTION handoffkeep_yt_link_plans_guard() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN RAISE EXCEPTION 'yt_link_plans is append-only'; END; $$`,
	`DROP TRIGGER IF EXISTS yt_link_plans_append_only ON yt_link_plans`,
	`CREATE TRIGGER yt_link_plans_append_only BEFORE UPDATE OR DELETE OR TRUNCATE ON yt_link_plans
		FOR EACH STATEMENT EXECUTE FUNCTION handoffkeep_yt_link_plans_guard()`,

	`CREATE TABLE IF NOT EXISTS yt_link_owned (
		id BIGSERIAL PRIMARY KEY,
		tenant TEXT NOT NULL CHECK(tenant <> '' AND length(tenant) <= 255),
		link_type TEXT NOT NULL CHECK(link_type IN ('subtask','relates','duplicate')),
		from_issue TEXT NOT NULL CHECK(from_issue <> '' AND length(from_issue) <= 64),
		to_issue TEXT NOT NULL CHECK(to_issue <> '' AND length(to_issue) <= 64),
		writer_task_id BIGINT NOT NULL CHECK(writer_task_id > 0),
		target_task_id BIGINT NOT NULL CHECK(target_task_id > 0),
		kind TEXT NOT NULL,
		intended_op_id BIGINT NOT NULL CHECK(intended_op_id > 0),
		intended_at TIMESTAMPTZ NOT NULL,
		released_op_id BIGINT CHECK(released_op_id > 0),
		released_at TIMESTAMPTZ,
		CHECK(from_issue <> to_issue AND writer_task_id <> target_task_id),
		CHECK((kind = 'parent' AND link_type = 'subtask') OR (kind = 'related' AND link_type = 'relates')
			OR (kind = 'duplicate_of' AND link_type = 'duplicate')),
		CHECK(released_at IS NULL OR released_op_id IS NOT NULL))`,
	`CREATE UNIQUE INDEX IF NOT EXISTS yt_link_owned_live ON yt_link_owned(tenant, link_type, from_issue, to_issue)
		WHERE released_op_id IS NULL`,
	// The plan reads the unreleased rows touching one issue, on either side.
	`CREATE INDEX IF NOT EXISTS yt_link_owned_from ON yt_link_owned(tenant, from_issue) WHERE released_op_id IS NULL`,
	`CREATE INDEX IF NOT EXISTS yt_link_owned_to ON yt_link_owned(tenant, to_issue) WHERE released_op_id IS NULL`,
	// Released rows a later read has not confirmed absent yet.
	`CREATE INDEX IF NOT EXISTS yt_link_owned_releasing ON yt_link_owned(released_op_id)
		WHERE released_op_id IS NOT NULL AND released_at IS NULL`,

	`CREATE TABLE IF NOT EXISTS yt_link_unowned (
		id BIGSERIAL PRIMARY KEY,
		tenant TEXT NOT NULL CHECK(tenant <> '' AND length(tenant) <= 255),
		issue_id TEXT NOT NULL CHECK(issue_id <> '' AND length(issue_id) <= 64),
		link_type TEXT NOT NULL CHECK(link_type IN ('subtask','relates','duplicate')),
		side TEXT NOT NULL CHECK(side <> '' AND length(side) <= 32),
		other_issue TEXT NOT NULL CHECK(other_issue <> '' AND length(other_issue) <= 64),
		first_seen_op_id BIGINT NOT NULL CHECK(first_seen_op_id > 0),
		last_seen_op_id BIGINT NOT NULL CHECK(last_seen_op_id >= first_seen_op_id),
		first_seen_at TIMESTAMPTZ NOT NULL,
		last_seen_at TIMESTAMPTZ NOT NULL,
		gone_at TIMESTAMPTZ)`,
	// One open row per link as seen from one issue: the plan upserts on it.
	`CREATE UNIQUE INDEX IF NOT EXISTS yt_link_unowned_open ON yt_link_unowned(tenant, issue_id, link_type, side, other_issue)
		WHERE gone_at IS NULL`,

	`CREATE TABLE IF NOT EXISTS yt_links_sweep (
		singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK(singleton),
		generation BIGINT NOT NULL DEFAULT 0 CHECK(generation >= 0),
		done_generation BIGINT NOT NULL DEFAULT 0 CHECK(done_generation >= 0 AND done_generation <= generation),
		after_task_id BIGINT NOT NULL DEFAULT 0 CHECK(after_task_id >= 0),
		updated_at TIMESTAMPTZ NOT NULL)`,
}
