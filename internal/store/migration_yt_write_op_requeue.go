package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// YTWriteOpRequeueMigration (hk 5506) lets an operator requeue the ops that
// YouTrack's read-only maintenance answer rejected before hk 5494 (#521): a
// rejected op whose recorded refusal is 418 invalid_state, of a kind whose
// refusal is its only write (ytRequeueKinds), returns to prepared with the
// same id, key and payload, so the normal dispatcher sends it once more.
//
// The guard change is one more arm of handoffkeep_yt_write_ops_guard, the
// newest definition (20261118-yt-write-op-payload-redact): the requeue
// arm is true only for that row shape, and every identity and payload check
// stays as it was. The guard keeps its other rule: a resolved row changes
// only by this arm or the redaction. The table yt_write_op_requeues records
// each requeue (who, when, and the rejected row's reason, error, attempts
// and resolved_at) in the same transaction, so the history survives the
// reset.
//
// Expand-only: CREATE TABLE and CREATE OR REPLACE FUNCTION take no lock on
// yt_write_ops. The previous release never moves a rejected row back to
// prepared, so the wider guard changes nothing it does. After names the
// migration whose function this one replaces, so it never runs before it.
const YTWriteOpRequeueMigration = "20261120-yt-write-op-requeue"

func init() {
	registerMigration(migration{ID: YTWriteOpRequeueMigration, After: []string{YTWriteOpPayloadRedactMigration}, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, ytWriteOpRequeueStatements)
	}})
}

var ytWriteOpRequeueStatements = []string{
	`CREATE TABLE IF NOT EXISTS yt_write_op_requeues (
		id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		op_id BIGINT NOT NULL REFERENCES yt_write_ops(id),
		requeued_at TIMESTAMPTZ NOT NULL,
		by_operator TEXT NOT NULL CHECK(by_operator <> '' AND length(by_operator) <= 200),
		prior_attempts INTEGER NOT NULL CHECK(prior_attempts >= 0),
		prior_reason TEXT NOT NULL CHECK(length(prior_reason) <= 200),
		prior_last_error TEXT NOT NULL CHECK(length(prior_last_error) <= 2000),
		prior_resolved_at TIMESTAMPTZ NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS yt_write_op_requeues_op ON yt_write_op_requeues(op_id, id)`,
	`CREATE OR REPLACE FUNCTION handoffkeep_yt_write_ops_guard() RETURNS trigger LANGUAGE plpgsql AS $$
	DECLARE
		retarget BOOLEAN;
		reissue BOOLEAN;
		redact BOOLEAN;
		requeue BOOLEAN;
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
		-- hk 5506: a rejected op refused by YouTrack's read-only maintenance
		-- answer (418 invalid_state, the text writeFailed records as
		-- last_error) returns to prepared, as a fresh prepared op: attempts 0,
		-- no reason, no error, not resolved. Its payload and identity are
		-- unchanged. Only the kinds whose refusal is their only write
		-- (ytRequeueKinds in Go) qualify, and only with a payload, which a
		-- redaction removes.
		requeue := TG_OP = 'UPDATE' AND OLD.state = 'rejected' AND NEW.state = 'prepared'
			AND OLD.reason = 'http_418' AND position('418 invalid_state' IN OLD.last_error) > 0
			AND OLD.kind IN ('comment_append','article_create') AND OLD.payload IS NOT NULL
			AND OLD.resolved_at IS NOT NULL AND NEW.resolved_at IS NULL
			AND NEW.attempts = 0 AND OLD.reconciles = 0 AND NEW.reconciles = 0
			AND NEW.yt_result_id = OLD.yt_result_id AND NEW.reason = '' AND NEW.last_error = ''
			AND NEW.dispatched_at IS NULL;
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
		IF OLD.state IN ('applied','rejected') AND NOT (redact OR requeue) THEN
			RAISE EXCEPTION 'yt_write_ops: op % is % and cannot change', OLD.id, OLD.state;
		END IF;
		IF NOT ((OLD.state = 'prepared' AND NEW.state IN ('dispatched','rejected'))
			-- hk 4457: a queued comment op held at its claim stays prepared
			-- and is due again later.
			OR (OLD.state = 'prepared' AND NEW.state = 'prepared' AND OLD.kind = 'comment_append' AND OLD.yt_target_id = '')
			OR (OLD.state = 'dispatched' AND NEW.state IN ('prepared','applied','rejected','remote_unknown'))
			OR (OLD.state = 'remote_unknown' AND NEW.state IN ('remote_unknown','applied','rejected'))
			OR redact OR requeue) THEN
			RAISE EXCEPTION 'yt_write_ops: op % cannot move from % to %', OLD.id, OLD.state, NEW.state;
		END IF;
		RETURN NEW;
	END; $$`,
}
