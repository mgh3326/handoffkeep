package store

// Version 48 moves the decision request outbox from decision_notify (v38) to
// decision_deliveries: one row per (request, destination), so a second
// destination is a new row rather than new columns (docs/decision-discord.md).
//
// A version is the effective status the destination must show: open,
// answered, superseded or expired, as decisionEffectiveStatusSQL derives it.
// It is not a stored counter, because expiry has no writer of its own
// (decision_notify.go): an expired request is due again only because its
// effective status moved past applied_version. desired_version is the version
// the worker last tried to apply, applied_version the one the destination
// shows ("" before the first post). state is pending while the worker owns
// the row; unsent and failed are final, as last_status was in v38.
//
// The backfill copies every decision_notify row in the same transaction,
// after LOCK TABLE decision_notify IN EXCLUSIVE MODE. EXCLUSIVE conflicts with
// the ROW SHARE an older worker's SELECT ... FOR UPDATE takes and with the
// ROW EXCLUSIVE of its inserts and updates, so the copy waits for a worker
// that has claimed a row and is still sending, and no new claim or settle
// starts until the copy commits; SHARE would let a claim in flight settle
// only decision_notify after the copy, and the new worker would send it
// again. Plain reads (ACCESS SHARE) still pass. Locks: EXCLUSIVE on
// decision_notify, plus the new table (ACCESS EXCLUSIVE on create, ROW
// EXCLUSIVE for the copy), which nothing else reads yet. decision_notify is
// left in place, unwritten, for one release.
var decisionDeliveriesV48 = []string{
	`CREATE TABLE IF NOT EXISTS decision_deliveries (
		request_id BIGINT NOT NULL,
		destination TEXT NOT NULL CHECK(destination IN ('discord')),
		message_id TEXT NOT NULL DEFAULT '',
		desired_version TEXT NOT NULL DEFAULT '',
		applied_version TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL CHECK(state IN ('pending','unsent','failed')),
		attempts INT NOT NULL DEFAULT 0,
		next_attempt_at TIMESTAMPTZ NOT NULL,
		last_error_code TEXT NOT NULL DEFAULT '',
		posted_at TIMESTAMPTZ,
		edited_at TIMESTAMPTZ,
		created_at TIMESTAMPTZ NOT NULL,
		PRIMARY KEY(request_id, destination)
	)`,
	`CREATE INDEX IF NOT EXISTS decision_deliveries_due ON decision_deliveries(next_attempt_at, request_id) WHERE state='pending'`,
	`LOCK TABLE decision_notify IN EXCLUSIVE MODE`,
	`INSERT INTO decision_deliveries(request_id,destination,message_id,desired_version,applied_version,state,attempts,next_attempt_at,last_error_code,posted_at,edited_at,created_at)
		SELECT request_id,'discord',message_id,
			CASE WHEN last_status IN ('unsent','failed') THEN '' ELSE last_status END,
			CASE WHEN last_status IN ('unsent','failed') THEN '' ELSE last_status END,
			CASE WHEN last_status IN ('unsent','failed') THEN last_status ELSE 'pending' END,
			attempts,next_attempt_at,btrim(split_part(last_error,':',1)),posted_at,edited_at,created_at
		FROM decision_notify
		ON CONFLICT(request_id,destination) DO NOTHING`,
	`INSERT INTO schema_version(version) VALUES (48)`,
}
