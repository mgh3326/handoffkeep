package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// YTCommentHoldMigration (hk 4457) lets the claim hold a queued comment op.
// A comment queued behind its task's issue_create, whose issue landed in a
// project the task has since left, is kept prepared and made due again after
// a minute (the claim's hold branch): that is an UPDATE from prepared
// to prepared, which every earlier guard refused ("cannot move from prepared
// to prepared"), so the claim failed and the op was retried at once.
//
// The hold itself was taken out before release (PR #383 review: a hold that
// waits for a later op on the strictly ordered lane can wedge it). Its
// redesign (hk 4569, yt_comment_requeue.go) rejects the comment at its claim
// and copies it again as a new op, so it needs no prepared to prepared
// transition either, and no code makes that transition now. The migration stays
// registered: an ID once shipped on a branch database is never removed, and
// the wider rule is harmless with nothing using it.
//
// It replaces the yt_write_ops guard function with the rules of
// 20261113-yt-issue-claim-target plus one transition: a prepared
// comment_append op with no target may stay prepared. The identity rules are
// unchanged, so that update can move only its scheduling columns; a target
// is still set only in the update to dispatched.
//
// Expand-only: CREATE OR REPLACE FUNCTION on the existing trigger function.
// It takes no table lock beyond the function's catalog row; the trigger
// itself is unchanged. The previous release never updates a prepared op in
// place, so the wider rule changes nothing it does. After names the
// migration whose function this one replaces, so it never runs before it.
const YTCommentHoldMigration = "20261113-yt-comment-hold"

func init() {
	registerMigration(migration{ID: YTCommentHoldMigration, After: []string{YTIssueClaimTargetMigration}, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, ytCommentHoldStatements)
	}})
}

var ytCommentHoldStatements = []string{
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
			-- hk 4457: a queued comment op held at its claim stays prepared
			-- and is due again later.
			OR (OLD.state = 'prepared' AND NEW.state = 'prepared' AND OLD.kind = 'comment_append' AND OLD.yt_target_id = '')
			OR (OLD.state = 'dispatched' AND NEW.state IN ('prepared','applied','rejected','remote_unknown'))
			OR (OLD.state = 'remote_unknown' AND NEW.state IN ('remote_unknown','applied','rejected'))) THEN
			RAISE EXCEPTION 'yt_write_ops: op % cannot move from % to %', OLD.id, OLD.state, NEW.state;
		END IF;
		RETURN NEW;
	END; $$`,
}
