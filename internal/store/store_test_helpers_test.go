package store

import (
	"testing"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// schemaStore is the store tests' one fixture: a Store opened through Open,
// as production opens it, on a throwaway schema named after prefix. Use it
// for any test that counts rows, truncates, or seeds fixed keys; tests in the
// shared schema use Open(testdb.URL(t)) and unique lanes instead.
func schemaStore(t *testing.T, prefix string) *Store {
	t.Helper()
	_, url := testdb.Schema(t, prefix)
	s, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}
