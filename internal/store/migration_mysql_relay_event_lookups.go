package store

// 20261121-relay-event-lookups adds three secondary indexes on relay_events for
// the three reads that scan it on MySQL (hk 5928; digests in
// findings/2026-11-21/hk5584-mysql-digests, samples 5, 6 and 16).
//
// relay_events_lane_lookup (kind, owner_lane, event_id) serves the lane-event
// idempotency read in appendRelayEventOnce, `owner_lane=? AND event_id=? AND
// kind='lane.event' FOR UPDATE`. The existing functional unique key
// relay_events_lane_event_idempotency is over CASE expressions, which this
// predicate does not match, so the read used relay_events_owner_lane and
// examined every row of the lane (about 7.9k per call). The three equality
// columns make the read a single key lookup. Its FOR UPDATE locks only the
// matched entry, not the whole lane. A missing key takes a gap lock on the
// neighbouring entries for the transaction; that is short, and the
// relay-key MySQL transaction lock already serializes appends for one key.
//
// relay_events_undelivered_kind (kind, delivered_at) and
// relay_events_undelivered_owner (owner_lane, delivered_at) serve the
// undelivered polls, `kind=? AND delivered_at IS NULL ORDER BY id LIMIT ?` and
// `owner_lane=? AND delivered_at IS NULL ORDER BY id LIMIT ?`. MySQL has no
// partial index, so the NULL in delivered_at is the key: a lookup on NULL
// reads only the undelivered entries of the prefix, and InnoDB appends the
// primary key, so they come out in id order and LIMIT stops the scan. The
// older (kind, id) and (owner_lane) indexes walk every delivered row first.
//
// Expand-only: three secondary indexes, no column, table or constraint change.
// The previous release's reads are unchanged; its plans can only gain an index,
// and relay_events writes maintain three more keys. relay_events holds the lane
// and job event log, which is small next to tasks and yt_write_ops. Locks:
// InnoDB builds each secondary index online, so reads and writes continue; the
// statement takes a short metadata lock at its start and end. Each step is
// restartable: mysqlIfNoIndex runs it only while the index is absent.
var mysqlRelayEventLookupSteps = []string{
	mysqlIfNoIndex("relay_events", "relay_events_lane_lookup", `CREATE INDEX relay_events_lane_lookup ON relay_events(kind, owner_lane, event_id)`),
	mysqlIfNoIndex("relay_events", "relay_events_undelivered_kind", `CREATE INDEX relay_events_undelivered_kind ON relay_events(kind, delivered_at)`),
	mysqlIfNoIndex("relay_events", "relay_events_undelivered_owner", `CREATE INDEX relay_events_undelivered_owner ON relay_events(owner_lane, delivered_at)`),
}

func init() {
	registerMySQLMigration(mysqlMigration{id: "20261121-relay-event-lookups", after: []string{"20261118-mysql-relay-jobs"}, steps: mysqlRelayEventLookupSteps})
}
