package store

// The hk 5506 requeue on MySQL: the PostgreSQL migration of the same ID
// (migration_yt_write_op_requeue.go) in MySQL form. It adds the
// yt_write_op_requeues audit table and replaces the UPDATE guard trigger of
// yt_write_ops with the redaction version plus one arm: a rejected op with
// the read-only 418 record, of a kind whose refusal is its only write, may
// return to prepared as a fresh op. Identity, payload and tenant checks are
// the redaction trigger's, unchanged.
//
// Expand-only: CREATE TABLE IF NOT EXISTS takes no lock on yt_write_ops, and
// the trigger swap (DROP + CREATE) takes a metadata lock on yt_write_ops only
// while it runs. The previous release never moves a rejected row back to
// prepared, so the wider trigger changes nothing it does.
var mysqlYTWriteOpRequeueSteps = []string{
	`CREATE TABLE IF NOT EXISTS yt_write_op_requeues (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		op_id BIGINT NOT NULL,
		requeued_at DATETIME(6) NOT NULL,
		by_operator VARCHAR(200) NOT NULL,
		prior_attempts INT NOT NULL,
		prior_reason VARCHAR(200) NOT NULL,
		prior_last_error TEXT NOT NULL,
		prior_resolved_at DATETIME(6) NOT NULL,
		INDEX yt_write_op_requeues_op (op_id, id),
		CONSTRAINT yt_write_op_requeues_op_fk FOREIGN KEY (op_id) REFERENCES yt_write_ops(id),
		CONSTRAINT yt_write_op_requeues_by_check CHECK(by_operator <> '' AND CHAR_LENGTH(by_operator) <= 200),
		CONSTRAINT yt_write_op_requeues_attempts_check CHECK(prior_attempts >= 0),
		CONSTRAINT yt_write_op_requeues_reason_check CHECK(CHAR_LENGTH(prior_reason) <= 200))` + mysqlTableOptions,
	`DROP TRIGGER IF EXISTS yt_write_ops_guard_update`,
	`CREATE TRIGGER yt_write_ops_guard_update BEFORE UPDATE ON yt_write_ops FOR EACH ROW ` + mysqlBulkGuard(
		`IF (NOT (NEW.id <=> OLD.id) OR NOT (NEW.principal <=> OLD.principal)
				OR NOT (NEW.kind <=> OLD.kind) OR NOT (NEW.idem_key <=> OLD.idem_key)
				OR (NOT (NEW.payload_sha <=> OLD.payload_sha)
					AND NOT (OLD.kind = 'comment_append' AND OLD.state = 'prepared' AND NEW.state = 'dispatched' AND OLD.yt_target_id = '' AND NEW.yt_target_id <> '')
					AND NOT (OLD.kind = 'issue_create' AND OLD.state = 'prepared' AND NEW.state = 'dispatched' AND NEW.yt_target_id <> ''))
				OR (NOT (NEW.payload <=> OLD.payload)
					AND NOT (OLD.kind = 'issue_create' AND OLD.state = 'prepared' AND NEW.state = 'dispatched' AND NEW.yt_target_id <> '')
					AND NOT (`+mysqlYTPayloadRedactCond+`))
				OR NOT (NEW.entity_kind <=> OLD.entity_kind) OR NOT (NEW.entity_id <=> OLD.entity_id)
				OR (NOT (NEW.yt_target_id <=> OLD.yt_target_id)
					AND NOT (OLD.kind = 'comment_append' AND OLD.state = 'prepared' AND NEW.state = 'dispatched' AND OLD.yt_target_id = '' AND NEW.yt_target_id <> '')
					AND NOT (OLD.kind = 'issue_create' AND OLD.state = 'prepared' AND NEW.state = 'dispatched' AND NEW.yt_target_id <> ''))
				OR NOT (NEW.actor <=> OLD.actor)
				OR NOT (NEW.parent_id <=> OLD.parent_id) OR NOT (NEW.child_seq <=> OLD.child_seq)
				OR NOT (NEW.created_at <=> OLD.created_at)) THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='yt_write_ops: the outbound payload and identity of an op are immutable';
			END IF;
			IF OLD.state IN ('applied','rejected') AND NOT (`+mysqlYTPayloadRedactCond+`) AND NOT (`+mysqlYTRequeueCond+`) THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='yt_write_ops: a resolved op cannot change';
			END IF;
			IF NOT ((OLD.state = 'prepared' AND NEW.state IN ('dispatched','rejected'))
				OR (OLD.state = 'prepared' AND NEW.state = 'prepared' AND OLD.kind = 'comment_append' AND OLD.yt_target_id = '')
				OR (OLD.state = 'dispatched' AND NEW.state IN ('prepared','applied','rejected','remote_unknown'))
				OR (OLD.state = 'remote_unknown' AND NEW.state IN ('remote_unknown','applied','rejected'))
				OR (`+mysqlYTPayloadRedactCond+`)
				OR (`+mysqlYTRequeueCond+`)) THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='yt_write_ops: an op cannot make that state move';
			END IF;`),
}

// mysqlYTRequeueCond is the hk 5506 requeue exception, shared by the three
// arms of the guard update trigger above: a rejected op whose last_error holds
// YouTrack's read-only answer (418 invalid_state, as writeFailed records it)
// returns to prepared as a fresh op, attempts 0, no reason or error, not
// resolved. Its kind is one whose refusal is its only write, it keeps its
// payload, and it never went through a reconcile.
const mysqlYTRequeueCond = `OLD.state = 'rejected' AND NEW.state = 'prepared' AND OLD.reason = 'http_418'
	AND LOCATE('418 invalid_state', OLD.last_error) > 0
	AND OLD.kind IN ('comment_append','article_create') AND OLD.payload IS NOT NULL
	AND OLD.resolved_at IS NOT NULL AND NEW.resolved_at IS NULL
	AND NEW.attempts = 0 AND OLD.reconciles = 0 AND NEW.reconciles = 0
	AND (NEW.yt_result_id <=> OLD.yt_result_id) AND NEW.reason = '' AND NEW.last_error = ''
	AND NEW.dispatched_at IS NULL`

func init() {
	registerMySQLMigration(mysqlMigration{id: YTWriteOpRequeueMigration, after: []string{YTWriteOpPayloadRedactMigration}, steps: mysqlYTWriteOpRequeueSteps})
}
