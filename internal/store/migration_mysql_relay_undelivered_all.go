package store

// 20261124-relay-undelivered-all adds relay_events_undelivered_all
// (delivered_at, id) for the undelivered poll that names neither a lane nor a
// kind, `delivered_at IS NULL ORDER BY id LIMIT ?` (hk 6254, operator decision
// 491). The lookups of 20261121-relay-event-lookups lead with kind or
// owner_lane, so this poll had no usable index: production walked PRIMARY
// through every row: 48,279 rows examined per call to return 84, about 28 ms a
// call, and 311 calls in one 51-minute window.
//
// MySQL has no partial index, so the NULL in delivered_at is the key: a lookup
// on NULL reads only the undelivered entries, in id order, and LIMIT stops the
// scan. With an `id>?` cursor the same index serves a range. id is named
// although InnoDB appends the primary key anyway, so the order the poll needs
// is visible in the definition.
//
// Expand-only: one secondary index, no column, table or constraint change.
// The previous release's reads are unchanged; its plans can only gain an index.
// Write cost: each insert adds one entry, and the delivery update moves its
// row's entry from NULL to the delivery time. Locks: InnoDB builds the index
// online, so reads and writes continue; the statement takes a short metadata
// lock at its start and end. mysqlIfNoIndex runs it only while the index is
// absent, so the step is restartable. It needs delivered_at, which
// 20261118-mysql-relay-jobs adds to the relay_events stub.
var mysqlRelayUndeliveredAllSteps = []string{
	mysqlIfNoIndex("relay_events", "relay_events_undelivered_all", `CREATE INDEX relay_events_undelivered_all ON relay_events(delivered_at, id)`),
}

func init() {
	registerMySQLMigration(mysqlMigration{id: "20261124-relay-undelivered-all", after: []string{"20261118-mysql-relay-jobs"}, steps: mysqlRelayUndeliveredAllSteps})
}
