package store

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// schemaStore is the store tests' one fixture: a Store opened through Open,
// as production opens it, on a throwaway schema named after prefix. Use it
// for any test that counts rows, truncates, or seeds fixed keys; tests in the
// shared schema use Open(testdb.URL(t)) and unique lanes instead.
func schemaStore(t *testing.T, prefix string) *Store {
	t.Helper()
	s, _ := schemaStoreAt(t, prefix)
	return s
}

// schemaStoreAt is schemaStore for a test that also needs the schema's URL,
// to open a second connection or pool on the same schema.
func schemaStoreAt(t *testing.T, prefix string) (*Store, string) {
	t.Helper()
	_, url := testdb.Schema(t, prefix)
	s, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, url
}

// capturedStatement is one statement a counted pool ran, with its arguments,
// so a test can EXPLAIN exactly what a read asked the planner for.
type capturedStatement struct {
	SQL  string
	Args []any
}

// queryCounter counts the statements issued through one pool and keeps the
// statements themselves, so a test can assert that a read is bounded and that
// the SQL it did issue plans to an index scan.
type queryCounter struct {
	mu sync.Mutex
	// statements is every statement issued since the last Reset.
	statements []capturedStatement
}

// QueryCount is how many of the statements are queries. It leaves out the
// begin and commit pgx sends through Exec around a transaction, so a count is
// about a read's own statements.
func (c *queryCounter) QueryCount() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var n int64
	for _, st := range c.statements {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(st.SQL)), "SELECT") {
			n++
		}
	}
	return n
}

// Reset drops the statements seen so far.
func (c *queryCounter) Reset() {
	c.mu.Lock()
	c.statements = nil
	c.mu.Unlock()
}

// StatementsMatching returns the statements whose SQL contains substr.
func (c *queryCounter) StatementsMatching(substr string) []capturedStatement {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []capturedStatement
	for _, st := range c.statements {
		if strings.Contains(st.SQL, substr) {
			out = append(out, st)
		}
	}
	return out
}

func (c *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	c.mu.Lock()
	c.statements = append(c.statements, capturedStatement{SQL: data.SQL, Args: data.Args})
	c.mu.Unlock()
	return ctx
}

func (c *queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// countingStore is schemaStore with a pool whose tracer counts statements. The
// schema is migrated through Open first, so the counting Store only serves
// reads.
func countingStore(t *testing.T, prefix string) (*Store, *queryCounter) {
	t.Helper()
	_, url := testdb.Schema(t, prefix)
	seed, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	seed.Close()
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	counter := &queryCounter{}
	config.ConnConfig.Tracer = counter
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &Store{pool: pool}, counter
}

// deployRecordsOf is ListDeployRecordsByPrefix for one prefix: the read the
// console makes for one deploy service.
func deployRecordsOf(t *testing.T, s *Store, prefix string, limit int) []StoredDeployRecord {
	t.Helper()
	pages, err := s.ListDeployRecordsByPrefix(t.Context(), []string{prefix}, limit)
	if err != nil {
		t.Fatal(err)
	}
	return pages[prefix].Records
}
