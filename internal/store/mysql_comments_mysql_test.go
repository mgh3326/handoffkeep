//go:build mysql

package store

import (
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestMySQLCommentGuards(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	insertMySQLTestTask(t, s)
	var id int64
	if err := s.mysqlWriteTransaction(ctx, func(tx transaction) error {
		var err error
		id, err = mysqlInsertTaskCommentTx(ctx, tx, 1, "author", "body", time.Now().UTC())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{`UPDATE task_comments SET body='changed' WHERE id=?`, `DELETE FROM task_comments WHERE id=?`} {
		if _, err := s.mysqlPool.ExecContext(ctx, query, id); err == nil {
			t.Errorf("guard accepted %s", query)
		}
	}
	if _, err := s.mysqlPool.ExecContext(ctx, `DELETE FROM tasks WHERE id=1`); err == nil {
		t.Error("task delete bypassed comment guard through cascade")
	}
	conn, err := s.mysqlPool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var before int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_log`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `SET @hk_bulk_load=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE task_comments SET body='copy' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM task_comments WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `SET @hk_bulk_load=NULL`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_log`).Scan(&count); err != nil || count != before {
		t.Fatalf("bulk load emitted sync rows: count=%d before=%d err=%v", count, before, err)
	}
}

func TestMySQLNamedLockExcludesTransaction(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	lease, held, err := s.TryYouTrackSyncLock(ctx)
	if err != nil || !held {
		t.Fatalf("session lock: held=%t err=%v", held, err)
	}
	called := false
	acquired, err := s.mysqlWriteTransactionWithLock(ctx, mysqlYTSyncLock, func(transaction) error { called = true; return nil })
	if err != nil || acquired || called {
		t.Fatalf("contended transaction: acquired=%t called=%t err=%v", acquired, called, err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatal(err)
	}
	acquired, err = s.mysqlWriteTransactionWithLock(ctx, mysqlYTSyncLock, func(transaction) error { called = true; return nil })
	if err != nil || !acquired || !called {
		t.Fatalf("after release: acquired=%t called=%t err=%v", acquired, called, err)
	}
}

// Dropping each guard is the mutant: the formerly refused mutation must then
// succeed, proving the normal guard tests would catch a missing trigger.
func TestMySQLGuardMutants(t *testing.T) {
	for _, tc := range []struct{ trigger, mutation string }{
		{"task_comments_guard_update", `UPDATE task_comments SET body='mutant' WHERE id=?`},
		{"task_comments_guard_delete", `DELETE FROM task_comments WHERE id=?`},
		{"quota_history_bulk_update", `UPDATE quota_observations SET window_key='7d' WHERE id=?`},
		{"quota_history_bulk_delete", `DELETE FROM quota_observations WHERE id=?`},
	} {
		t.Run(tc.trigger, func(t *testing.T) {
			ctx := t.Context()
			s, err := Open(ctx, testdb.MySQLDatabase(t))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(s.Close)
			insertMySQLTestTask(t, s)
			var commentID int64
			if err := s.mysqlWriteTransaction(ctx, func(tx transaction) error {
				var err error
				commentID, err = mysqlInsertTaskCommentTx(ctx, tx, 1, "author", "body", time.Now().UTC())
				return err
			}); err != nil {
				t.Fatal(err)
			}
			res, err := s.mysqlPool.ExecContext(ctx, `INSERT INTO quota_observations(account_key,kind,window_key,measured_at,value) VALUES('test','auto','5h',?,JSON_OBJECT())`, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			quotaID, err := res.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			id := quotaID
			if tc.trigger == "task_comments_guard_update" || tc.trigger == "task_comments_guard_delete" {
				id = commentID
			}
			if err := s.mysqlWriteTransaction(ctx, func(tx transaction) error {
				_, err := tx.Exec(ctx, tc.mutation, id)
				return err
			}); err == nil {
				t.Fatal("unmutated guard accepted change")
			}
			if _, err := s.mysqlPool.ExecContext(ctx, `DROP TRIGGER `+tc.trigger); err != nil {
				t.Fatal(err)
			}
			if err := s.mysqlWriteTransaction(ctx, func(tx transaction) error {
				_, err := tx.Exec(ctx, tc.mutation, id)
				return err
			}); err != nil {
				t.Fatalf("mutant still refused change: %v", err)
			}
		})
	}
}

func TestMySQLWorkerSessionLock(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	lease, held, err := s.TryWorkerLock(ctx, HubForwardWorkerLock)
	if err != nil || !held {
		t.Fatalf("first worker: held=%t err=%v", held, err)
	}
	if _, held, err := s.TryWorkerLock(ctx, HubForwardWorkerLock); err != nil || held {
		t.Fatalf("second worker: held=%t err=%v", held, err)
	}
	lease.Release()
	again, held, err := s.TryWorkerLock(ctx, HubForwardWorkerLock)
	if err != nil || !held {
		t.Fatalf("after release: held=%t err=%v", held, err)
	}
	again.Release()
}

func TestMySQLTransactionRetryOnce(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	for _, code := range []uint16{1205, 1213} {
		calls := 0
		err := s.mysqlWriteTransaction(ctx, func(transaction) error {
			calls++
			return &mysql.MySQLError{Number: code, Message: "test retry"}
		})
		if err == nil || calls != 2 {
			t.Errorf("error %d: calls=%d err=%v", code, calls, err)
		}
	}
}

// insertMySQLTestTask inserts task 1, the comment tests' foreign-key target,
// through the write-transaction wrapper: the tasks emit trigger needs @hk_txn.
// It returns the largest sync_id then assigned, so a test that follows the feed
// starts after the task's own row.
func insertMySQLTestTask(t *testing.T, s *Store) int64 {
	t.Helper()
	err := s.mysqlWriteTransaction(t.Context(), func(tx transaction) error {
		_, err := tx.Exec(t.Context(), `INSERT INTO tasks(id,lane,title,kind,state,created_by,created_at,updated_at) VALUES (1,'lane','task','implement','backlog','test',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var max int64
	if err := s.mysqlPool.QueryRowContext(t.Context(), `SELECT max_sync_id FROM sync_state WHERE singleton=1`).Scan(&max); err != nil {
		t.Fatal(err)
	}
	return max
}
