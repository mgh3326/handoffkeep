package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestMigrateStatesRequiresDryRunAndDatabase(t *testing.T) {
	t.Setenv("HANDOFFKEEP_DB_URL", "")
	var out, errout bytes.Buffer
	if err := run([]string{"tasks", "migrate-states"}, &out, &errout); err == nil || !strings.Contains(err.Error(), "--dry-run") {
		t.Fatalf("without --dry-run: %v", err)
	}
	if err := run([]string{"tasks", "migrate-states", "--dry-run"}, &out, &errout); err == nil || !strings.Contains(err.Error(), "HANDOFFKEEP_DB_URL") {
		t.Fatalf("without a database: %v", err)
	}
	if err := run([]string{"tasks", "migrate-states", "--dry-run", "extra"}, &out, &errout); err == nil {
		t.Fatal("positional arguments are refused")
	}
	if out.Len() != 0 {
		t.Fatalf("a refused dry-run printed %q", out.String())
	}
}

func TestMigrationPlanOutput(t *testing.T) {
	plan := store.StateMigrationPlan{
		Counts: []store.StateMigrationCount{
			{LegacyState: "claimed", Rule: "claimed_ended", NewState: "unstarted", Count: 2},
			{LegacyState: "hold", Rule: "hold_icebox", NewState: "icebox", Count: 5},
		},
		HoldMirroredToCanceled: 3,
	}
	var text bytes.Buffer
	if err := writeStateMigrationPlan(&text, plan, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"legacy_state", "claimed_ended", "unstarted", "hold_icebox", "icebox", "changed tasks: 7", "mirrored to Linear that would show as Canceled: 3"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("dry-run table lacks %q:\n%s", want, text.String())
		}
	}
	var asJSON bytes.Buffer
	if err := writeStateMigrationPlan(&asJSON, plan, true); err != nil {
		t.Fatal(err)
	}
	var back store.StateMigrationPlan
	if err := json.Unmarshal(asJSON.Bytes(), &back); err != nil || len(back.Counts) != 2 || back.HoldMirroredToCanceled != 3 {
		t.Fatalf("json plan: %s err=%v", asJSON.String(), err)
	}
	var done bytes.Buffer
	if err := writeStateMigrationPlan(&done, store.StateMigrationPlan{Migrated: true}, false); err != nil || !strings.Contains(done.String(), "already migrated") {
		t.Fatalf("migrated: %q err=%v", done.String(), err)
	}
}

// fakeHK answers every request, recording the last one.
func fakeHK(t *testing.T, response any) (*httptest.Server, *struct{ Method, Path, Body string }) {
	t.Helper()
	last := &struct{ Method, Path, Body string }{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		_, _ = b.ReadFrom(r.Body)
		last.Method, last.Path, last.Body = r.Method, r.URL.Path, b.String()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(server.Close)
	return server, last
}

func TestTasksParentRelateAndDuplicateOfCommands(t *testing.T) {
	parent := int64(5)
	server, last := fakeHK(t, store.Task{ID: 9, State: "backlog", Category: "backlog", ParentID: &parent})
	var out, errout bytes.Buffer
	base := []string{"--url", server.URL, "--token", "t"}
	if err := run(append([]string{"tasks", "parent", "9", "--to", "5"}, base...), &out, &errout); err != nil {
		t.Fatal(err)
	}
	if last.Method != "POST" || last.Path != "/v1/tasks/9/parent" || !strings.Contains(last.Body, `"parent_id":5`) {
		t.Fatalf("parent request: %+v", *last)
	}
	for _, bad := range [][]string{{"tasks", "parent", "9"}, {"tasks", "parent", "9", "--to", "5", "--clear"}, {"tasks", "parent", "--to", "5"}} {
		if err := run(append(bad, base...), &out, &errout); err == nil {
			t.Errorf("%v must be refused", bad)
		}
	}
	rel, last := fakeHK(t, map[string]any{"changed": true, "task": store.Task{ID: 9, State: "backlog"}})
	relBase := []string{"--url", rel.URL, "--token", "t"}
	if err := run(append([]string{"tasks", "relate", "9", "--kind", "blocks", "--other", "11"}, relBase...), &out, &errout); err != nil {
		t.Fatal(err)
	}
	if last.Path != "/v1/tasks/9/relations" || !strings.Contains(last.Body, `"kind":"blocks"`) || !strings.Contains(last.Body, `"other":11`) {
		t.Fatalf("relate request: %+v", *last)
	}
	if err := run(append([]string{"tasks", "unrelate", "9", "--kind", "blocks", "--other", "11"}, relBase...), &out, &errout); err != nil {
		t.Fatal(err)
	}
	if last.Path != "/v1/tasks/9/relations/remove" {
		t.Fatalf("unrelate request: %+v", *last)
	}
	if err := run(append([]string{"tasks", "relate", "9", "--kind", "blocks"}, relBase...), &out, &errout); err == nil {
		t.Error("relate without --other must be refused")
	}
	// transition --duplicate-of and add --parent reach the server.
	dup, last := fakeHK(t, store.Task{ID: 9, State: "duplicate", Category: "canceled"})
	dupBase := []string{"--url", dup.URL, "--token", "t"}
	if err := run(append([]string{"tasks", "transition", "9", "--to", "duplicate", "--duplicate-of", "4", "--note", "same"}, dupBase...), &out, &errout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(last.Body, `"duplicate_of":4`) || !strings.Contains(last.Body, `"to":"duplicate"`) {
		t.Fatalf("transition request: %+v", *last)
	}
	if err := run(append([]string{"tasks", "list", "--parent", "3"}, dupBase...), &out, &errout); err == nil {
		t.Error("--parent is accepted only by tasks add")
	}
	if err := run(append([]string{"tasks", "claim", "9", "--duplicate-of", "3"}, dupBase...), &out, &errout); err == nil {
		t.Error("--duplicate-of is accepted only by tasks transition")
	}
	project := "experiment"
	added, last := fakeHK(t, store.Task{ID: 10, State: "backlog", Project: &project})
	addBase := []string{"--url", added.URL, "--token", "t"}
	if err := run(append([]string{"tasks", "add", "--lane", "l", "--title", "child", "--project", "experiment", "--parent", "3"}, addBase...), &out, &errout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(last.Body, `"parent_id":3`) {
		t.Fatalf("add request: %+v", *last)
	}
}
