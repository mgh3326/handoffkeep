//go:build mysql

package store

import (
	"testing"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestMySQLTransactionTryLock(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	first, err := s.db().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(ctx)
	second, err := s.db().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Rollback(ctx)
	key := mysqlLockKey("id", "8241873997")
	if held, err := mysqlTransactionLock(ctx, first, key, true); err != nil || !held {
		t.Fatalf("first try: held=%v err=%v", held, err)
	}
	if held, err := mysqlTransactionLock(ctx, second, key, true); err != nil || held {
		t.Fatalf("contended try: held=%v err=%v", held, err)
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if held, err := mysqlTransactionLock(ctx, second, key, true); err != nil || !held {
		t.Fatalf("try after commit: held=%v err=%v", held, err)
	}
}
