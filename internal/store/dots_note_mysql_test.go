//go:build mysql

package store

import (
	"testing"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// The MySQL leg of the dots-note tests in dots_note_test.go: each scenario
// runs again on a fresh MySQL database, so the MySQL twins of the comment and
// intake hooks and of migration 20261125-dots-note-outbox-kind are exercised.
func init() {
	dotsNoteMySQLStore = func(t *testing.T) *Store {
		t.Helper()
		s, err := Open(t.Context(), testdb.MySQLDatabase(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		return s
	}
}
