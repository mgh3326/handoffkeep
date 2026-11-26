package store

// 20261127-outbox-unsent-poll-order lets the MySQL hub poll preserve its
// durable id order and stop at LIMIT; the range index added by
// 20261125-outbox-unsent-poll leads with created_at and requires a filesort.
// MySQL has no partial index, so the two NULL state columns lead and id is the
// ordering key; created_at remains a residual predicate for this poll. The
// registry migration is additive. InnoDB builds this secondary index online
// (ALGORITHM=INPLACE, LOCK=NONE), taking brief metadata locks at start/end and
// scanning the sub-1 MB outbox without blocking its writes.
var mysqlOutboxUnsentPollOrderSteps = []string{
	mysqlIfNoIndex("notification_outbox", "notification_outbox_unsent_poll_order", `CREATE INDEX notification_outbox_unsent_poll_order ON notification_outbox(sent_at, skipped_at, id) ALGORITHM=INPLACE LOCK=NONE`),
}

func init() {
	registerMySQLMigration(mysqlMigration{id: "20261127-outbox-unsent-poll-order", after: []string{"20261125-outbox-unsent-poll"}, steps: mysqlOutboxUnsentPollOrderSteps})
}
