package mcp

import (
	"context"
	"strings"
	"testing"
)

// task_intake refuses a NUL byte in its title, body or project as invalid
// input before any database call. Before the fix the duplicate search sent the
// raw title to PostgreSQL and the caller got SQLSTATE 22021 as an internal error.
func TestTaskIntakeRefusesNULBeforeTheDatabase(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	session := writesSession(t, st)
	countTasks := func() int {
		t.Helper()
		tasks, err := st.ListTasks(ctx, "", "", "", nil, nil, 100)
		if err != nil {
			t.Fatal(err)
		}
		return len(tasks)
	}
	before := countTasks()
	calls := []map[string]any{
		{"title": "nul\x00title intake", "body": "b", "project": "mcp-project"},
		{"title": "clean title", "body": "nul\x00body", "project": "mcp-project"},
		{"title": "clean title", "body": "b", "project": "mcp\x00project"},
	}
	for _, args := range calls {
		text, isError := toolText(t, session, "task_intake", args)
		if !isError || strings.Contains(text, "SQLSTATE") || strings.Contains(text, "22021") {
			t.Errorf("task_intake %v: isError=%v text=%q", args["title"], isError, text)
		}
	}
	if got := countTasks(); got != before {
		t.Errorf("tasks=%d after refused intakes, want %d", got, before)
	}
}

// task_search passes its query to the store search, which refuses a NUL byte
// before any database call, so the caller gets a tool error, not SQLSTATE 22021.
func TestTaskSearchRefusesNULBeforeTheDatabase(t *testing.T) {
	st := newOperatorStore(t)
	session := writesSession(t, st)
	text, isError := toolText(t, session, "task_search", map[string]any{"query": "nul\x00query"})
	if !isError || strings.Contains(text, "SQLSTATE") || strings.Contains(text, "22021") {
		t.Errorf("task_search with a NUL query: isError=%v text=%q", isError, text)
	}
}
