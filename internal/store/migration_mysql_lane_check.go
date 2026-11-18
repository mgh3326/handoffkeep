package store

// The MySQL twin of 20261118-lane-check (operator decision 418, hk 5201): the
// lane_looks table and the notification_outbox CHECK that admits
// 'lane_check'. It follows 20261118-drift-outbox-kind, whose v3 constraint it
// widens.
//
// MySQL cannot swap a CHECK in one statement and its DDL commits step by step,
// so the swap is restartable instead: the wider constraint is added under a
// new name first (the two coexist, the old one still binding), then the old
// one is dropped when it is still there. CREATE TABLE locks only the new
// table.
const mysqlOutboxKindsV4 = `'decision_answered','chat_answer','dots_report','dots_decision','dots_blocked','dots_followup','yt_write_op','decision_denied','disposition_answered','deploy_result','deploy_lag','task_order','task_drift','lane_check'`

var mysqlLaneCheckSteps = []string{
	`CREATE TABLE IF NOT EXISTS lane_looks (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		lane VARCHAR(255) NOT NULL CHECK(lane <> ''),
		notice_event_id VARCHAR(256) NOT NULL CHECK(notice_event_id <> ''),
		findings_hash VARCHAR(16) NOT NULL CHECK(findings_hash REGEXP '^[0-9a-f]{8}$'),
		note LONGTEXT NOT NULL DEFAULT (''),
		actor VARCHAR(320) NOT NULL CHECK(actor <> ''),
		created_at DATETIME(6) NOT NULL,
		CONSTRAINT lane_looks_notice UNIQUE(lane, notice_event_id),
		KEY lane_looks_lane(lane, id DESC)
	)` + mysqlTableOptions,
	mysqlIfNoConstraint("notification_outbox", "notification_outbox_kind_check_v4",
		`ALTER TABLE notification_outbox ADD CONSTRAINT notification_outbox_kind_check_v4 CHECK(kind IN (`+mysqlOutboxKindsV4+`))`),
	mysqlIfConstraint("notification_outbox", "notification_outbox_kind_check_v3",
		`ALTER TABLE notification_outbox DROP CHECK notification_outbox_kind_check_v3`),
}

func init() {
	registerMySQLMigration(mysqlMigration{id: laneCheckMigrationID, after: []string{driftOutboxKindMigrationID}, steps: mysqlLaneCheckSteps})
}
