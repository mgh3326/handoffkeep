package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261111-yt-task-binding-origin adds what the issue_create kind (hk 3898)
// needs to publish a task's binding in the transaction that records its op
// applied:
//
//   - yt_task_bindings.origin: how the binding was published. Every row
//     before this release came from `youtrack comments-backfill`'s
//     verification, so the constant default 'import_verified' is right for
//     them; an applied issue_create op writes 'op_applied'.
//   - yt_task_bindings.op_id: the issue_create op that published an
//     op_applied binding (NULL otherwise). The op's admission gate reads it.
//   - yt_write_ops_binding_conflict: the applied ops whose binding was not
//     published (reason binding_conflict), which the alert worker scans every
//     pass; without it that scan reads every applied op.
//   - yt_issue_apply_guard: a trigger that refuses to mark an issue_create op
//     applied without its binding (or a recorded binding_conflict), so the
//     previous release's resolver, which knows no such kind, cannot close one.
//
// After both tables' migrations: the statements alter, index and guard them.
//
// Expand-only: two new nullable or defaulted columns, one partial index and
// one trigger that touches only issue_create ops, which no previous release
// writes. The previous release does not name the columns (its inserts get the
// defaults, its reads list their columns). Locks: ADD COLUMN takes ACCESS
// EXCLUSIVE on yt_task_bindings briefly (a constant default rewrites no row;
// the CHECK scans the few rows it holds); CREATE INDEX takes SHARE on
// yt_write_ops for the build, holding back op writes on a table of a few
// thousand rows for that long; CREATE TRIGGER takes SHARE ROW EXCLUSIVE on it
// briefly. The migration writes no row.
func init() {
	registerMigration(migration{ID: YTTaskBindingOriginMigration, After: []string{YTTaskBindingsMigration, "20261109-yt-write-ops"}, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, ytTaskBindingOriginStatements)
	}})
}

// YTTaskBindingOriginMigration is the migration's registered ID. The
// read-only youtrack commands refuse a schema that does not record it
// (Store.MigrationApplied): their binding reads select origin and op_id.
const YTTaskBindingOriginMigration = "20261111-yt-task-binding-origin"

var ytTaskBindingOriginStatements = []string{
	`ALTER TABLE yt_task_bindings ADD COLUMN IF NOT EXISTS origin TEXT NOT NULL DEFAULT 'import_verified'
		CHECK(origin IN ('import_verified','op_applied'))`,
	`ALTER TABLE yt_task_bindings ADD COLUMN IF NOT EXISTS op_id BIGINT NULL`,
	`CREATE INDEX IF NOT EXISTS yt_write_ops_binding_conflict ON yt_write_ops(id) WHERE reason = 'binding_conflict'`,
	// An issue_create op is applied only together with its task's binding
	// to the result issue, or with the recorded binding_conflict. This
	// release's Apply does both in one transaction; a resolver that does not
	// know the kind (the previous release during blue-green or after a
	// rollback) is refused here and the op stays remote_unknown.
	`CREATE OR REPLACE FUNCTION handoffkeep_yt_issue_apply_guard() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN
		IF NEW.kind = 'issue_create' AND NEW.state = 'applied' AND OLD.state <> 'applied'
			AND NEW.reason <> 'binding_conflict'
			AND NOT EXISTS (SELECT 1 FROM yt_task_bindings b
				WHERE b.task_id::text = NEW.entity_id AND b.tenant = NEW.tenant AND b.issue_id = NEW.yt_result_id) THEN
			RAISE EXCEPTION 'yt_write_ops: issue_create op % applied without its binding', OLD.id;
		END IF;
		RETURN NEW;
	END; $$`,
	`DROP TRIGGER IF EXISTS yt_issue_apply_guard ON yt_write_ops`,
	`CREATE TRIGGER yt_issue_apply_guard BEFORE UPDATE ON yt_write_ops FOR EACH ROW EXECUTE FUNCTION handoffkeep_yt_issue_apply_guard()`,
}
