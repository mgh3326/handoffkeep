//go:build mysql

package store

import (
	"testing"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// Under the mysql tag the merge-link retention tests (doc_retention_merge_test.go)
// also run on a fresh MySQL database each.
func init() {
	retentionMySQLStore = func(t *testing.T) *Store {
		t.Helper()
		s, err := Open(t.Context(), testdb.MySQLDatabase(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		return s
	}
}
