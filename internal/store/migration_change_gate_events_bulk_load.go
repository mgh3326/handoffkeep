package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Store-copy inserts changes and change_gate_events separately. Without this
// guard, copying a changes row with a gate would synthesize a second history
// event before the copied event arrives. Ordinary writes keep the existing
// trigger behavior. Replacing the function takes no application-table lock.
func init() {
	registerMigration(migration{ID: "20261117-change-gate-events-bulk-load", After: []string{"20261113-change-gate-events"}, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, []string{`CREATE OR REPLACE FUNCTION change_gate_event_record() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF current_setting('handoffkeep.bulk_load', true) = 'on' THEN
				RETURN NULL;
			END IF;
			INSERT INTO change_gate_events(change_id,head_sha,state,changed,reasons,policy_revision,decided_at,source)
			VALUES(NEW.id,NEW.gate_head,NEW.gate_state,
				TG_OP='INSERT' OR OLD.gate_state IS DISTINCT FROM NEW.gate_state OR OLD.gate_head IS DISTINCT FROM NEW.gate_head,
				coalesce(NEW.gate_reasons,'[]'::jsonb),NEW.gate_policy_revision,NEW.gate_decided_at,coalesce(NEW.gate_source,'review'))
			ON CONFLICT DO NOTHING;
			RETURN NULL;
		END $$`})
	}})
	registerMySQLMigration(mysqlMigration{id: "20261117-change-gate-events-bulk-load", after: []string{"20261117-mysql-changes"}, steps: []string{`DO 0`}})
}
