package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func linearTestStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if url == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for Linear store tests")
	}
	st, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

func linearTask(t *testing.T, st *Store, refs TaskRefs) Task {
	t.Helper()
	task, err := st.CreateTask(t.Context(), Task{
		Lane:      fmt.Sprintf("linear-%d", time.Now().UnixNano()),
		Title:     "Mirror connector contract",
		Kind:      "implement",
		Refs:      refs,
		CreatedBy: "linear-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestLinearStateMappingIsCompleteAndSafe(t *testing.T) {
	if len(LinearTaskStateMapping) != len(taskStates) {
		t.Fatalf("mapping entries=%d task states=%d", len(LinearTaskStateMapping), len(taskStates))
	}
	for state := range taskStates {
		if _, found := LinearTaskStateMapping[state]; !found {
			t.Errorf("missing mapping for %s", state)
		}
	}
	nonterminal := []string{"backlog", "claimed", "in_progress", "verifying", "join", "hold", "needs_decision"}
	for _, state := range nonterminal {
		mapping := LinearTaskStateMapping[state]
		if mapping.Terminal || mapping.Name == "Done" || mapping.Name == "Canceled" {
			t.Errorf("nonterminal %s maps to terminal %+v", state, mapping)
		}
	}
	if mapping := LinearTaskStateMapping["needs_decision"]; mapping.Mutate || mapping.Name != "" {
		t.Fatalf("needs_decision must not mutate Linear: %+v", mapping)
	}
}

func TestLinearOutboxMutationAtomicity(t *testing.T) {
	st := linearTestStore(t)
	st.EnableLinearSync()
	task := linearTask(t, st, TaskRefs{Linear: &TaskLinear{Sync: true, Tier: "T3", Grade: "S", Brief: "brief/task"}})
	rows, err := st.ListLinearOutbox(t.Context(), task.ID)
	if err != nil || len(rows) != 1 || rows[0].Seq != 1 || rows[0].Op != LinearOpIssueCreate {
		t.Fatalf("create outbox=%+v err=%v", rows, err)
	}
	if _, err = st.ClaimTask(t.Context(), task.ID, "builder"); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ClaimTask(t.Context(), task.ID, "builder-again"); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("duplicate claim error=%v", err)
	}
	rows, err = st.ListLinearOutbox(t.Context(), task.ID)
	if err != nil || len(rows) != 2 || rows[1].Seq != 2 || rows[1].Op != LinearOpIssueState {
		t.Fatalf("claim outbox=%+v err=%v", rows, err)
	}
	if _, err = st.TransitionTask(t.Context(), task.ID, "in_progress", "builder", "started", nil); err != nil {
		t.Fatal(err)
	}
	before, found, err := st.GetTask(t.Context(), task.ID)
	if err != nil || !found {
		t.Fatalf("before task found=%t err=%v", found, err)
	}

	connection, err := pgx.Connect(t.Context(), os.Getenv("HANDOFFKEEP_TEST_DB_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	functionName := fmt.Sprintf("fail_linear_outbox_%d", task.ID)
	triggerName := fmt.Sprintf("fail_linear_outbox_trigger_%d", task.ID)
	if _, err = connection.Exec(t.Context(), fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced outbox failure'; END; $$`, functionName)); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = connection.Exec(context.Background(), "DROP FUNCTION IF EXISTS "+functionName+"() CASCADE")
	}()
	if _, err = connection.Exec(t.Context(), fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON linear_outbox FOR EACH ROW WHEN (NEW.task_id=%d) EXECUTE FUNCTION %s()`, triggerName, task.ID, functionName)); err != nil {
		t.Fatal(err)
	}
	if _, err = st.TransitionTask(t.Context(), task.ID, "verifying", "builder", "must roll back", nil); err == nil || !strings.Contains(err.Error(), "forced outbox failure") {
		t.Fatalf("forced transition error=%v", err)
	}
	after, found, err := st.GetTask(t.Context(), task.ID)
	if err != nil || !found {
		t.Fatalf("after task found=%t err=%v", found, err)
	}
	if after.State != before.State || len(after.Events) != len(before.Events) {
		t.Fatalf("task mutation escaped rollback: before=%+v after=%+v", before, after)
	}
	rows, err = st.ListLinearOutbox(t.Context(), task.ID)
	if err != nil || len(rows) != 3 {
		t.Fatalf("outbox escaped rollback: rows=%+v err=%v", rows, err)
	}
	if _, err = connection.Exec(t.Context(), "DROP TRIGGER "+triggerName+" ON linear_outbox"); err != nil {
		t.Fatal(err)
	}
	if _, err = connection.Exec(t.Context(), "DROP FUNCTION "+functionName+"()"); err != nil {
		t.Fatal(err)
	}
	if _, err = connection.Exec(t.Context(), `INSERT INTO linear_outbox(task_id,seq,op,payload,next_attempt_at,created_at,updated_at) VALUES($1,1,'issue_state','{}',now(),now(),now())`, task.ID); err == nil {
		t.Fatal("UNIQUE(task_id,seq) accepted a duplicate")
	}
}

func TestLinearOptInAndNeedsDecisionBoundaries(t *testing.T) {
	st := linearTestStore(t)
	st.EnableLinearSync()
	for name, refs := range map[string]TaskRefs{
		"absent": {},
		"false":  {Linear: &TaskLinear{Sync: false}},
	} {
		t.Run(name, func(t *testing.T) {
			task := linearTask(t, st, refs)
			if _, err := st.ClaimTask(t.Context(), task.ID, "builder"); err != nil {
				t.Fatal(err)
			}
			for _, state := range []string{"in_progress", "verifying", "merged"} {
				if _, err := st.TransitionTask(t.Context(), task.ID, state, "builder", state, nil); err != nil {
					t.Fatal(err)
				}
			}
			rows, err := st.ListLinearOutbox(t.Context(), task.ID)
			if err != nil || len(rows) != 0 {
				t.Fatalf("opt-out rows=%+v err=%v", rows, err)
			}
		})
	}
	task := linearTask(t, st, TaskRefs{Linear: &TaskLinear{Sync: true}})
	if _, err := st.ClaimTask(t.Context(), task.ID, "builder"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionTask(t.Context(), task.ID, "needs_decision", "builder", "choose", nil); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListLinearOutbox(t.Context(), task.ID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("needs_decision emitted a Linear mutation: rows=%+v err=%v", rows, err)
	}
}

func TestLinearCreateAndTerminalPayloads(t *testing.T) {
	st := linearTestStore(t)
	st.EnableLinearSync()
	refs := TaskRefs{Linear: &TaskLinear{
		Sync:      true,
		Tier:      "T3",
		Grade:     "S+",
		Brief:     "brief/connector",
		Labels:    []string{"connector", "approved"},
		Report:    "report/connector",
		Verify:    "report/connector/independent",
		Decision:  "decision/connector",
		DeploySHA: "abcdef0123456789",
	}}
	task := linearTask(t, st, refs)
	rows, err := st.ListLinearOutbox(t.Context(), task.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("create rows=%+v err=%v", rows, err)
	}
	for _, fragment := range []string{task.Title, refs.Linear.Brief, task.Lane, refs.Linear.Tier, refs.Linear.Grade, fmt.Sprintf("hk-task:%d", task.ID)} {
		if !strings.Contains(rows[0].Payload.Description, fragment) {
			t.Errorf("description missing %q: %s", fragment, rows[0].Payload.Description)
		}
	}
	wantLabels := append([]string{}, refs.Linear.Labels...)
	wantLabels = append(wantLabels, task.Lane, refs.Linear.Tier, refs.Linear.Grade)
	sort.Strings(wantLabels)
	gotLabels := append([]string{}, rows[0].Payload.Labels...)
	sort.Strings(gotLabels)
	if !reflect.DeepEqual(gotLabels, wantLabels) {
		t.Fatalf("labels=%v want=%v", gotLabels, wantLabels)
	}
	if _, err = st.ClaimTask(t.Context(), task.ID, "builder"); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"in_progress", "verifying", "merged"} {
		if _, err = st.TransitionTask(t.Context(), task.ID, state, "builder", "terminal summary", nil); err != nil {
			t.Fatal(err)
		}
	}
	rows, err = st.ListLinearOutbox(t.Context(), task.ID)
	if err != nil || len(rows) != 7 {
		t.Fatalf("terminal rows=%+v err=%v", rows, err)
	}
	last := rows[len(rows)-3:]
	if last[0].Op != LinearOpIssueState || last[1].Op != LinearOpTerminalComment || last[2].Op != LinearOpTerminalArchive {
		t.Fatalf("terminal order=%v,%v,%v", last[0].Op, last[1].Op, last[2].Op)
	}
	for _, fragment := range []string{"terminal summary", refs.Linear.Report, refs.Linear.Verify, refs.Linear.Decision, refs.Linear.DeploySHA} {
		if !strings.Contains(last[1].Payload.Comment, fragment) {
			t.Errorf("terminal comment missing %q: %s", fragment, last[1].Payload.Comment)
		}
	}
}
