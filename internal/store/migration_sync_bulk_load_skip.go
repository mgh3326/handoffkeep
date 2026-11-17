package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261118-sync-bulk-load-skip lets the copy tool load tasks without journaling
// them (hk 5074, design §11.6). A load that fills tasks into a target database
// must not write sync_log rows or rebuild sync_decisions from the rows it is
// copying: sync_decisions is copied itself, and the feed starts fresh on the
// target. Only the copy tool sets handoffkeep.bulk_load, transaction-locally
// with set_config(..., true). The setting is read with the missing-ok form, so
// an unset GUC is NULL and both functions behave exactly as before. Foreign
// keys stay enforced: this is a GUC check, not session_replication_role.
//
// Expand-only. CREATE OR REPLACE FUNCTION rewrites two catalog rows and locks
// no table; the triggers that call the functions are untouched, and a previous
// release's binary keeps working because the functions differ only when the
// GUC is on.
func init() {
	registerMigration(migration{ID: "20261118-sync-bulk-load-skip", Apply: applySyncBulkLoadSkip})
}

func applySyncBulkLoadSkip(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, []string{
		`CREATE OR REPLACE FUNCTION handoffkeep_sync_emit() RETURNS trigger LANGUAGE plpgsql AS $$
 DECLARE row_id TEXT;
 BEGIN
   IF current_setting('handoffkeep.bulk_load', true) = 'on' THEN RETURN NULL; END IF;
   IF TG_OP='DELETE' THEN row_id := OLD.id::text; ELSE row_id := NEW.id::text; END IF;
   INSERT INTO sync_log(model,model_id,action,at) VALUES(TG_ARGV[0],row_id,substr(TG_OP,1,1),clock_timestamp());
   RETURN NULL;
 END $$`,
		`CREATE OR REPLACE FUNCTION handoffkeep_sync_decision() RETURNS trigger LANGUAGE plpgsql AS $$
 DECLARE request_id TEXT;
 BEGIN
   IF current_setting('handoffkeep.bulk_load', true) = 'on' THEN RETURN NULL; END IF;
   IF TG_OP='DELETE' THEN DELETE FROM sync_decisions WHERE task_id=OLD.id; RETURN NULL; END IF;
   request_id := NEW.refs->'decision_request'->>'id';
   IF TG_OP='UPDATE' AND OLD.refs->'decision_request' IS NOT DISTINCT FROM NEW.refs->'decision_request' THEN RETURN NULL; END IF;
   DELETE FROM sync_decisions WHERE task_id=NEW.id AND id IS DISTINCT FROM request_id;
   IF COALESCE(request_id,'')<>'' THEN
     INSERT INTO sync_decisions(id,task_id,updated_at) VALUES(request_id,NEW.id,NEW.updated_at)
     ON CONFLICT(id) DO UPDATE SET task_id=EXCLUDED.task_id,updated_at=EXCLUDED.updated_at;
   END IF;
   RETURN NULL;
 END $$`,
	})
}

// The MySQL triggers carry the @hk_bulk_load guard from their creation
// (migration_mysql_tasks.go, migration_mysql_comments.go), so this ID has
// nothing to change there; the registry needs a same-named entry (DO 0 is a
// no-op statement).
func init() {
	registerMySQLMigration(mysqlMigration{id: "20261118-sync-bulk-load-skip", after: []string{"20261118-mysql-tasks"}, steps: []string{`DO 0`}})
}
