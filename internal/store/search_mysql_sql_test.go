package store

import (
	"strings"
	"testing"
)

// The MySQL search statements are built here without a database: each must bind
// one argument per placeholder, and none may carry PostgreSQL-only syntax. The
// execution itself is covered by the //go:build mysql tests in CI.
func TestSearchMySQLStatementsBindEveryPlaceholder(t *testing.T) {
	type built struct {
		name string
		sql  string
		args []any
	}
	var cases []built
	docSQL, docArgs := docsSearchMySQL("needle", "s1", 21)
	cases = append(cases, built{"docs", docSQL, docArgs})
	for _, c := range []struct {
		name     string
		lane     string
		comments bool
	}{
		{"tasks", "lane", false},
		{"tasks+comments", "lane", true},
		{"tasks no lane", "", true},
	} {
		sql, args := tasksSearchMySQL("needle", c.lane, "needle", 21, c.comments)
		cases = append(cases, built{c.name, sql, args})
	}
	for _, c := range cases {
		if got := countPlaceholders(c.sql); got != len(c.args) {
			t.Errorf("%s: %d placeholders, %d args\n%s", c.name, got, len(c.args), c.sql)
		}
		for _, pg := range []string{"$1", "$2", "$3", "$4", "ILIKE", "::", "DISTINCT ON", "to_tsvector", "plainto_tsquery", "ts_headline", "||", "split_part", "to_regclass"} {
			if strings.Contains(c.sql, pg) {
				t.Errorf("%s: PostgreSQL syntax %q in MySQL statement:\n%s", c.name, pg, c.sql)
			}
		}
	}
}

// countPlaceholders counts ? outside single-quoted SQL literals. The statements
// here put no ? inside a literal, so the count is the bound-argument count.
func countPlaceholders(sql string) int {
	n, inLit := 0, false
	for _, r := range sql {
		switch {
		case r == '\'':
			inLit = !inLit
		case r == '?' && !inLit:
			n++
		}
	}
	return n
}
