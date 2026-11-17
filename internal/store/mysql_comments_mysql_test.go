//go:build mysql

package store

import (
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestMySQLCommentGuards(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if _, err := s.mysqlPool.ExecContext(ctx, `INSERT INTO tasks(id) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
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
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_log`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("bulk load emitted sync rows: count=%d err=%v", count, err)
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
		{"quota_history_immutable_update", `UPDATE quota_observations SET window_key='7d' WHERE id=?`},
		{"quota_history_immutable_delete", `DELETE FROM quota_observations WHERE id=?`},
	} {
		t.Run(tc.trigger, func(t *testing.T) {
			ctx := t.Context()
			s, err := Open(ctx, testdb.MySQLDatabase(t))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(s.Close)
			if _, err := s.mysqlPool.ExecContext(ctx, `INSERT INTO tasks(id) VALUES (1)`); err != nil {
				t.Fatal(err)
			}
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
