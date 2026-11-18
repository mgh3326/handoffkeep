package store

// The MySQL twin of 20261118-task-orders (hk 5125). It follows the baseline
// that creates notification_outbox, whose kind CHECK it widens.
//
// MySQL cannot swap a CHECK in one statement and its DDL commits step by step,
// so the swap is restartable instead: the wider constraint is added under a new
// name first (the two coexist, the old one still binding), then the old one is
// dropped when it is still there. CREATE TABLE locks only the new table.
const mysqlOutboxKindsV2 = `'decision_answered','chat_answer','dots_report','dots_decision','dots_blocked','dots_followup','yt_write_op','decision_denied','disposition_answered','deploy_result','deploy_lag','task_order'`

var mysqlTaskOrdersSteps = []string{
	`CREATE TABLE IF NOT EXISTS task_orders (
		id VARCHAR(32) NOT NULL PRIMARY KEY CHECK(id REGEXP '^ord-[a-z2-7]{10}$'),
		task_id BIGINT NOT NULL,
		words LONGTEXT NOT NULL CHECK(TRIM(words) <> ''),
		channel VARCHAR(16) NOT NULL CHECK(channel IN ('console','dots','cli')),
		actor VARCHAR(320) NOT NULL CHECK(actor <> ''),
		verified BOOLEAN NOT NULL,
		idempotency_key VARCHAR(128) NOT NULL CHECK(idempotency_key <> ''),
		created_at DATETIME(6) NOT NULL,
		target_lane VARCHAR(255) NOT NULL CHECK(target_lane <> ''),
		acked_at DATETIME(6), acked_by VARCHAR(320), ack_comment_id BIGINT,
		withdrawn_at DATETIME(6), withdrawn_by VARCHAR(320), withdraw_reason LONGTEXT,
		CONSTRAINT task_orders_verified_channel CHECK(verified = (channel <> 'dots')),
		CONSTRAINT task_orders_one_close CHECK(acked_at IS NULL OR withdrawn_at IS NULL),
		CONSTRAINT task_orders_ack_by CHECK((acked_at IS NULL) = (acked_by IS NULL)),
		CONSTRAINT task_orders_withdraw_by CHECK((withdrawn_at IS NULL) = (withdrawn_by IS NULL)),
		UNIQUE KEY task_orders_actor_key(actor, idempotency_key),
		KEY task_orders_task(task_id, id),
		CONSTRAINT task_orders_task_fk FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE RESTRICT
	)` + mysqlTableOptions,
	mysqlIfNoConstraint("notification_outbox", "notification_outbox_kind_check_v2",
		`ALTER TABLE notification_outbox ADD CONSTRAINT notification_outbox_kind_check_v2 CHECK(kind IN (`+mysqlOutboxKindsV2+`))`),
	mysqlIfConstraint("notification_outbox", "notification_outbox_kind_check",
		`ALTER TABLE notification_outbox DROP CHECK notification_outbox_kind_check`),
}

func init() {
	registerMySQLMigration(mysqlMigration{id: taskOrdersMigrationID, after: []string{"20261118-mysql-relay-jobs"}, steps: mysqlTaskOrdersSteps})
}
