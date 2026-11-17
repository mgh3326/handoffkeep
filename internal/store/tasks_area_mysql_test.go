//go:build mysql

package store

import (
	"testing"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// TestMySQLTasksArea runs the dialect-neutral cases of tasks_area_cases_test.go
// on MySQL, each on its own database. The database cleanup also fails the test
// when a case left a sync_log row without a sync_id.
func TestMySQLTasksArea(t *testing.T) {
	for _, c := range sortedTaskAreaCases() {
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
