package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261121-relay-decision-lane is the PostgreSQL side of the open-decision lane
// index (hk 5879). Its MySQL counterpart carries the same ID, in
// migration_mysql_relay_decision_lane.go, as the registry requires.
// The partial index matches openLaneDecisionSQL's predicate exactly, so the
// planner reads only the [decision-needed] lane events. It runs in the
// migrating transaction, so its build takes SHARE on relay_events for the scan;
// the table is the lane and job event log, and the index covers only the
// decision-needed rows.
func init() {
	registerMigration(migration{ID: "20261121-relay-decision-lane", Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, []string{
			`CREATE INDEX IF NOT EXISTS relay_events_decision_lane ON relay_events(id) WHERE kind='lane.event' AND text LIKE '[decision-needed]%'`,
		})
	}})
}
