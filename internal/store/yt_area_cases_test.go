package store

import (
	"sort"
	"testing"
)

// The YouTrack area's behaviour tests run on both dialects: a case is a
// function of a freshly migrated Store that uses only the store's public
// methods and no dialect-specific SQL (fixtures included). PostgreSQL runs
// them here; TestMySQLYTArea (yt_area_mysql_test.go) runs the same list on
// MySQL. A case registers itself from init() in its own file, so areas and
// PRs never edit a shared list (the pattern of tasks_area_cases_test.go).
type ytAreaCase struct {
	name string
	run  func(t *testing.T, s *Store)
}

var ytAreaCases []ytAreaCase

func registerYTAreaCase(name string, run func(t *testing.T, s *Store)) {
	ytAreaCases = append(ytAreaCases, ytAreaCase{name, run})
}

func sortedYTAreaCases() []ytAreaCase {
	out := append([]ytAreaCase(nil), ytAreaCases...)
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func TestYTAreaPostgres(t *testing.T) {
	for _, c := range sortedYTAreaCases() {
		t.Run(c.name, func(t *testing.T) { c.run(t, schemaStore(t, "yt_area")) })
	}
}
