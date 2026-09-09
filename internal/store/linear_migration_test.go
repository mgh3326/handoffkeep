package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLinearV10AdditiveAndIdempotent(t *testing.T) {
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
	schema := fmt.Sprintf("linear_v10_%d", time.Now().UnixNano())
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
	if _, err = pool.Exec(ctx, `INSERT INTO schema_version(version) SELECT unnest(ARRAY[1,2,3,4,5,6,7,9])`); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `CREATE TABLE tasks (id BIGSERIAL PRIMARY KEY, lane TEXT NOT NULL, parent_lane TEXT NOT NULL DEFAULT '', title TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('implement','verify','fix','decide','ops')), state TEXT NOT NULL CHECK(state IN ('backlog','claimed','in_progress','verifying','join','hold','needs_decision','merged','dropped')), priority INTEGER NOT NULL DEFAULT 0, refs JSONB NOT NULL DEFAULT '{}'::jsonb, claimed_by TEXT NOT NULL DEFAULT '', created_by TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL)`); err != nil {
		t.Fatal(err)
	}

	s := &Store{pool: pool}
	if err = s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var version10, version8 bool
	if err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_version WHERE version=10), EXISTS(SELECT 1 FROM schema_version WHERE version=8)`).Scan(&version10, &version8); err != nil {
		t.Fatal(err)
	}
	if !version10 || version8 {
		t.Fatalf("version10=%t version8=%t", version10, version8)
	}
	var tableOID, uniqueOID uint32
	if err = pool.QueryRow(ctx, `SELECT 'linear_outbox'::regclass::oid`).Scan(&tableOID); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT oid FROM pg_constraint WHERE conrelid='linear_outbox'::regclass AND contype='u'`).Scan(&uniqueOID); err != nil {
		t.Fatal(err)
	}
	if err = s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var repeatedTableOID, repeatedUniqueOID uint32
	if err = pool.QueryRow(ctx, `SELECT 'linear_outbox'::regclass::oid`).Scan(&repeatedTableOID); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT oid FROM pg_constraint WHERE conrelid='linear_outbox'::regclass AND contype='u'`).Scan(&repeatedUniqueOID); err != nil {
		t.Fatal(err)
	}
	if repeatedTableOID != tableOID || repeatedUniqueOID != uniqueOID {
		t.Fatalf("migration rebuilt objects: table=%d/%d unique=%d/%d", tableOID, repeatedTableOID, uniqueOID, repeatedUniqueOID)
	}
}
