package store

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestStateCategoriesAreTheDesignTable pins the derived category of each of
// the eleven states to docs/linear-internalize.md "State model".
func TestStateCategoriesAreTheDesignTable(t *testing.T) {
	want := map[string]string{
		"triage": "triage", "backlog": "backlog", "unstarted": "unstarted",
		"researching": "started", "developing": "started", "reviewing": "started",
		"merged": "completed", "deployed": "completed", "completed": "completed",
		"dropped": "canceled", "duplicate": "canceled", "icebox": "canceled",
	}
	if len(TaskStates()) != 12 {
		t.Fatalf("TaskStates=%v, want twelve", TaskStates())
	}
	for state, category := range want {
		if got := TaskCategory(state); got != category {
			t.Errorf("TaskCategory(%q)=%q, want %q", state, got, category)
		}
		if !taskStates[state] {
			t.Errorf("%q missing from taskStates", state)
		}
	}
	if len(taskStates) != len(want) {
		t.Errorf("taskStates has %d entries, want %d", len(taskStates), len(want))
	}
	// Open means started and nothing else; backlog, unstarted and icebox are not open.
	for state := range want {
		open := state == "researching" || state == "developing" || state == "reviewing"
		if IsStartedState(state) != open {
			t.Errorf("IsStartedState(%q)=%t, want %t", state, IsStartedState(state), open)
		}
	}
	for _, old := range LegacyTaskStates() {
		if taskStates[old] {
			t.Errorf("old name %q must not be a state", old)
		}
		if TaskCategory(old) != "" {
			t.Errorf("old name %q has a category", old)
		}
	}
}

// TestManualTransitionTableIsTheDesignTable pins every manual edge to the
// design's "Manual transitions" table, so an edit to TaskTransitions has to
// edit this list as well.
func TestManualTransitionTableIsTheDesignTable(t *testing.T) {
	want := map[string][]string{
		"triage":      {"backlog", "unstarted", "dropped", "duplicate"},
		"backlog":     {"unstarted", "researching", "developing", "icebox", "dropped", "duplicate", "completed"},
		"unstarted":   {"backlog", "researching", "developing", "icebox", "dropped", "duplicate", "completed"},
		"researching": {"developing", "reviewing", "unstarted", "icebox", "dropped", "duplicate", "completed"},
		"developing":  {"researching", "reviewing", "unstarted", "icebox", "dropped", "duplicate", "completed"},
		"reviewing":   {"developing", "merged", "unstarted", "icebox", "dropped", "duplicate", "completed"},
		"merged":      {"unstarted"},
		"deployed":    {"unstarted"},
		"completed":   {"unstarted"},
		"dropped":     {"unstarted"},
		"duplicate":   {"unstarted"},
		"icebox":      {"backlog", "unstarted"},
	}
	got := map[string][]string{}
	for from, targets := range TaskTransitions {
		for to := range targets {
			got[from] = append(got[from], to)
		}
		sort.Strings(got[from])
	}
	for from := range want {
		sort.Strings(want[from])
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TaskTransitions=%v\nwant %v", got, want)
	}
	// No manual edge enters deployed, and triage has no edge to a started state.
	for from, targets := range TaskTransitions {
		if targets["deployed"] {
			t.Errorf("manual edge %s -> deployed", from)
		}
	}
	for _, to := range []string{"researching", "developing", "reviewing", "merged"} {
		if TaskTransitions["triage"][to] {
			t.Errorf("triage -> %s", to)
		}
	}
	// A task cannot enter merged from researching or developing by hand.
	if TaskTransitions["researching"]["merged"] || TaskTransitions["developing"]["merged"] {
		t.Error("researching/developing -> merged must not be a manual edge")
	}
}

func TestSpawnRuleSubstates(t *testing.T) {
	for kind, want := range map[string]string{"implement": "developing", "fix": "developing", "ops": "developing", "verify": "researching", "decide": "researching"} {
		if got := substateForKind(kind); got != want {
			t.Errorf("substateForKind(%q)=%q, want %q", kind, got, want)
		}
	}
	// A live job's role of consult or research gives researching, otherwise the kind decides.
	cases := []struct{ role, kind, want string }{
		{"consult", "implement", "researching"},
		{"research", "fix", "researching"},
		{"worker", "implement", "developing"},
		{"worker", "verify", "researching"},
		{"", "ops", "developing"},
	}
	for _, c := range cases {
		if got := substateForJobRole(c.role, c.kind); got != c.want {
			t.Errorf("substateForJobRole(%q,%q)=%q, want %q", c.role, c.kind, got, c.want)
		}
	}
}

