//go:build mysql

package store

import (
	"testing"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestMySQLBenchKeysKeepTrailingSpaces(t *testing.T) {
	dbURL := testdb.MySQLDatabase(t)
	s, err := Open(t.Context(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	testBenchKeysKeepTrailingSpaces(t, s)
}
