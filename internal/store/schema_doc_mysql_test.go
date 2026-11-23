//go:build mysql

package store

import (
	"context"
	"os"
	"testing"
)

// TestSchemaDocMigrate builds the MySQL schema to head through the production
// migration path (Open on a mysql:// URL) for the schema-doc workflow
// (.github/workflows/schema-doc.yml, hk 6106). It is gated by
// HANDOFFKEEP_SCHEMA_DOC_MYSQL_URL, which only that workflow sets, so it skips
// everywhere else. It leaves the migrated schema in place for tbls, and writes
// SELECT VERSION() to HANDOFFKEEP_SCHEMA_DOC_VERSION_FILE when that is set, so
// the document states the engine it was built on.
func TestSchemaDocMigrate(t *testing.T) {
	rawURL := os.Getenv("HANDOFFKEEP_SCHEMA_DOC_MYSQL_URL")
	if rawURL == "" {
		t.Skip("HANDOFFKEEP_SCHEMA_DOC_MYSQL_URL is unset; this test runs only in the schema-doc workflow")
	}
	ctx := context.Background()
	s, err := Open(ctx, rawURL)
	if err != nil {
		t.Fatalf("Open migrated the MySQL schema: %v", err)
	}
	defer s.Close()

	var recorded int
	if err := s.mysqlPool.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&recorded); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if recorded != len(mysqlMigrations) {
		t.Fatalf("recorded migrations = %d, want %d (the registry)", recorded, len(mysqlMigrations))
	}

	var version string
	if err := s.mysqlPool.QueryRowContext(ctx, `SELECT VERSION()`).Scan(&version); err != nil {
		t.Fatalf("SELECT VERSION(): %v", err)
	}
	t.Logf("MySQL server version: %s", version)
	if path := os.Getenv("HANDOFFKEEP_SCHEMA_DOC_VERSION_FILE"); path != "" {
		if err := os.WriteFile(path, []byte(version), 0o644); err != nil {
			t.Fatalf("write version file: %v", err)
		}
	}
}
