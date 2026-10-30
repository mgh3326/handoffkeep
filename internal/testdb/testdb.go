// Package testdb is the one way tests reach PostgreSQL.
//
// Every DB-backed test gets its URL from URL (or a schema from Schema), so
// the skip policy is the same everywhere: without HANDOFFKEEP_TEST_DB_URL a
// local run skips the test, while a CI run (CI or GITHUB_ACTIONS set) fails
// it, so a misconfigured CI job cannot pass by skipping.
package testdb

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvVar names the variable that holds the test database URL.
const EnvVar = "HANDOFFKEEP_TEST_DB_URL"

// URL returns the shared test database URL, or skips (locally) / fails
// (in CI) the test when it is unset.
func URL(t testing.TB) string {
	t.Helper()
	u := os.Getenv(EnvVar)
	if u == "" {
		if InCI() {
			t.Fatalf("%s must be set in CI; PostgreSQL tests may not skip there", EnvVar)
		}
		t.Skipf("%s is not set; skipping PostgreSQL test", EnvVar)
	}
	return u
}

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
// whose search_path puts it first (then public, for extensions). The schema
// is dropped at cleanup. Use it for tests that compare whole-table state or
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
	return schema, WithSearchPath(t, base, schema+",public")
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
