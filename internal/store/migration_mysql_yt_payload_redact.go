package store

// The hk 5056 payload redaction on MySQL (hk 4899 B4a): yt_write_ops.payload
// becomes nullable and the guard update trigger allows exactly one more
// change, the redaction of a resolved op older than 30 days.
//
// Expand-only: MODIFY to a nullable JSON is a widening, and the trigger is
// replaced by DROP + CREATE, which takes a metadata lock on yt_write_ops
// only while the trigger is swapped. The previous release never NULLs
// a payload, so the wider rule changes nothing it does. The steps are
// restartable: the ALTER runs only while the column is still NOT NULL
// (mysqlIfColumnNotNull), and DROP TRIGGER IF EXISTS makes the CREATE
// repeatable.
var mysqlYTPayloadRedactSteps = []string{
	mysqlIfColumnNotNull("yt_write_ops", "payload", `ALTER TABLE yt_write_ops MODIFY payload JSON NULL`),
	// What the alert tick's redaction batch scans: resolved ops by age.
	// Plain (not partial) indexes have no predicate in MySQL, so this one
	// covers resolved_at for the batch's range; it is not drift-compared.
	mysqlIfNoIndex("yt_write_ops", "yt_write_ops_payload_redact", `CREATE INDEX yt_write_ops_payload_redact ON yt_write_ops(resolved_at, id)`),
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
			IF OLD.state IN ('applied','rejected') AND NOT (`+mysqlYTPayloadRedactCond+`) THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='yt_write_ops: a resolved op cannot change';
			END IF;
			IF NOT ((OLD.state = 'prepared' AND NEW.state IN ('dispatched','rejected'))
				OR (OLD.state = 'prepared' AND NEW.state = 'prepared' AND OLD.kind = 'comment_append' AND OLD.yt_target_id = '')
				OR (OLD.state = 'dispatched' AND NEW.state IN ('prepared','applied','rejected','remote_unknown'))
				OR (OLD.state = 'remote_unknown' AND NEW.state IN ('remote_unknown','applied','rejected'))
				OR (`+mysqlYTPayloadRedactCond+`)) THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='yt_write_ops: an op cannot make that state move';
			END IF;`),
}

// mysqlYTPayloadRedactCond is the hk 5056 redaction exception, shared by the
// three arms of the guard update trigger above: an applied or rejected row
// whose resolved_at is more than 30 days ago may set payload to NULL, with
// every other column unchanged, including payload_sha, the receipt of what
// was sent. The tenant guard trigger has no such exception, so the tenant
// stays immutable.
const mysqlYTPayloadRedactCond = `OLD.state IN ('applied','rejected') AND (NEW.state <=> OLD.state)
	AND OLD.payload IS NOT NULL AND NEW.payload IS NULL
	AND OLD.resolved_at IS NOT NULL AND OLD.resolved_at < DATE_SUB(UTC_TIMESTAMP(6), INTERVAL 30 DAY)
	AND (NEW.payload_sha <=> OLD.payload_sha)
	AND (NEW.yt_result_id <=> OLD.yt_result_id)
	AND (NEW.reason <=> OLD.reason)
	AND (NEW.last_error <=> OLD.last_error)
	AND (NEW.dispatched_at <=> OLD.dispatched_at)
	AND (NEW.next_attempt_at <=> OLD.next_attempt_at)
	AND (NEW.resolved_at <=> OLD.resolved_at)
	AND (NEW.attempts <=> OLD.attempts)
	AND (NEW.reconciles <=> OLD.reconciles)`

func init() {
	registerMySQLMigration(mysqlMigration{id: YTWriteOpPayloadRedactMigration, after: []string{"20261118-mysql-yt-write-ops"}, steps: mysqlYTPayloadRedactSteps})
}
