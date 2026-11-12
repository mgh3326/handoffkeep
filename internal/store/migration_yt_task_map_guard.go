package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261112-yt-task-map-guard tightens the issue_create apply invariant: a
// matching binding alone is insufficient; the task-to-issue map row must be
// committed too. This closes the old-resolver path that could apply an
// issue_create op while publishing only a comment map. Replacing the existing
// trigger function takes a catalog lock only; it takes no lock on application
// tables and changes no rows.
func init() {
	registerMigration(migration{ID: "20261112-yt-task-map-guard", After: []string{"20261111-yt-task-binding-origin", "20261109-yt-write-ops"}, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, ytTaskMapGuardStatements)
	}})
}

var ytTaskMapGuardStatements = []string{
	`CREATE OR REPLACE FUNCTION handoffkeep_yt_issue_apply_guard() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN
		IF NEW.kind = 'issue_create' AND NEW.state = 'applied' AND OLD.state <> 'applied'
			AND NEW.reason <> 'binding_conflict'
			AND (NOT EXISTS (SELECT 1 FROM yt_task_bindings b
				WHERE b.task_id::text = NEW.entity_id AND b.tenant = NEW.tenant AND b.issue_id = NEW.yt_result_id)
				OR NOT EXISTS (SELECT 1 FROM yt_entity_map m
					WHERE m.entity_kind = 'task' AND m.hk_key = NEW.entity_id AND m.yt_id = NEW.yt_result_id)) THEN
			RAISE EXCEPTION 'yt_write_ops: issue_create op % applied without its binding or task map', OLD.id;
		END IF;
		RETURN NEW;
	END; $$`,
}
