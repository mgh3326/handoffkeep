package store

import (
	"context"
	"strings"
	"testing"
)

// CheckIntake and Search refuse text PostgreSQL cannot hold before they touch
// the database. These run without a pool: a refusal returns before any query,
// so a nil pool would panic if a refused input got through.
func TestCheckIntakeRefusesNULBeforeTheDatabase(t *testing.T) {
	project := "mcp-project"
	base := IntakeInput{Source: "chatgpt", SourceRef: "chatgpt-test", Title: "intake", Body: "body", Project: &project, CreatedBy: "tester"}
	cases := map[string]func(*IntakeInput){
		"title": func(in *IntakeInput) { in.Title = "nul\x00title" },
		"body":  func(in *IntakeInput) { in.Body = "nul\x00body" },
		"project": func(in *IntakeInput) {
			bad := "nul\x00project"
			in.Project = &bad
		},
	}
	for field, mutate := range cases {
		in := base
		mutate(&in)
		if err := CheckIntake(in); err == nil {
			t.Errorf("%s with a NUL byte: CheckIntake accepted it", field)
		}
	}
	if err := CheckIntake(base); err != nil {
		t.Fatalf("valid intake refused: %v", err)
	}
}

func TestSearchRefusesNULBeforeTheDatabase(t *testing.T) {
	var s Store // no pool: a refused query must return before any database call
	if _, err := s.Search(context.Background(), "nul\x00query", "tasks", "", 5); err == nil {
		t.Fatal("Search accepted a query with a NUL byte")
	}
	long := strings.Repeat("a", 513)
	if _, err := s.Search(context.Background(), long, "tasks", "", 5); err == nil {
		t.Fatal("Search accepted a query over 512 bytes")
	}
}
