// Package testdb is the one way tests reach PostgreSQL.
//
// Every DB-backed test gets its URL from URL (or a schema from Schema), so
// the skip policy is the same everywhere: without HANDOFFKEEP_TEST_DB_URL a
// local run skips the test, while a CI run (CI or GITHUB_ACTIONS set) fails
// it, so a misconfigured CI job cannot pass by skipping. A CI job that runs
// without PostgreSQL on purpose (the PR tier and the macOS job, see
// .github/workflows/ci.yml) says so with HANDOFFKEEP_TEST_NO_DB=1 and skips.
package testdb

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvVar names the variable that holds the test database URL.
const EnvVar = "HANDOFFKEEP_TEST_DB_URL"

// NoDBVar, set to "1", declares a CI job that runs without PostgreSQL, so
// DB-backed tests skip there instead of failing.
const NoDBVar = "HANDOFFKEEP_TEST_NO_DB"

// ExtSchema is the schema that holds the pg_trgm extension in the test
// database. Throwaway schemas reach the extension's operator classes through
// it and nothing else, so they never fall through to tables in public.
const ExtSchema = "hk_test_ext"

// extLockID is the advisory lock that serializes ensureExt across test
// processes. It differs from the store's migration lock.
const extLockID = 824180046

// URL returns the shared test database URL, or skips (locally) / fails
// (in CI) the test when it is unset. A URL without a search_path gets
// "public,hk_test_ext", so shared-schema tests still find gin_trgm_ops.
func URL(t testing.TB) string {
	t.Helper()
	u := os.Getenv(EnvVar)
	if u == "" {
		if os.Getenv(NoDBVar) == "1" {
			t.Skipf("%s=1: this job runs without PostgreSQL; DB tests run after merge", NoDBVar)
		}
		if InCI() {
			t.Fatalf("%s must be set in CI; PostgreSQL tests may not skip there", EnvVar)
		}
		t.Skipf("%s is not set; skipping PostgreSQL test", EnvVar)
	}
	ensureExt(t, u)
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Query().Get("search_path") != "" {
		return u
	}
	return WithSearchPath(t, u, "public,"+ExtSchema)
}

var (
	extOnce sync.Once
	extErr  error
)

// ensureExt makes pg_trgm exist in ExtSchema, once per process. Test
// processes of different packages run in parallel against one database, so
// the work runs under an advisory lock: the check and the CREATE EXTENSION
// cannot interleave with another process's. An extension that an earlier run
// (or a store migration in public) installed elsewhere is moved, because an
// extension lives in one schema per database.
func ensureExt(t testing.TB, dbURL string) {
	t.Helper()
	extOnce.Do(func() { extErr = installExt(dbURL) })
	if extErr != nil {
		t.Fatalf("testdb: install pg_trgm in %s: %v", ExtSchema, extErr)
	}
}

func installExt(dbURL string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, extLockID); err != nil {
		return err
	}
	if err := placeExt(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// placeExt leaves pg_trgm installed in ExtSchema, whether it was missing or
// installed in another schema. The caller holds extLockID.
func placeExt(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+ExtSchema); err != nil {
		return err
	}
	var current string
	err := tx.QueryRow(ctx, `SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = 'pg_trgm'`).Scan(&current)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		_, err = tx.Exec(ctx, `CREATE EXTENSION pg_trgm SCHEMA `+ExtSchema)
	case err != nil:
	case current != ExtSchema:
		_, err = tx.Exec(ctx, `ALTER EXTENSION pg_trgm SET SCHEMA `+ExtSchema)
	}
	return err
}

// SearchPath is the search_path for a throwaway schema: the schema, then the
// extension schema. Tests that create their own schema (to add a role, say)
// use it instead of writing schema+",public".
func SearchPath(schema string) string { return schema + "," + ExtSchema }

// InCI reports whether the process runs under CI.
func InCI() bool {
	return os.Getenv("CI") == "true" || os.Getenv("GITHUB_ACTIONS") == "true"
}

// Conn opens one connection to the shared test database, closed at cleanup.
func Conn(t testing.TB) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

var schemaSeq atomic.Int64

// Schema creates a throwaway schema named after prefix and returns a URL
// whose search_path puts it first, then ExtSchema for pg_trgm; public is not
// on the path, so a table that exists only in public is invisible. The
// schema is dropped at cleanup. Use it for tests that compare whole-table state or
// run lock-taking DDL, where sharing public with concurrently running
// packages would make them flaky.
func Schema(t testing.TB, prefix string) (schema, dbURL string) {
	t.Helper()
	base := URL(t)
	schema = fmt.Sprintf("%s_%d_%d", strings.ToLower(prefix), time.Now().UnixNano(), schemaSeq.Add(1))
	admin := Conn(t)
	if _, err := admin.Exec(context.Background(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	return schema, WithSearchPath(t, base, SearchPath(schema))
}

// WithSearchPath returns dbURL with its search_path parameter set.
func WithSearchPath(t testing.TB, dbURL, searchPath string) string {
	t.Helper()
	parsed, err := url.Parse(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", searchPath)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// SchemaPool is Schema plus a pool bound to the new schema, closed at
// cleanup before the schema is dropped.
func SchemaPool(t testing.TB, prefix string) (*pgxpool.Pool, string) {
	t.Helper()
	schema, dbURL := Schema(t, prefix)
	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, schema
}
