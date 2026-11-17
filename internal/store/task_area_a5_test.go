package store

import (
	"reflect"
	"strings"
	"testing"
)

// These tests need no database: they check the text and argument builders the
// MySQL paths of the spawn, thread and project files use.

func TestA5PositionalOrdersAndRepeatsArguments(t *testing.T) {
	query, args := a5Positional(`UPDATE t SET a=$5,b=JSON_ARRAY_APPEND(b,'$',c),u=$4 WHERE id=$1 AND d=$2 AND c=$3 AND e=$4`, []any{"id", "d", "c", "now", "five"})
	if want := `UPDATE t SET a=?,b=JSON_ARRAY_APPEND(b,'$',c),u=? WHERE id=? AND d=? AND c=? AND e=?`; query != want {
		t.Fatalf("query=%q, want %q", query, want)
	}
	if want := []any{"five", "now", "id", "d", "c", "now"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("args=%v, want %v", args, want)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("a placeholder with no argument was accepted")
		}
	}()
	a5Positional(`SELECT $2`, []any{1})
}

func TestTaskThreadItemsMySQLBindsOneCutoffPerDecisionSource(t *testing.T) {
	for _, c := range []struct{ gates, deploys, decisions bool }{{}, {gates: true}, {deploys: true}, {decisions: true}, {true, true, true}} {
		q := taskThreadItemsMySQL(c.gates, c.deploys, c.decisions)
		want := 3
		if c.decisions {
			want += 2
		}
		if got := strings.Count(q, "?"); got != want {
			t.Errorf("%+v: %d placeholders, want %d", c, got, want)
		}
		if strings.Contains(q, "$") || strings.Contains(q, `"`) || strings.Contains(q, "||") || strings.Contains(q, "::") {
			t.Errorf("%+v: PostgreSQL syntax left in the MySQL text", c)
		}
		for table, present := range map[string]bool{"change_gate_events": c.gates, "deploy_changes": c.deploys, "decision_requests": c.decisions} {
			if strings.Contains(q, table) != present {
				t.Errorf("%+v: table %s present=%v in the query", c, table, !present)
			}
		}
		if !strings.Contains(q, "task_events e") {
			t.Errorf("%+v: the event source is missing", c)
		}
	}
}
