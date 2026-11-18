//go:build mysql

package store

import (
	"testing"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// TestMySQLSnapshotReads runs the dialect-neutral snapshot-consistency cases
// of snapshot_reads_cases_test.go on MySQL, each on its own database. The
// database cleanup also fails the test when a case left a sync_log row
// without a sync_id.
func TestMySQLSnapshotReads(t *testing.T) {
	for _, c := range sortedSnapshotReadCases() {
		t.Run(c.name, func(t *testing.T) {
			s, err := Open(t.Context(), testdb.MySQLDatabase(t))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(s.Close)
			c.run(t, s)
		})
	}
}
