package store

// 20261121-relay-decision-lane indexes the open-decision lane events that
// the operator inbox counts and lists (attentionLaneSQLMySQL, hk 5879). Without
// it the planner reads every lane.event row (about 53k examined per call on
// production, digest samples 8 and 13) and tests the [decision-needed] prefix on
// each one. The statement is a functional key on relay_events, which the prior
// migration created with kind and text, so it depends on
// 20261118-mysql-relay-jobs. The key is a 0/1 flag, not the row id: MySQL rejects
// a functional key over the AUTO_INCREMENT id (error 3754).
//
// MySQL stores the flag for every relay_events row, not only the
// decision-needed ones: a functional key has no partial form (the PostgreSQL
// partial index in migration_relay_decision_lane.go does). A read of flag = 1
// still ranges over only the matching entries, so the plan is right, but the
// index is as large as the table. The build is online for InnoDB: concurrent
// reads and writes continue. The statement takes a metadata lock at its start
// and end, and a long transaction on relay_events can delay that lock. Writes
// do not pause for the whole build; an earlier comment said they did, and that
// was not verified.

func init() {
	registerMySQLMigration(mysqlMigration{id: "20261121-relay-decision-lane", after: []string{"20261118-mysql-relay-jobs"}, steps: []string{
		mysqlIfNoIndex("relay_events", "relay_events_decision_lane", `CREATE INDEX relay_events_decision_lane ON relay_events((`+relayDecisionNeededKeyMySQL+`))`),
	}})
}
