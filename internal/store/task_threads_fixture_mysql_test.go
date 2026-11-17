//go:build mysql

package store

import (
	"testing"
	"time"
)

// The MySQL twins of the raw statements in task_threads_fixture_test.go.
func init() {
	a5MySQLFixtures.forceClaim = func(t *testing.T, s *Store, taskID int64, key string, claimedAt time.Time) {
		t.Helper()
		res, err := s.mysqlPool.ExecContext(t.Context(), `UPDATE task_thread_deliveries SET claim_key=?,claimed_at=? WHERE task_id=? AND state='active' AND claim_key=''`, key, claimedAt.UTC(), taskID)
		if err != nil {
			t.Fatalf("force claim %q on task %d: %v", key, taskID, err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			t.Fatalf("force claim %q on task %d: rows=%d err=%v", key, taskID, n, err)
		}
	}
	// tasks is a synced table: its writes go through the store's transaction so
	// the sync_log rows the trigger writes are numbered at commit.
	a5MySQLFixtures.setDue = func(t *testing.T, s *Store, taskID int64, at time.Time) {
		t.Helper()
		ctx := t.Context()
		tx, release, err := s.begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		defer tx.Rollback(ctx)
		n, err := tx.Exec(ctx, `UPDATE tasks SET due_at=? WHERE id=?`, at.UTC(), taskID)
		if err != nil || n != 1 {
			t.Fatalf("set due on task %d: rows=%d err=%v", taskID, n, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
}
