package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestPlaneV16AdditiveAndIdempotent brings a pre-v16 schema forward and
// proves the migration creates the mirror tables once, without rebuilding
// them on a second run.
func TestPlaneV16AdditiveAndIdempotent(t *testing.T) {
	url := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if url == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for PostgreSQL migration tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("plane_v16_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE") }()

	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err = pool.Exec(ctx, `CREATE TABLE schema_version (version INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	// Seed versions through 15 with the minimal tasks table v16's foreign key
	// needs; the migration gate then applies only the v16 statements.
	if _, err = pool.Exec(ctx, `INSERT INTO schema_version(version) SELECT unnest(ARRAY[1,2,3,4,5,6,7,9,10,11,12,13,14,15])`); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `CREATE TABLE tasks (id BIGSERIAL PRIMARY KEY, lane TEXT NOT NULL, parent_lane TEXT NOT NULL DEFAULT '', title TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('implement','verify','fix','decide','ops')), state TEXT NOT NULL CHECK(state IN ('backlog','claimed','in_progress','verifying','join','hold','needs_decision','merged','dropped')), priority INTEGER NOT NULL DEFAULT 0, refs JSONB NOT NULL DEFAULT '{}'::jsonb, claimed_by TEXT NOT NULL DEFAULT '', created_by TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL)`); err != nil {
		t.Fatal(err)
	}

	s := &Store{pool: pool}
	if err = s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var version16 bool
	if err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_version WHERE version=16)`).Scan(&version16); err != nil {
		t.Fatal(err)
	}
	if !version16 {
		t.Fatal("schema_version is missing version 16")
	}
	var outboxOID, issuesOID, uniqueOID uint32
	if err = pool.QueryRow(ctx, `SELECT 'plane_outbox'::regclass::oid`).Scan(&outboxOID); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT 'plane_issues'::regclass::oid`).Scan(&issuesOID); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT oid FROM pg_constraint WHERE conrelid='plane_outbox'::regclass AND contype='u'`).Scan(&uniqueOID); err != nil {
		t.Fatal(err)
	}
	var dryrunAllowed bool
	if err = pool.QueryRow(ctx, `SELECT COUNT(*) > 0 FROM pg_constraint WHERE conrelid='plane_outbox'::regclass AND contype='c' AND pg_get_constraintdef(oid) LIKE '%dryrun%'`).Scan(&dryrunAllowed); err != nil {
		t.Fatal(err)
	}
	if !dryrunAllowed {
		t.Fatal("plane_outbox state CHECK does not admit 'dryrun'")
	}
	if err = s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var repeatedOutboxOID, repeatedIssuesOID, repeatedUniqueOID uint32
	if err = pool.QueryRow(ctx, `SELECT 'plane_outbox'::regclass::oid`).Scan(&repeatedOutboxOID); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT 'plane_issues'::regclass::oid`).Scan(&repeatedIssuesOID); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT oid FROM pg_constraint WHERE conrelid='plane_outbox'::regclass AND contype='u'`).Scan(&repeatedUniqueOID); err != nil {
		t.Fatal(err)
	}
	if repeatedOutboxOID != outboxOID || repeatedIssuesOID != issuesOID || repeatedUniqueOID != uniqueOID {
		t.Fatalf("migration rebuilt objects: %d/%d %d/%d %d/%d", outboxOID, repeatedOutboxOID, issuesOID, repeatedIssuesOID, uniqueOID, repeatedUniqueOID)
	}
}
