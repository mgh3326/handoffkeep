package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// YTIssueClaimTargetMigration (hk 3909, 4318) lets an issue_create op take
// the YouTrack project its task is in when it is claimed. A task can change
// project after its op was prepared and before it is sent; the claim that
// dispatches the op reads the task's project and sets the op's target and
// payload, with the payload hash they are part of (claimYTIssueTargetTx).
// The op keeps its id, key and place on its lane, so comments queued behind
// it stay behind it and take their target from the binding it publishes, in
// this release and in the previous one.
//
// It replaces the yt_write_ops guard function with the rules of
// 20261112-yt-comment-queued-target plus one exception: a prepared
// issue_create op may, in the update that moves it to dispatched, set
// yt_target_id (to a non-empty value), payload and payload_sha. Every other
// identity column stays immutable, and an op that was sent is never
// retargeted (dispatched, remote_unknown and resolved ops cannot return to
// that update; a deferred op returns to prepared and is retargeted at its
// next claim, before anything is sent again).
//
// Expand-only: CREATE OR REPLACE FUNCTION on the existing trigger function.
// It takes no table lock beyond the function's catalog row; the trigger
// itself is unchanged. The previous release never updates an issue_create
// op's target or payload, so the wider rule changes nothing it does. After
// names the migration whose function this one replaces, so it never runs
// before it.
const YTIssueClaimTargetMigration = "20261113-yt-issue-claim-target"

func init() {
	registerMigration(migration{ID: YTIssueClaimTargetMigration, After: []string{YTCommentQueuedTargetMigration}, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, ytIssueClaimTargetStatements)
	}})
}

var ytIssueClaimTargetStatements = []string{
	`CREATE OR REPLACE FUNCTION handoffkeep_yt_write_ops_guard() RETURNS trigger LANGUAGE plpgsql AS $$
	DECLARE
		retarget BOOLEAN;
		reissue BOOLEAN;
	BEGIN
		IF TG_OP = 'DELETE' THEN
			RAISE EXCEPTION 'yt_write_ops is append-only';
		END IF;
		-- hk 4263: a queued comment op receives its target once, at its claim.
		retarget := TG_OP = 'UPDATE' AND OLD.kind = 'comment_append' AND OLD.state = 'prepared' AND NEW.state = 'dispatched'
			AND OLD.yt_target_id = '' AND NEW.yt_target_id <> '';
		-- hk 4318: an issue_create op takes its task's project at its claim.
		reissue := TG_OP = 'UPDATE' AND OLD.kind = 'issue_create' AND OLD.state = 'prepared' AND NEW.state = 'dispatched'
			AND NEW.yt_target_id <> '';
		IF NEW.id IS DISTINCT FROM OLD.id OR NEW.principal IS DISTINCT FROM OLD.principal
			OR NEW.kind IS DISTINCT FROM OLD.kind OR NEW.idem_key IS DISTINCT FROM OLD.idem_key
			OR (NEW.payload_sha IS DISTINCT FROM OLD.payload_sha AND NOT (retarget OR reissue))
			OR (NEW.payload IS DISTINCT FROM OLD.payload AND NOT reissue)
			OR NEW.entity_kind IS DISTINCT FROM OLD.entity_kind OR NEW.entity_id IS DISTINCT FROM OLD.entity_id
			OR (NEW.yt_target_id IS DISTINCT FROM OLD.yt_target_id AND NOT (retarget OR reissue)) OR NEW.actor IS DISTINCT FROM OLD.actor
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
