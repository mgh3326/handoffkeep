package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261113-change-gate-events keeps every applied review gate decision as an
// append-only row (hk 4000; hk:doc design/2026-11-11/operator-surfaces §1).
// The changes row keeps only the newest verdict (gate_state, gate_head), so a
// blocked verdict followed by ready between two polls of the Discord poster
// was never posted. change_gate_events is the history the poster reads, keyed
// by its id instead of the verdict's text.
//
// A trigger on changes writes the row, inside the transaction of whichever
// writer changed the gate columns. The previous release, which knows only the
// snapshot, leaves history too, so no gate write can skip it.
//
// changed is true when the decision's verdict (head or state) differs from
// the one the changes row held before it. mergewake may send the same verdict
// again; that is recorded, and the poster skips it. decided_at is mergewake's
// time for the decision, so the UNIQUE key makes a repeated write a no-op.
//
// Upgrade. The steps run in this order in one transaction:
//  1. CREATE TRIGGER takes SHARE ROW EXCLUSIVE on changes until commit. Every
//     gate writer takes ROW EXCLUSIVE on changes first, so they wait here. From
//     the moment the lock is held, every later gate write fires the trigger.
//  2. The backfill then reads a settled snapshot: one event per change with a
//     gate, so a verdict the old poster had not posted yet is still posted.
//  3. The UPDATE rewrites each delivery's posted keys from the old form
//     g:<change>:<head>:<state> to g:<event id>, so a verdict already posted is
//     not posted again.
//
// Expand-only: a new table, a new trigger, one backfill and a rewrite of
// posted keys that the previous release only ever appends to. The previous
// release never reads the table. The UPDATE takes ROW EXCLUSIVE on
// task_thread_deliveries.
func init() {
	registerMigration(migration{ID: "20261113-change-gate-events", After: []string{"20261111-task-thread-deliveries"}, Apply: applyChangeGateEventsMigration})
}

func applyChangeGateEventsMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, []string{
		`CREATE TABLE IF NOT EXISTS change_gate_events (
			id BIGSERIAL PRIMARY KEY,
			change_id BIGINT NOT NULL REFERENCES changes(id),
			head_sha TEXT NOT NULL CHECK(head_sha ~ '^[0-9a-f]{40}$'),
			state TEXT NOT NULL CHECK(state IN ('ready','blocked','pending')),
			changed BOOLEAN NOT NULL,
			reasons JSONB NOT NULL DEFAULT '[]',
			policy_revision TEXT,
			decided_at TIMESTAMPTZ NOT NULL,
			source TEXT NOT NULL CHECK(source IN ('webhook','poll','fresh','review')),
			recorded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE(change_id, head_sha, state, decided_at)
		)`,
		`CREATE OR REPLACE FUNCTION change_gate_event_record() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			INSERT INTO change_gate_events(change_id,head_sha,state,changed,reasons,policy_revision,decided_at,source)
			VALUES(NEW.id,NEW.gate_head,NEW.gate_state,
				TG_OP='INSERT' OR OLD.gate_state IS DISTINCT FROM NEW.gate_state OR OLD.gate_head IS DISTINCT FROM NEW.gate_head,
				coalesce(NEW.gate_reasons,'[]'::jsonb),NEW.gate_policy_revision,NEW.gate_decided_at,coalesce(NEW.gate_source,'review'))
			ON CONFLICT DO NOTHING;
			RETURN NULL;
		END $$`,
		`CREATE OR REPLACE TRIGGER change_gate_events_record AFTER INSERT OR UPDATE OF gate_state, gate_head, gate_decided_at ON changes
			FOR EACH ROW WHEN (NEW.gate_state IS NOT NULL AND NEW.gate_head IS NOT NULL AND NEW.gate_decided_at IS NOT NULL)
			EXECUTE FUNCTION change_gate_event_record()`,
		`INSERT INTO change_gate_events(change_id,head_sha,state,changed,reasons,policy_revision,decided_at,source)
			SELECT id,gate_head,gate_state,true,coalesce(gate_reasons,'[]'::jsonb),gate_policy_revision,gate_decided_at,coalesce(gate_source,'review')
			FROM changes WHERE gate_state IS NOT NULL AND gate_head IS NOT NULL AND gate_decided_at IS NOT NULL
			ON CONFLICT DO NOTHING`,
		`UPDATE task_thread_deliveries d SET posted = d.posted || ARRAY(
			SELECT 'g:'||ev.id FROM change_tasks ct JOIN change_gate_events ev ON ev.change_id=ct.change_id
			WHERE ct.task_id=d.task_id AND ('g:'||ev.change_id||':'||ev.head_sha||':'||ev.state)=ANY(d.posted))
			WHERE d.destination='discord' AND cardinality(d.posted)>0`,
	})
}
