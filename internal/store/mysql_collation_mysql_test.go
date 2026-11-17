//go:build mysql

package store

import (
	"database/sql"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/mysqlurl"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// The store needs utf8mb4_0900_bin everywhere (design §11.4). The B1b stub of
// `tasks` had no explicit collation, so on a database whose default is
// case-insensitive its text columns would inherit that default: the tasks
// source_ref unique key would treat "ABC" and "abc" as one reference. This test
// gives the database a case-insensitive default before the first Open and checks
// that no migrated column ends up with another collation.
func TestMySQLEveryColumnIsBinaryCollationOnACaseInsensitiveDatabase(t *testing.T) {
	url := testdb.MySQLDatabase(t)
	cfg, err := mysqlurl.Parse(url)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE `"+cfg.DBName+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	rows, err := s.mysqlPool.QueryContext(t.Context(), `SELECT table_name,column_name,collation_name FROM information_schema.columns WHERE table_schema=DATABASE() AND collation_name IS NOT NULL AND collation_name<>'utf8mb4_0900_bin' ORDER BY table_name,column_name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, column, collation string
		if err := rows.Scan(&table, &column, &collation); err != nil {
			t.Fatal(err)
		}
		t.Errorf("%s.%s has collation %s, want utf8mb4_0900_bin", table, column, collation)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// The visible effect: two source references that differ only in case are two keys.
	for _, ref := range []string{"ABC", "abc"} {
		if err := s.mysqlWriteTransaction(t.Context(), func(tx transaction) error {
			_, err := tx.Exec(t.Context(), `INSERT INTO tasks(lane,title,kind,state,created_by,created_at,updated_at,source,source_ref) VALUES('lane','t','implement','backlog','test',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6),'collation',?)`, ref)
			return err
		}); err != nil {
			t.Fatalf("source_ref %q: %v", ref, err)
		}
	}
}
