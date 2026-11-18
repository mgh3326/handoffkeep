package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// YTWriteOpPayloadRedactMigration (hk 5056) redacts the payloads of applied
// or rejected yt_write_ops older than 30 days (design/2026-11-17/hk4899-mysql
// §2, operator decision dr-5056-2 with answer A on 2026-11-18): the payload
// becomes NULL and payload_sha is kept as the receipt.
//
// It widens the guard with exactly one more change: an applied or rejected
// row whose resolved_at is more than 30 days ago may set payload to NULL,
// with every other column unchanged. The tenant trigger is untouched, so the
// tenant stays immutable; the issue/decision apply guards fire only on a move
// into applied, which a redaction never is.
//
// Expand-only: ALTER COLUMN DROP NOT NULL is a widening, CREATE OR REPLACE
// FUNCTION takes no table lock beyond the function's catalog row, and the new
// partial index takes SHARE on yt_write_ops only for its scan of a small
// table. The previous release never NULLs a payload, so the wider rule
// changes nothing it does: it inserts non-NULL payloads and never updates a
// resolved row. After names the migration whose function this one replaces,
// so it never runs before it.
const YTWriteOpPayloadRedactMigration = "20261118-yt-write-op-payload-redact"

func init() {
	registerMigration(migration{ID: YTWriteOpPayloadRedactMigration, After: []string{YTCommentHoldMigration}, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, ytWriteOpPayloadRedactStatements)
	}})
}

var ytWriteOpPayloadRedactStatements = []string{
	`ALTER TABLE yt_write_ops ALTER COLUMN payload DROP NOT NULL`,
	// What the alert tick's redaction batch scans: resolved ops that still
	// hold a payload, by age. The partial predicate is exactly the batch's,
	// so the steady-state scan reads almost nothing.
	`CREATE INDEX IF NOT EXISTS yt_write_ops_payload_redact ON yt_write_ops(resolved_at, id) WHERE state IN ('applied','rejected') AND payload IS NOT NULL`,
	`CREATE OR REPLACE FUNCTION handoffkeep_yt_write_ops_guard() RETURNS trigger LANGUAGE plpgsql AS $$
	DECLARE
		retarget BOOLEAN;
		reissue BOOLEAN;
		redact BOOLEAN;
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
		-- hk 5056: a resolved op's payload becomes NULL once, more than 30
		-- days after it resolved. Every other column stays exactly as it
		-- was, including payload_sha, the receipt of what was sent.
		redact := TG_OP = 'UPDATE' AND OLD.state IN ('applied','rejected') AND NEW.state = OLD.state
			AND OLD.payload IS NOT NULL AND NEW.payload IS NULL
			AND OLD.resolved_at IS NOT NULL AND OLD.resolved_at < now() - interval '30 days'
			AND NEW.payload_sha IS NOT DISTINCT FROM OLD.payload_sha
			AND NEW.yt_result_id IS NOT DISTINCT FROM OLD.yt_result_id
			AND NEW.reason IS NOT DISTINCT FROM OLD.reason
			AND NEW.last_error IS NOT DISTINCT FROM OLD.last_error
			AND NEW.dispatched_at IS NOT DISTINCT FROM OLD.dispatched_at
			AND NEW.next_attempt_at IS NOT DISTINCT FROM OLD.next_attempt_at
			AND NEW.resolved_at IS NOT DISTINCT FROM OLD.resolved_at
			AND NEW.attempts IS NOT DISTINCT FROM OLD.attempts
			AND NEW.reconciles IS NOT DISTINCT FROM OLD.reconciles;
		IF NEW.id IS DISTINCT FROM OLD.id OR NEW.principal IS DISTINCT FROM OLD.principal
			OR NEW.kind IS DISTINCT FROM OLD.kind OR NEW.idem_key IS DISTINCT FROM OLD.idem_key
			OR (NEW.payload_sha IS DISTINCT FROM OLD.payload_sha AND NOT (retarget OR reissue))
			OR (NEW.payload IS DISTINCT FROM OLD.payload AND NOT (reissue OR redact))
			OR NEW.entity_kind IS DISTINCT FROM OLD.entity_kind OR NEW.entity_id IS DISTINCT FROM OLD.entity_id
			OR (NEW.yt_target_id IS DISTINCT FROM OLD.yt_target_id AND NOT (retarget OR reissue)) OR NEW.actor IS DISTINCT FROM OLD.actor
			OR NEW.parent_id IS DISTINCT FROM OLD.parent_id OR NEW.child_seq IS DISTINCT FROM OLD.child_seq
			OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
			RAISE EXCEPTION 'yt_write_ops: the outbound payload and identity of an op are immutable';
		END IF;
		IF OLD.state IN ('applied','rejected') AND NOT redact THEN
			RAISE EXCEPTION 'yt_write_ops: op % is % and cannot change', OLD.id, OLD.state;
		END IF;
		IF NOT ((OLD.state = 'prepared' AND NEW.state IN ('dispatched','rejected'))
			-- hk 4457: a queued comment op held at its claim stays prepared
			-- and is due again later.
			OR (OLD.state = 'prepared' AND NEW.state = 'prepared' AND OLD.kind = 'comment_append' AND OLD.yt_target_id = '')
			OR (OLD.state = 'dispatched' AND NEW.state IN ('prepared','applied','rejected','remote_unknown'))
			OR (OLD.state = 'remote_unknown' AND NEW.state IN ('remote_unknown','applied','rejected'))
			OR redact) THEN
			RAISE EXCEPTION 'yt_write_ops: op % cannot move from % to %', OLD.id, OLD.state, NEW.state;
		END IF;
		RETURN NEW;
	END; $$`,
}
