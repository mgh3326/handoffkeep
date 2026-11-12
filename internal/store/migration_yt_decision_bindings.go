package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261112-yt-decision-bindings adds what the decision kinds (hk 4054,
// docs/youtrack-write-protocol.md "Kinds decision_create and
// decision_fields") need:
//
//   - yt_decision_bindings: each decision request's DEC issue (tenant and
//     stable internal id), the one target its decision_fields ops write to.
//     Two writers, as for tasks (hk 3833 §2.1): `youtrack decisions-backfill`
//     after verifying the imported DEC-<id> issue (origin import_verified),
//     and the transaction that applies a decision_create op (origin
//     op_applied, op_id that op). decision_id is decision_requests.id as
//     decimal text; there is no foreign key, as in yt_task_bindings.
//   - yt_decision_apply_guard: a trigger that refuses to mark a
//     decision_create op applied without its binding (or a recorded
//     binding_conflict), so the previous release's resolver, which knows no
//     such kind, cannot close one.
//
// After 20261109-yt-write-ops: the trigger is on yt_write_ops.
//
// Expand-only: one new table with its primary key and unique index, and one
// trigger that touches only decision_create ops, which no previous release
// writes. Locks: CREATE TABLE locks only the new, empty table; CREATE TRIGGER
// takes SHARE ROW EXCLUSIVE on yt_write_ops briefly. The migration writes no
// row.
func init() {
	registerMigration(migration{ID: YTDecisionBindingsMigration, After: []string{"20261109-yt-write-ops"}, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, ytDecisionBindingsStatements)
	}})
}

// YTDecisionBindingsMigration is the migration's registered ID. The
// decisions preflight, which never migrates, refuses a schema that does not
// record it.
const YTDecisionBindingsMigration = "20261112-yt-decision-bindings"

var ytDecisionBindingsStatements = []string{
	`CREATE TABLE IF NOT EXISTS yt_decision_bindings (
		decision_id TEXT PRIMARY KEY CHECK(decision_id ~ '^[1-9][0-9]{0,18}$'),
		tenant TEXT NOT NULL CHECK(tenant <> '' AND length(tenant) <= 255),
		issue_id TEXT NOT NULL CHECK(issue_id <> '' AND length(issue_id) <= 64),
		issue_readable TEXT NOT NULL DEFAULT '' CHECK(length(issue_readable) <= 64),
		project_id TEXT NOT NULL DEFAULT '' CHECK(length(project_id) <= 64),
		origin TEXT NOT NULL CHECK(origin IN ('import_verified','op_applied')),
		op_id BIGINT NULL,
		bound_by TEXT NOT NULL CHECK(bound_by <> '' AND length(bound_by) <= 200),
		bound_at TIMESTAMPTZ NOT NULL,
		UNIQUE(tenant, issue_id))`,
	// A decision_create op is applied only together with its decision's
	// binding to the result issue, or with the recorded binding_conflict.
	// This release's Apply does both in one transaction; a resolver that
	// does not know the kind is refused here and the op stays
	// remote_unknown.
	`CREATE OR REPLACE FUNCTION handoffkeep_yt_decision_apply_guard() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN
		IF NEW.kind = 'decision_create' AND NEW.state = 'applied' AND OLD.state <> 'applied'
			AND NEW.reason <> 'binding_conflict'
			AND NOT EXISTS (SELECT 1 FROM yt_decision_bindings b
				WHERE b.decision_id = NEW.entity_id AND b.tenant = NEW.tenant AND b.issue_id = NEW.yt_result_id) THEN
			RAISE EXCEPTION 'yt_write_ops: decision_create op % applied without its binding', OLD.id;
		END IF;
		RETURN NEW;
	END; $$`,
	`DROP TRIGGER IF EXISTS yt_decision_apply_guard ON yt_write_ops`,
	`CREATE TRIGGER yt_decision_apply_guard BEFORE UPDATE ON yt_write_ops FOR EACH ROW EXECUTE FUNCTION handoffkeep_yt_decision_apply_guard()`,
}
