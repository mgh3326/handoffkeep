package store

// The MySQL twin of 20261125-dots-note-outbox-kind (hk 6307): the
// notification_outbox kind CHECK admits 'dots_note'. It follows
// 20261118-lane-check, whose v4 constraint it widens.
//
// MySQL cannot swap a CHECK in one statement and its DDL commits step by step,
// so the swap is restartable (as in the lane-check twin): the wider constraint
// is added under a new name first, while the old one still binds, then the
// old one is dropped when it is still there. The previous release never
// writes 'dots_note', and the wider list admits every kind it does write.
const mysqlOutboxKindsV5 = mysqlOutboxKindsV4 + `,'dots_note'`

var mysqlDotsNoteKindSteps = []string{
	mysqlIfNoConstraint("notification_outbox", "notification_outbox_kind_check_v5",
		`ALTER TABLE notification_outbox ADD CONSTRAINT notification_outbox_kind_check_v5 CHECK(kind IN (`+mysqlOutboxKindsV5+`))`),
	mysqlIfConstraint("notification_outbox", "notification_outbox_kind_check_v4",
		`ALTER TABLE notification_outbox DROP CHECK notification_outbox_kind_check_v4`),
}

func init() {
	registerMySQLMigration(mysqlMigration{id: dotsNoteKindMigrationID, after: []string{laneCheckMigrationID}, steps: mysqlDotsNoteKindSteps})
}
