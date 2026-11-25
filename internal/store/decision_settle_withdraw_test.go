package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const settledBackfillID = "20261125-settled-open-decision-withdraw"

// TestSettleWithdrawsOpenDecisionRequest: a task with an open request that
// settles (merged, completed, dropped) has the request withdrawn with the
// reason, its inbox row superseded, and drops out of drift's
// settled_open_decision.
func TestSettleWithdrawsOpenDecisionRequest(t *testing.T) {
	s, _ := searchTestStore(t)
	for _, to := range []string{StateMerged, StateCompleted, StateDropped} {
		t.Run(to, func(t *testing.T) {
			settleWithdrawCase(t, s, to)
		})
	}
}

// TestSettleKeepsClosedDecisionRequest: an answered request is not changed by
// settling its task.
func TestSettleKeepsClosedDecisionRequest(t *testing.T) {
	s, _ := searchTestStore(t)
	settleKeepsCase(t, s)
}

// TestSettledOpenDecisionBackfill: the registered migration withdraws the open
// request of each task already settled, supersedes its open inbox row, leaves
// an unsettled task alone, and a second run changes nothing.
func TestSettledOpenDecisionBackfill(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	settled, unsettledReq := settledBackfillSetup(t, s)

	var m migration
	found := false
	for _, r := range migrationRegistry {
		if r.ID == settledBackfillID {
			m, found = r, true
		}
	}
	if !found {
		t.Fatalf("migration %s is not registered", settledBackfillID)
	}
	run := func() {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err := m.Apply(ctx, tx); err != nil {
			t.Fatalf("backfill: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	run()
	for _, id := range settled {
		x, _, err := s.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		r := x.Refs.DecisionRequest
		if r == nil || r.Status != DecisionRequestWithdrawn || r.WithdrawnReason != DecisionWithdrawnTaskSettled {
			t.Fatalf("task %d after backfill request=%+v", id, r)
		}
		if status, by := settleInboxRow(t, s, r.ID); status != "superseded" || by != DecisionRequestWithdrawn {
			t.Fatalf("task %d inbox status=%s superseded_by=%s", id, status, by)
		}
	}
	if status, _ := settleInboxRow(t, s, unsettledReq); status != "open" {
		t.Fatalf("unsettled request %s inbox status=%s, want open", unsettledReq, status)
	}

	before := backfillSnapshot(t, pool)
	run()
	if after := backfillSnapshot(t, pool); fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("second run changed rows:\nbefore=%v\nafter=%v", before, after)
	}
}

// backfillSnapshot reads every task's refs and every inbox row's status,
// superseded_by and updated_at, so a run that changed anything shows up as a
// difference.
func backfillSnapshot(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	ctx := context.Background()
	var out []string
	for _, q := range []string{
		`SELECT id::text || ' ' || refs::text FROM tasks ORDER BY id`,
		`SELECT id::text || ' ' || status || ' ' || superseded_by || ' ' || updated_at::text FROM decision_requests ORDER BY id`,
	} {
		rows, err := pool.Query(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			out = append(out, line)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	return out
}
