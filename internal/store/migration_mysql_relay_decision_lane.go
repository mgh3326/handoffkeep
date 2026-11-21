package store

// 20261121-relay-decision-lane indexes the open-decision lane events that
// the operator inbox counts and lists (attentionLaneSQLMySQL, hk 5879). Without
// it the planner scans every lane.event row (about 53k examined per call on
// production) and tests the [decision-needed] prefix on each one; the index
// ranges over the matching events alone. The statement is a functional key on
// relay_events, which the prior migration created with kind and text, so it
// depends on 20261118-mysql-relay-jobs. The key is a 0/1 flag, not the row id:
// MySQL rejects a functional key over the AUTO_INCREMENT id (error 3754). The
// build scans relay_events and takes a
// metadata lock for its duration, pausing writes to it until the index is
// built, as the relay_events index builds in that migration do. Keep the table
// small during this expand step; the rows the index covers are the
// decision-needed lane events only.

func init() {
	registerMySQLMigration(mysqlMigration{id: "20261121-relay-decision-lane", after: []string{"20261118-mysql-relay-jobs"}, steps: []string{
		mysqlIfNoIndex("relay_events", "relay_events_decision_lane", `CREATE INDEX relay_events_decision_lane ON relay_events((`+relayDecisionNeededKeyMySQL+`))`),
	}})
}
