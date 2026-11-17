//go:build mysql

package store

import (
	"testing"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestMySQLMigrationResumesAfterPartialDDL(t *testing.T) {
	s, err := Open(t.Context(), testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	migration := mysqlMigration{id: "20261117-test-resume", steps: []string{
		`CREATE TABLE IF NOT EXISTS migration_resume_fixture (id BIGINT PRIMARY KEY)`,
		`INVALID SQL`,
	}}
	if err := migrateMySQL(t.Context(), s.mysqlPool, []mysqlMigration{migration}); err == nil {
		t.Fatal("broken second step succeeded")
	}
	var recorded bool
	if err := s.mysqlPool.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE id=?)`, migration.id).Scan(&recorded); err != nil || recorded {
		t.Fatalf("partial migration recorded=%v err=%v", recorded, err)
	}
	migration.steps[1] = `CREATE TABLE IF NOT EXISTS migration_resume_second (id BIGINT PRIMARY KEY)`
	if err := migrateMySQL(t.Context(), s.mysqlPool, []mysqlMigration{migration}); err != nil {
		t.Fatal(err)
	}
	if err := migrateMySQL(t.Context(), s.mysqlPool, []mysqlMigration{migration}); err != nil {
		t.Fatal(err)
	}
	if err := s.mysqlPool.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE id=?)`, migration.id).Scan(&recorded); err != nil || !recorded {
		t.Fatalf("completed migration recorded=%v err=%v", recorded, err)
	}
}
