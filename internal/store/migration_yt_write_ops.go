package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261109-yt-write-ops adds the op log hk's YouTrack write protocol runs on
// (hk 3697; internal/ytwrite) and the map of hk identities to YouTrack ids that
// an applied op commits with its state change.
//
// Expand-only: two new tables and their indexes, a self-referencing foreign key
// (parent_id) and one trigger function per table. It touches no existing table
// and takes no lock on one; CREATE TRIGGER and CREATE INDEX take their locks on
// the new, empty tables only. The previous release neither reads nor writes
// them, and the migration writes no row. Nothing in the hk binary writes either
// table yet: the first writer is slice b1a.
//
// yt_write_ops is an append-only log whose rows move only along the lifecycle
// prepared -> dispatched -> applied | rejected | remote_unknown (a 429 or 401
// returns a dispatched row to prepared). The trigger refuses any change to the
// columns that identify the write, any move the lifecycle does not allow, any
// change to a resolved row, and every delete, so a retry can never see a payload
// other than the one that was committed before the remote call.
func init() {
	registerMigration(migration{ID: "20261109-yt-write-ops", Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, ytWriteOpsStatements)
	}})
}

var ytWriteOpsStatements = []string{
	`CREATE TABLE IF NOT EXISTS yt_write_ops (
		id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		principal TEXT NOT NULL CHECK(principal <> '' AND length(principal) <= 200),
		kind TEXT NOT NULL CHECK(kind ~ '^[a-z][a-z0-9_]{0,63}$'),
		idem_key TEXT NOT NULL CHECK(idem_key <> '' AND length(idem_key) <= 200),
		payload_sha TEXT NOT NULL CHECK(payload_sha <> '' AND length(payload_sha) <= 128),
		payload JSONB NOT NULL,
		entity_kind TEXT NOT NULL CHECK(entity_kind ~ '^[a-z][a-z0-9_]{0,63}$'),
		entity_id TEXT NOT NULL CHECK(entity_id <> '' AND length(entity_id) <= 200),
		yt_target_id TEXT NOT NULL DEFAULT '' CHECK(length(yt_target_id) <= 64),
		actor TEXT NOT NULL CHECK(actor <> '' AND length(actor) <= 200),
		parent_id BIGINT REFERENCES yt_write_ops(id),
		child_seq INTEGER NOT NULL DEFAULT 0 CHECK(child_seq >= 0),
		state TEXT NOT NULL CHECK(state IN ('prepared','dispatched','applied','rejected','remote_unknown')),
		yt_result_id TEXT NOT NULL DEFAULT '' CHECK(length(yt_result_id) <= 64),
		reason TEXT NOT NULL DEFAULT '' CHECK(length(reason) <= 200),
		last_error TEXT NOT NULL DEFAULT '' CHECK(length(last_error) <= 2000),
		attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts >= 0),
		reconciles INTEGER NOT NULL DEFAULT 0 CHECK(reconciles >= 0),
		created_at TIMESTAMPTZ NOT NULL,
		dispatched_at TIMESTAMPTZ,
		next_attempt_at TIMESTAMPTZ NOT NULL,
		resolved_at TIMESTAMPTZ,
		UNIQUE(principal, kind, idem_key),
		CHECK((state IN ('applied','rejected')) = (resolved_at IS NOT NULL)),
		CHECK(state <> 'applied' OR yt_result_id <> ''))`,
	// The ordered write lane of one entity: its unresolved ops, oldest first.
	`CREATE INDEX IF NOT EXISTS yt_write_ops_entity_open ON yt_write_ops(entity_kind, entity_id, id) WHERE state NOT IN ('applied','rejected')`,
	// What the worker scans: prepared and remote_unknown ops by due time.
	`CREATE INDEX IF NOT EXISTS yt_write_ops_due ON yt_write_ops(next_attempt_at, id) WHERE state IN ('prepared','remote_unknown')`,
	// Dispatches that may have been lost.
	`CREATE INDEX IF NOT EXISTS yt_write_ops_dispatched ON yt_write_ops(dispatched_at, id) WHERE state = 'dispatched'`,
	`CREATE OR REPLACE FUNCTION handoffkeep_yt_write_ops_guard() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN
		IF TG_OP = 'DELETE' THEN
			RAISE EXCEPTION 'yt_write_ops is append-only';
		END IF;
		IF NEW.id IS DISTINCT FROM OLD.id OR NEW.principal IS DISTINCT FROM OLD.principal
			OR NEW.kind IS DISTINCT FROM OLD.kind OR NEW.idem_key IS DISTINCT FROM OLD.idem_key
			OR NEW.payload_sha IS DISTINCT FROM OLD.payload_sha OR NEW.payload IS DISTINCT FROM OLD.payload
			OR NEW.entity_kind IS DISTINCT FROM OLD.entity_kind OR NEW.entity_id IS DISTINCT FROM OLD.entity_id
			OR NEW.yt_target_id IS DISTINCT FROM OLD.yt_target_id OR NEW.actor IS DISTINCT FROM OLD.actor
			OR NEW.parent_id IS DISTINCT FROM OLD.parent_id OR NEW.child_seq IS DISTINCT FROM OLD.child_seq
			OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
			RAISE EXCEPTION 'yt_write_ops: the outbound payload and identity of an op are immutable';
		END IF;
		IF OLD.state IN ('applied','rejected') THEN
			RAISE EXCEPTION 'yt_write_ops: op % is % and cannot change', OLD.id, OLD.state;
		END IF;
		IF NOT ((OLD.state = 'prepared' AND NEW.state IN ('dispatched','rejected'))
			OR (OLD.state = 'dispatched' AND NEW.state IN ('prepared','applied','rejected','remote_unknown'))
			OR (OLD.state = 'remote_unknown' AND NEW.state IN ('remote_unknown','applied','rejected'))) THEN
			RAISE EXCEPTION 'yt_write_ops: op % cannot move from % to %', OLD.id, OLD.state, NEW.state;
		END IF;
		RETURN NEW;
	END; $$`,
	`DROP TRIGGER IF EXISTS yt_write_ops_guard ON yt_write_ops`,
	`CREATE TRIGGER yt_write_ops_guard BEFORE UPDATE OR DELETE ON yt_write_ops FOR EACH ROW EXECUTE FUNCTION handoffkeep_yt_write_ops_guard()`,
	// One hk identity <-> one YouTrack id, per entity kind. Committed with the
	// op's `applied`, in the same transaction.
	`CREATE TABLE IF NOT EXISTS yt_entity_map (
		entity_kind TEXT NOT NULL CHECK(entity_kind ~ '^[a-z][a-z0-9_]{0,63}$'),
		hk_key TEXT NOT NULL CHECK(hk_key <> '' AND length(hk_key) <= 200),
		yt_id TEXT NOT NULL CHECK(yt_id <> '' AND length(yt_id) <= 64),
		yt_parent_id TEXT NOT NULL DEFAULT '' CHECK(length(yt_parent_id) <= 64),
		op_id BIGINT NOT NULL REFERENCES yt_write_ops(id),
		created_at TIMESTAMPTZ NOT NULL,
		PRIMARY KEY(entity_kind, hk_key),
		UNIQUE(entity_kind, yt_id))`,
}
