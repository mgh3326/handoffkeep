package store

import (
	"context"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestLinearV10AdditiveAndIdempotent(t *testing.T) {
	ctx := context.Background()
	pool, _ := testdb.SchemaPool(t, "linear_v10")
	var err error
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
