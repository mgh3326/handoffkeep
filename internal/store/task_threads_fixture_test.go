package store

import (
	"testing"
	"time"
)

// Raw statements the porter-A5 cases need that no public method offers. The
// PostgreSQL ones are here; the MySQL twins are in
// task_threads_fixture_mysql_test.go (build tag mysql), which fills
// a5MySQLFixtures from init. Without the tag a MySQL store skips the case.
var a5MySQLFixtures struct {
	forceClaim func(t *testing.T, s *Store, taskID int64, key string, claimedAt time.Time)
	setDue     func(t *testing.T, s *Store, taskID int64, at time.Time)
}

// a5ForceClaim marks an active, unclaimed thread row as claimed under key, as the
// claim of a change, deploy or decision line would (those line sources belong
// to other areas).
func a5ForceClaim(t *testing.T, s *Store, taskID int64, key string, claimedAt time.Time) {
	t.Helper()
	if s.dialect == mysqlDialect {
		if a5MySQLFixtures.forceClaim == nil {
			t.Skip("the MySQL raw fixtures are built with -tags mysql")
		}
		a5MySQLFixtures.forceClaim(t, s, taskID, key, claimedAt)
		return
	}
	tag, err := s.pool.Exec(t.Context(), `UPDATE task_thread_deliveries SET claim_key=$2,claimed_at=$3 WHERE task_id=$1 AND state='active' AND claim_key=''`, taskID, key, claimedAt.UTC())
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("force claim %q on task %d: rows=%d err=%v", key, taskID, tag.RowsAffected(), err)
	}
}

// a5SetDue sets a task's due date; no public method does.
func a5SetDue(t *testing.T, s *Store, taskID int64, at time.Time) {
	t.Helper()
	if s.dialect == mysqlDialect {
		if a5MySQLFixtures.setDue == nil {
			t.Skip("the MySQL raw fixtures are built with -tags mysql")
		}
		a5MySQLFixtures.setDue(t, s, taskID, at)
		return
	}
	tag, err := s.pool.Exec(t.Context(), `UPDATE tasks SET due_at=$2 WHERE id=$1`, taskID, at.UTC())
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("set due on task %d: rows=%d err=%v", taskID, tag.RowsAffected(), err)
	}
}
