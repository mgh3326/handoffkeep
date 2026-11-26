//go:build mysql

package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// The MySQL twins of the PostgreSQL tests in decision_settle_withdraw_test.go.
// The bodies are shared (decision_settle_test.go); only the store and the
// backfill's execution differ. Each store opens on its own database.

func TestMySQLSettleWithdrawsOpenDecisionRequest(t *testing.T) {
	s, err := Open(t.Context(), testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, to := range []string{StateMerged, StateCompleted, StateDropped} {
		t.Run(to, func(t *testing.T) {
			settleWithdrawCase(t, s, to)
		})
	}
}

func TestMySQLSettleKeepsClosedDecisionRequest(t *testing.T) {
	s, err := Open(t.Context(), testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	settleKeepsCase(t, s)
}

func TestMySQLSettleClearsFlagOnlyDecision(t *testing.T) {
	s, err := Open(t.Context(), testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	settleFlagOnlyCase(t, s)
}

// TestMySQLSettledOpenDecisionBackfill runs the MySQL steps of the registered
// migration, in one transaction as the migrator does, and checks the same
// result as the PostgreSQL test, including that a second run changes nothing.
func TestMySQLSettledOpenDecisionBackfill(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	settled, unsettledReq := settledBackfillSetup(t, s)

	var steps []string
	for _, m := range mysqlMigrations {
		if m.id == settledBackfillID {
			steps = m.steps
		}
	}
	if len(steps) == 0 {
		t.Fatalf("MySQL migration %s is not registered", settledBackfillID)
	}
	run := func() {
		tx, release, err := s.begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		defer tx.Rollback(ctx)
		for _, step := range steps {
			if _, err := tx.Exec(ctx, step); err != nil {
				t.Fatalf("backfill step: %v", err)
			}
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
		if x.AwaitingDecision {
			t.Fatalf("task %d still awaits a decision after backfill", id)
		}
		if status, by := settleInboxRow(t, s, r.ID); status != "superseded" || by != DecisionRequestWithdrawn {
			t.Fatalf("task %d inbox status=%s superseded_by=%s", id, status, by)
		}
	}
	if status, _ := settleInboxRow(t, s, unsettledReq); status != "open" {
		t.Fatalf("unsettled request %s inbox status=%s, want open", unsettledReq, status)
	}

	before := settledSnapshotMySQL(t, s)
	run()
	if after := settledSnapshotMySQL(t, s); fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("second run changed rows:\nbefore=%v\nafter=%v", before, after)
	}
}

// settledSnapshotMySQL reads every task's refs and every inbox row's status,
// superseded_by and updated_at, as backfillSnapshot does on PostgreSQL.
func settledSnapshotMySQL(t *testing.T, s *Store) []string {
	t.Helper()
	ctx := context.Background()
	var out []string
	for _, q := range []string{
		`SELECT CONCAT(id, ' ', refs) FROM tasks ORDER BY id`,
		`SELECT CONCAT(id, ' ', status, ' ', superseded_by, ' ', updated_at) FROM decision_requests ORDER BY id`,
	} {
		rows, err := s.db().Query(ctx, q)
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
