package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// YTCommentQueuedTargetMigration (hk 4263) lets a comment_append op queued
// behind its task's issue_create receive its target when it is claimed.
// Under comments authority youtrack a comment on a task whose issue is not
// created yet is accepted with an op that has no target (an empty yt_target_id):
// the issue id exists only once the create applies and publishes the task's
// binding. The claim that dispatches the op reads that binding and fills the
// target, with the payload hash the target is part of (claimYTCommentTargetTx).
//
// It replaces the yt_write_ops guard function with the same rules plus one
// exception: a prepared comment_append op with an empty target may, in the
// update that moves it to dispatched, set yt_target_id to a non-empty value
// and payload_sha with it. Every other identity column stays immutable, and a
// target once set never changes again.
//
// Expand-only: CREATE OR REPLACE FUNCTION on the existing trigger function.
// It takes no table lock beyond the function's catalog row; the trigger
// itself is unchanged. The previous release never writes an empty-target op
// and never updates a target, so the wider rule changes nothing it does; its
// comment gate (b.issue_id = o.yt_target_id) never admits an empty-target op,
// so it never sends one.
const YTCommentQueuedTargetMigration = "20261112-yt-comment-queued-target"

func init() {
	registerMigration(migration{ID: YTCommentQueuedTargetMigration, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, ytCommentQueuedTargetStatements)
	}})
}

var ytCommentQueuedTargetStatements = []string{
	`CREATE OR REPLACE FUNCTION handoffkeep_yt_write_ops_guard() RETURNS trigger LANGUAGE plpgsql AS $$
	DECLARE
		retarget BOOLEAN;
	BEGIN
		IF TG_OP = 'DELETE' THEN
			RAISE EXCEPTION 'yt_write_ops is append-only';
		END IF;
		-- hk 4263: a queued comment op receives its target once, at its claim.
		retarget := TG_OP = 'UPDATE' AND OLD.kind = 'comment_append' AND OLD.state = 'prepared' AND NEW.state = 'dispatched'
			AND OLD.yt_target_id = '' AND NEW.yt_target_id <> '';
		IF NEW.id IS DISTINCT FROM OLD.id OR NEW.principal IS DISTINCT FROM OLD.principal
			OR NEW.kind IS DISTINCT FROM OLD.kind OR NEW.idem_key IS DISTINCT FROM OLD.idem_key
			OR (NEW.payload_sha IS DISTINCT FROM OLD.payload_sha AND NOT retarget) OR NEW.payload IS DISTINCT FROM OLD.payload
			OR NEW.entity_kind IS DISTINCT FROM OLD.entity_kind OR NEW.entity_id IS DISTINCT FROM OLD.entity_id
			OR (NEW.yt_target_id IS DISTINCT FROM OLD.yt_target_id AND NOT retarget) OR NEW.actor IS DISTINCT FROM OLD.actor
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
}
