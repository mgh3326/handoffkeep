package testdb

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// useTrigram creates a table in the pool's throwaway schema, indexes it with
// gin_trgm_ops, and runs a trigram query: it fails if the opclass or the
// similarity function is unreachable from that schema's search_path.
func useTrigram(exec func(sql string) error, query func(sql string) (float64, error)) error {
	if err := exec(`CREATE TABLE trgm_probe (body TEXT NOT NULL)`); err != nil {
		return err
	}
	if err := exec(`CREATE INDEX trgm_probe_idx ON trgm_probe USING GIN (body gin_trgm_ops)`); err != nil {
		return err
	}
	score, err := query(`SELECT similarity('handoff', 'handoff')`)
	if err != nil {
		return err
	}
	if score != 1 {
		return fmt.Errorf("similarity = %v, want 1", score)
	}
	return nil
}

func probe(t *testing.T, prefix string) (schema string, run func() error) {
	t.Helper()
	pool, schema := SchemaPool(t, prefix)
	ctx := t.Context()
	return schema, func() error {
		return useTrigram(
			func(sql string) error { _, err := pool.Exec(ctx, sql); return err },
			func(sql string) (float64, error) {
				var f float32
				err := pool.QueryRow(ctx, sql).Scan(&f)
				return float64(f), err
			})
	}
}

func TestParallelSchemasSeeTrigramOpclass(t *testing.T) {
	const n = 6
	var ready sync.WaitGroup
	ready.Add(n)
	for i := 0; i < n; i++ {
		t.Run(fmt.Sprintf("schema%d", i), func(t *testing.T) {
			t.Parallel()
			_, run := probe(t, "tdb_par")
			ready.Done()
			ready.Wait() // every schema exists before any of them indexes
			if err := run(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDroppingOneSchemaLeavesTheOtherWorking(t *testing.T) {
	first, runFirst := probe(t, "tdb_drop_a")
	_, runSecond := probe(t, "tdb_drop_b")
	if err := runFirst(); err != nil {
		t.Fatal(err)
	}
	if _, err := Conn(t).Exec(t.Context(), "DROP SCHEMA "+first+" CASCADE"); err != nil {
		t.Fatal(err)
	}
	if err := runSecond(); err != nil {
		t.Fatalf("second schema after the first was dropped: %v", err)
	}
	// The extension must survive too: a new schema still gets the opclass.
	_, runThird := probe(t, "tdb_drop_c")
	if err := runThird(); err != nil {
		t.Fatalf("new schema after a drop: %v", err)
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

// Concurrent first-time installs from several test processes must all
// succeed; the goroutines stand in for the processes (each has its own
// connection, which is what the advisory lock serializes).
func TestInstallExtConcurrent(t *testing.T) {
	url := URL(t)
	const n = 8
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- installExt(url)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
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
