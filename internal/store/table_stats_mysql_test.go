//go:build mysql

package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// TestTableStatsMySQLReadsExactCountsAndRestoresTheSession reads the shared
// schema, so it asserts only on its own uniquely named table: an exact count
// it created, the fresh flag, and that the read left the stats-expiry setting
// at its global default on the pooled connection. It does not write the
// report document, whose key is fixed.
func TestTableStatsMySQLReadsExactCountsAndRestoresTheSession(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	name := fmt.Sprintf("tstats_%d", time.Now().UnixNano())
	if _, err := s.mysqlPool.ExecContext(ctx, "CREATE TABLE "+quoteMySQLIdent(name)+" (id BIGINT PRIMARY KEY) ENGINE=InnoDB"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.mysqlPool.Exec("DROP TABLE IF EXISTS " + quoteMySQLIdent(name))
	})
	for i := 1; i <= 3; i++ {
		if _, err := s.mysqlPool.ExecContext(ctx, "INSERT INTO "+quoteMySQLIdent(name)+" (id) VALUES (?)", i); err != nil {
			t.Fatal(err)
		}
	}

	read, err := s.readTableStatsMySQL(ctx)
	if err != nil {
		t.Fatalf("readTableStatsMySQL: %v", err)
	}
	got, ok := read.tables[name]
	if !ok {
		t.Fatalf("snapshot does not list %s", name)
	}
	if got[0] != 3 {
		t.Fatalf("exact rows for %s = %d, want 3", name, got[0])
	}
	if !read.fresh {
		t.Fatal("snapshot reports stale statistics; the server accepted the setting, so fresh must be true")
	}
	if read.elapsed <= 0 {
		t.Fatal("snapshot elapsed time was not recorded")
	}

	// The setting must be back at the global default on the connection the
	// pool hands out next.
	var global, session int64
	if err := s.mysqlPool.QueryRowContext(ctx, "SELECT @@GLOBAL.information_schema_stats_expiry").Scan(&global); err != nil {
		t.Fatal(err)
	}
	conn, err := s.mysqlPool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.QueryRowContext(ctx, "SELECT @@SESSION.information_schema_stats_expiry").Scan(&session); err != nil {
		t.Fatal(err)
	}
	if session != global {
		t.Fatalf("session information_schema_stats_expiry = %d after the snapshot, want the global default %d", session, global)
	}
}
