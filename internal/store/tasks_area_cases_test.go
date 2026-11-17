package store

import (
	"sort"
	"testing"
)

// The tasks area's behaviour tests run on both dialects: a case is a function
// of a freshly migrated Store that uses only the store's public methods and no
// dialect-specific SQL (fixtures included). PostgreSQL runs them here;
// TestMySQLTasksArea (tasks_area_mysql_test.go) runs the same list on MySQL.
// A case registers itself from init() in its own file, so areas and PRs never
// edit a shared list.
type taskAreaCase struct {
	name string
	run  func(t *testing.T, s *Store)
}

var taskAreaCases []taskAreaCase

func registerTaskAreaCase(name string, run func(t *testing.T, s *Store)) {
	taskAreaCases = append(taskAreaCases, taskAreaCase{name, run})
}

func sortedTaskAreaCases() []taskAreaCase {
	out := append([]taskAreaCase(nil), taskAreaCases...)
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func TestTasksAreaPostgres(t *testing.T) {
	for _, c := range sortedTaskAreaCases() {
		t.Run(c.name, func(t *testing.T) { c.run(t, schemaStore(t, "tasks_area")) })
	}
}