// TestOldNamesParseAsFilters is the "Old names on input" table for state
// filters; the API tests exercise the same names over HTTP.
func TestOldNamesParseAsFilters(t *testing.T) {
	cases := map[string]StateFilter{
		"in_progress":    {States: []string{"researching", "developing"}},
		"claimed":        {States: []string{"researching", "developing"}},
		"verifying":      {States: []string{"reviewing"}},
		"join":           {States: []string{"reviewing"}},
		"hold":           {States: []string{"icebox"}},
		"reopened":       {States: []string{"unstarted"}},
		"needs_decision": {Awaiting: true},
		"merged":         {States: []string{"merged"}},
		"duplicate":      {States: []string{"duplicate"}},
	}
	for name, want := range cases {
		got, ok := ParseStateFilter(name)
		if !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("ParseStateFilter(%q)=%+v ok=%t, want %+v", name, got, ok, want)
		}
	}
	if _, ok := ParseStateFilter("nonsense"); ok {
		t.Error("an unknown name must not parse")
	}
	if !validTaskQuery("", "in_progress", "", nil) || validTaskQuery("", "nonsense", "", nil) {
		t.Error("validTaskQuery must accept old filter names and refuse unknown ones")
	}
}

// TestLinearMappingCoversEveryState keeps the Linear table total over the
// eleven states, with the design's terminal set.
func TestLinearMappingCoversEveryState(t *testing.T) {
	terminal := map[string]bool{"merged": true, "deployed": true, "completed": true, "dropped": true, "duplicate": true, "icebox": true}
	names := map[string]string{
		"triage": "Backlog", "backlog": "Backlog", "unstarted": "Backlog",
		"researching": "In Progress", "developing": "In Progress", "reviewing": "In Review",
		"merged": "Done", "deployed": "Done", "completed": "Done", "dropped": "Canceled", "duplicate": "Canceled", "icebox": "Canceled",
	}
	for _, state := range TaskStates() {
		m, ok := LinearTaskStateMapping[state]
		if !ok || !m.Mutate || m.Name != names[state] || m.Terminal != terminal[state] {
			t.Errorf("LinearTaskStateMapping[%q]=%+v ok=%t", state, m, ok)
		}
	}
	if len(LinearTaskStateMapping) != len(TaskStates()) {
		t.Errorf("mapping has %d entries for %d states", len(LinearTaskStateMapping), len(TaskStates()))
	}
}

// TestTaskStateMachineReturnsDetachedClosureTypes keeps the exported read
// model from aliasing the enforcement table: a caller mutating the returned
// rule must not change what applyLifecycle accepts.
func TestTaskStateMachineReturnsDetachedClosureTypes(t *testing.T) {
	var merged *TaskClosureRule
	for _, state := range TaskStateMachine() {
		if state.Name == StateMerged {
			merged = state.Closure
		}
	}
	if merged == nil || len(merged.Types) == 0 {
		t.Fatal("merged has no closure rule")
	}
	merged.Types[0] = "cancelled"
	if got := taskClosureRules[StateMerged].Types[0]; got != "resolved" {
		t.Fatalf("mutating the returned rule changed enforcement to %q", got)
	}
	x := Task{}
	if err := applyLifecycle(&x, StateReviewing, StateMerged, time.Now().UTC(), &TaskClosure{Type: "resolved", Evidence: "https://example.com/r"}); err != nil {
		t.Fatalf("enforcement refused resolved after the read model was mutated: %v", err)
	}
}

func TestRollbackScriptVersion(t *testing.T) {
	b, err := readRepoFile("scripts/rollback-state-migration.sql")
	if err != nil {
		t.Fatal(err)
	}
	if want := "schema_version: " + itoaForTest(stateMigrationVersion); !contains(string(b), want) {
		t.Errorf("rollback script does not carry %q", want)
	}
	if want := "DELETE FROM schema_version WHERE version = " + itoaForTest(stateMigrationVersion) + ";"; !contains(string(b), want) {
		t.Errorf("rollback script does not delete schema version %d", stateMigrationVersion)
	}
	if SchemaVersion < stateMigrationVersion {
		t.Errorf("SchemaVersion=%d is below the state migration %d", SchemaVersion, stateMigrationVersion)
	}
}

func TestSettledStatesSQLMatchesIsSettledState(t *testing.T) {
	got := map[string]bool{}
	for _, state := range strings.Split(settledStatesSQL, ",") {
		got[strings.Trim(state, "'")] = true
	}
	for _, state := range TaskStates() {
		if got[state] != IsSettledState(state) {
			t.Errorf("settled mismatch for %s", state)
		}
		delete(got, state)
	}
	if len(got) != 0 {
		t.Errorf("unknown settled states: %v", got)
	}
}
