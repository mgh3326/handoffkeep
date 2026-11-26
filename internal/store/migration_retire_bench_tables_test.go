package store

import (
	"strings"
	"testing"
)

// benchTableExists reports whether name is a table in the test schema.
func benchTableExists(t *testing.T, s *Store, name string) bool {
	t.Helper()
	var exists bool
	if err := s.pool.QueryRow(t.Context(), `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}

// createRetiredBenchTables creates both retired tables, each with one column,
// so a test can insert a row into either.
func createRetiredBenchTables(t *testing.T, s *Store) {
	t.Helper()
	for _, table := range retiredBenchTables {
		if _, err := s.pool.Exec(t.Context(), `CREATE TABLE `+table+` (id INT)`); err != nil {
			t.Fatal(err)
		}
	}
}

// runRetireBenchTables applies the migration in its own transaction, as the
// runner does, and returns its error.
func runRetireBenchTables(t *testing.T, s *Store) error {
	t.Helper()
	tx, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := applyRetireBenchTables(t.Context(), tx); err != nil {
		return err
	}
	return tx.Commit(t.Context())
}

func TestRetireBenchTablesDropsEmptyTables(t *testing.T) {
	s := schemaStore(t, "retire-bench-empty")
	createRetiredBenchTables(t, s)
	if err := runRetireBenchTables(t, s); err != nil {
		t.Fatalf("empty tables refused: %v", err)
	}
	for _, table := range retiredBenchTables {
		if benchTableExists(t, s, table) {
			t.Errorf("%s survived the migration", table)
		}
	}
}

func TestRetireBenchTablesRefusesAnyRowAndKeepsBoth(t *testing.T) {
	for _, table := range retiredBenchTables {
		t.Run(table, func(t *testing.T) {
			s := schemaStore(t, "retire-bench-rows")
			createRetiredBenchTables(t, s)
			if _, err := s.pool.Exec(t.Context(), `INSERT INTO `+table+` (id) VALUES (1)`); err != nil {
				t.Fatal(err)
			}
			err := runRetireBenchTables(t, s)
			if err == nil || !strings.Contains(err.Error(), table+" holds 1 rows") {
				t.Fatalf("err = %v, want a refusal naming %s and its row count", err, table)
			}
			for _, name := range retiredBenchTables {
				if !benchTableExists(t, s, name) {
					t.Errorf("%s was dropped by a refused migration", name)
				}
			}
		})
	}
}

func TestRetireBenchTablesIsNoOpWhenAbsent(t *testing.T) {
	s := schemaStore(t, "retire-bench-absent")
	for _, table := range retiredBenchTables {
		if benchTableExists(t, s, table) {
			t.Fatalf("%s exists on a fresh install", table)
		}
	}
	if err := runRetireBenchTables(t, s); err != nil {
		t.Fatalf("absent tables refused: %v", err)
	}
}
