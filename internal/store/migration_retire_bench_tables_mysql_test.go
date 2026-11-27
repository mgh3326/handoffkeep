//go:build mysql

package store

import (
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// mysqlRetireTableExists reports whether name is a table in the test database.
func mysqlRetireTableExists(t *testing.T, s *Store, name string) bool {
	t.Helper()
	var exists bool
	if err := s.mysqlPool.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?)`, name).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}

// runMySQLRetireBenchTables runs only this migration, as the runner does on a
// database that has not recorded it, and returns the runner's error.
func runMySQLRetireBenchTables(t *testing.T, s *Store) error {
	t.Helper()
	if _, err := s.mysqlPool.ExecContext(t.Context(), `DELETE FROM schema_migrations WHERE id=?`, RetireBenchTablesMigration); err != nil {
		t.Fatal(err)
	}
	return migrateMySQL(t.Context(), s.mysqlPool, []mysqlMigration{mysqlRetireBenchTables})
}

func TestMySQLRetireBenchTablesDropsEmptyTables(t *testing.T) {
	s, err := Open(t.Context(), testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	for _, table := range retiredBenchTables {
		if _, err := s.mysqlPool.ExecContext(t.Context(), `CREATE TABLE `+table+` (id INT)`); err != nil {
			t.Fatal(err)
		}
	}
	if err := runMySQLRetireBenchTables(t, s); err != nil {
		t.Fatalf("empty tables refused: %v", err)
	}
	for _, table := range retiredBenchTables {
		if mysqlRetireTableExists(t, s, table) {
			t.Errorf("%s survived the migration", table)
		}
	}
}

func TestMySQLRetireBenchTablesRefusesAnyRowAndKeepsBoth(t *testing.T) {
	for _, table := range retiredBenchTables {
		t.Run(table, func(t *testing.T) {
			s, err := Open(t.Context(), testdb.MySQLDatabase(t))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(s.Close)
			for _, name := range retiredBenchTables {
				if _, err := s.mysqlPool.ExecContext(t.Context(), `CREATE TABLE `+name+` (id INT)`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.mysqlPool.ExecContext(t.Context(), `INSERT INTO `+table+` (id) VALUES (1)`); err != nil {
				t.Fatal(err)
			}
			err = runMySQLRetireBenchTables(t, s)
			if err == nil || !strings.Contains(err.Error(), table+" holds 1 rows") {
				t.Fatalf("err = %v, want a refusal naming %s and its row count", err, table)
			}
			for _, name := range retiredBenchTables {
				if !mysqlRetireTableExists(t, s, name) {
					t.Errorf("%s was dropped by a refused migration", name)
				}
			}
			// Once the rows are gone the same migration runs to completion: a
			// refusal is restartable.
			if _, err := s.mysqlPool.ExecContext(t.Context(), `DELETE FROM `+table); err != nil {
				t.Fatal(err)
			}
			if err := runMySQLRetireBenchTables(t, s); err != nil {
				t.Fatalf("rerun after emptying %s: %v", table, err)
			}
			for _, name := range retiredBenchTables {
				if mysqlRetireTableExists(t, s, name) {
					t.Errorf("%s survived the rerun", name)
				}
			}
		})
	}
}

func TestMySQLRetireBenchTablesIsNoOpWhenAbsent(t *testing.T) {
	s, err := Open(t.Context(), testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	for _, table := range retiredBenchTables {
		if mysqlRetireTableExists(t, s, table) {
			t.Fatalf("%s exists on a fresh install", table)
		}
	}
	if err := runMySQLRetireBenchTables(t, s); err != nil {
		t.Fatalf("absent tables refused: %v", err)
	}
}
