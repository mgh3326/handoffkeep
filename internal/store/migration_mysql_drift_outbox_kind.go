package store

// The MySQL twin of 20261118-drift-outbox-kind (hk 5112 track B-3), the twin
// that admits 'task_drift' to notification_outbox, whose kind CHECK the
// 20261118-task-orders twin widened to v2.
//
// MySQL cannot swap a CHECK in one statement and its DDL commits step by step,
// so the swap is restartable instead: the wider constraint is added under a
// new name first (the two coexist, the old one still binding), then the old
// one is dropped when it is still there. CREATE TABLE locks only the new
// table.
const mysqlOutboxKindsV3 = `'decision_answered','chat_answer','dots_report','dots_decision','dots_blocked','dots_followup','yt_write_op','decision_denied','disposition_answered','deploy_result','deploy_lag','task_order','task_drift'`

func init() {
	registerMySQLMigration(mysqlMigration{
		id:    driftOutboxKindMigrationID,
		after: []string{taskOrdersMigrationID},
		steps: []string{
			mysqlIfNoConstraint("notification_outbox", "notification_outbox_kind_check_v3",
				`ALTER TABLE notification_outbox ADD CONSTRAINT notification_outbox_kind_check_v3 CHECK(kind IN (`+mysqlOutboxKindsV3+`))`),
			mysqlIfConstraint("notification_outbox", "notification_outbox_kind_check_v2",
				`ALTER TABLE notification_outbox DROP CHECK notification_outbox_kind_check_v2`),
		},
	})
}
