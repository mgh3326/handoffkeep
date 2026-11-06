package testdb

import (
	"context"
	"fmt"
	neturl "net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// useTrigram creates a table in the schema behind dbURL, indexes it with
// gin_trgm_ops, and runs a trigram query: it fails if the opclass or the
// similarity function is unreachable from that schema's search_path.
func useTrigram(ctx context.Context, dbURL string) error {
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	for _, sql := range []string{
		`CREATE TABLE trgm_probe (body TEXT NOT NULL)`,
		`CREATE INDEX trgm_probe_idx ON trgm_probe USING GIN (body gin_trgm_ops)`,
	} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			return err
		}
	}
	var score float32
	if err := conn.QueryRow(ctx, `SELECT similarity('handoff', 'handoff')`).Scan(&score); err != nil {
		return err
	}
	if score != 1 {
		return fmt.Errorf("similarity = %v, want 1", score)
	}
	return nil
}

// probe creates a throwaway schema on base and uses the trigram opclass in
// it. It is safe to call from a goroutine; the caller runs drop.
func probe(ctx context.Context, base, prefix string) (schema string, drop func(), err error) {
	schema, dbURL, drop, err := newSchema(ctx, base, prefix)
	if err != nil {
		return "", nil, err
	}
	if err := useTrigram(ctx, dbURL); err != nil {
		drop()
		return "", nil, fmt.Errorf("schema %s: %w", schema, err)
	}
	return schema, drop, nil
}

// probeAll runs n probes concurrently, each creating its own schema, and
// returns the drops of those that succeeded.
func probeAll(t *testing.T, base string, n int) (drops []func()) {
	t.Helper()
	start := make(chan struct{})
	type result struct {
		drop func()
		err  error
	}
	results := make(chan result, n)
	for i := 0; i < n; i++ {
		go func() {
			<-start
			_, drop, err := probe(t.Context(), base, "tdb_par")
			results <- result{drop, err}
		}()
	}
	close(start)
	for i := 0; i < n; i++ {
		r := <-results
		if r.err != nil {
			t.Error(r.err)
			continue
		}
		t.Cleanup(r.drop)
		drops = append(drops, r.drop)
	}
	return drops
}

// Schemas are created inside the concurrent workers, not before them.
func TestParallelSchemasSeeTrigramOpclass(t *testing.T) {
	if got := len(probeAll(t, URL(t), 6)); got != 6 {
		t.Fatalf("%d of 6 parallel schemas worked", got)
	}
}

func TestDroppingOneSchemaLeavesTheOtherWorking(t *testing.T) {
	base := URL(t)
	ctx := t.Context()
	_, dropFirst, err := probe(ctx, base, "tdb_drop_a")
	if err != nil {
		t.Fatal(err)
	}
	_, dropSecond, err := probe(ctx, base, "tdb_drop_b")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dropSecond)
	secondSchema, secondURL, dropThird, err := newSchema(ctx, base, "tdb_drop_c")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dropThird)
	dropFirst() // DROP SCHEMA ... CASCADE on the first
	if err := useTrigram(ctx, secondURL); err != nil {
		t.Fatalf("schema %s after the first was dropped: %v", secondSchema, err)
	}
	// The extension must survive too: a new schema still gets the opclass.
	if _, drop, err := probe(ctx, base, "tdb_drop_d"); err != nil {
		t.Fatalf("new schema after a drop: %v", err)
	} else {
		drop()
	}
}

func TestThrowawaySchemaCannotSeePublicTables(t *testing.T) {
	name := fmt.Sprintf("tdb_public_only_%d", time.Now().UnixNano())
	admin := Conn(t)
	ctx := t.Context()
	if _, err := admin.Exec(ctx, `CREATE TABLE public.`+name+` (id INT)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), `DROP TABLE IF EXISTS public.`+name) })

	pool, _ := SchemaPool(t, "tdb_nopublic")
	var visible bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible {
		t.Fatalf("public.%s is visible from a throwaway schema", name)
	}
	if _, err := pool.Exec(ctx, `SELECT * FROM `+name); err == nil {
		t.Fatalf("SELECT from public.%s succeeded in a throwaway schema", name)
	}
	// The same table is visible on the shared URL, whose path starts at public.
	var shared bool
	if err := admin.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&shared); err != nil {
		t.Fatal(err)
	}
	if !shared {
		t.Fatalf("public.%s is not visible on the shared URL", name)
	}
}

// Concurrent first-time setup: a fresh database has no pg_trgm, and each
// worker installs it and creates its own schema, as test processes of
// different packages do on a new CI database.
func TestFirstTimeSetupConcurrent(t *testing.T) {
	admin := Conn(t)
	ctx := t.Context()
	name := fmt.Sprintf("tdb_fresh_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), `DROP DATABASE `+name+` WITH (FORCE)`) })
	parsed, err := neturl.Parse(URL(t))
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name
	parsed.RawQuery = ""
	fresh := parsed.String()

	const n = 8
	start := make(chan struct{})
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			<-start
			if err := installExt(fresh); err != nil {
				errs <- err
				return
			}
			_, drop, err := probe(ctx, fresh, "tdb_fresh")
			if err == nil {
				drop()
			}
			errs <- err
		}()
	}
	close(start)
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	var schemas int
	conn, err := pgx.Connect(ctx, fresh)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = 'pg_trgm' AND n.nspname = $1`, ExtSchema).Scan(&schemas); err != nil || schemas != 1 {
		t.Fatalf("pg_trgm in %s: count=%d err=%v", ExtSchema, schemas, err)
	}
}

// An extension installed in public (a store migration on the shared URL, or an
// older test database) is moved into ExtSchema. The move runs in a
// transaction that is rolled back, so concurrent packages never see it.
func TestPlaceExtMovesExtensionOutOfPublic(t *testing.T) {
	conn := Conn(t)
	ctx := t.Context()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, extLockID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `ALTER EXTENSION pg_trgm SET SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	if err := placeExt(ctx, tx); err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := tx.QueryRow(ctx, `SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = 'pg_trgm'`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if schema != ExtSchema {
		t.Fatalf("pg_trgm is in %q, want %q", schema, ExtSchema)
	}
}
