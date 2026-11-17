package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// The store-copy tool loads decision_requests and sync_decisions themselves,
// so a copied decision request must not emit a second feed row. Only the
// copier sets the transaction-local GUC; an unset GUC preserves normal writes.
// Lock: replacing the trigger function takes no table lock. The existing
// trigger resolves its function by name, so it immediately uses the new body.
func init() {
	registerMigration(migration{ID: "20261118-decision-sync-bulk-load", Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, []string{
			`CREATE OR REPLACE FUNCTION handoffkeep_sync_emit_decision_record() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE row_id TEXT;
BEGIN
  IF current_setting('handoffkeep.bulk_load', true) = 'on' THEN RETURN NULL; END IF;
  IF TG_OP='DELETE' THEN row_id := OLD.id::text; ELSE row_id := NEW.id::text; END IF;
  INSERT INTO sync_log(model,model_id,action,at) VALUES('decision','record:'||row_id,substr(TG_OP,1,1),clock_timestamp());
  RETURN NULL;
END $$`,
		})
	}})
}
