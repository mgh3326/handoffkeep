package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestTaskClosureMigration(t *testing.T) {
	s := schemaStore(t, "task_closure")
	var version int
	if err := s.pool.QueryRow(t.Context(), `SELECT max(version) FROM schema_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("version=%d err=%v", version, err)
	}
	var activityColumn bool
	if err := s.pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='tasks' AND column_name='last_activity_at')`).Scan(&activityColumn); err != nil || !activityColumn {
		t.Fatalf("last activity migration applied=%t err=%v", activityColumn, err)
	}
	var jobContextColumn bool
	if err := s.pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='jobs' AND column_name='harness')`).Scan(&jobContextColumn); err != nil || !jobContextColumn {
		t.Fatalf("job context migration applied=%t err=%v", jobContextColumn, err)
	}
	var denialColumn bool
	if err := s.pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='decision_answer_attempts' AND column_name='denies_attempt_id')`).Scan(&denialColumn); err != nil || !denialColumn {
		t.Fatalf("decision denial migration applied=%t err=%v", denialColumn, err)
	}
	var sessionColumn bool
	if err := s.pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='decision_answer_attempts' AND column_name='operator_quote')`).Scan(&sessionColumn); err != nil || !sessionColumn {
		t.Fatalf("decision session migration applied=%t err=%v", sessionColumn, err)
	}
	for _, table := range []string{"task_origins", "changes", "decision_resolutions", "decision_deliveries", "verifications", "dots_results", "change_checks", "change_threads", "change_findings"} {
		var exists bool
		if err := s.pool.QueryRow(t.Context(), `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil || !exists {
			t.Fatalf("preceding table %s exists=%t err=%v", table, exists, err)
		}
	}
	x, err := s.CreateTask(t.Context(), Task{Lane: relaneLane(t, "closure"), Title: "closure", Kind: "decide", CreatedBy: "tester", Project: projectPtr(testProjectName)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE tasks SET state='completed' WHERE id=$1`, x.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(t.Context(), `UPDATE tasks SET refs='{"closure":{"type":"bogus","evidence":"x"}}' WHERE id=$1`, x.ID)
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "23514" {
		t.Fatalf("constraint error=%v", err)
	}
}

func TestCompletedKindGate(t *testing.T) {
	s := schemaStore(t, "completed_kind")
	for _, kind := range []string{"decide", "ops", "verify", "implement", "fix"} {
		for _, from := range []string{StateBacklog, StateUnstarted, StateResearching, StateDeveloping, StateReviewing} {
			t.Run(kind+"/"+from, func(t *testing.T) {
				x := flowTask(t, s, s.pool, kind, from, true)
				got, err := s.TransitionTaskWith(t.Context(), x.ID, TransitionInput{To: StateCompleted, By: "tester", Evidence: "https://example.com/result"})
				if kind == "implement" || kind == "fix" {
					if !errors.Is(err, ErrTaskConflict) || err.Error() != "task_conflict: completed applies to decide, ops and verify tasks" {
						t.Fatalf("err=%v", err)
					}
					return
				}
				if err != nil || got.State != StateCompleted {
					t.Fatalf("state=%s err=%v", got.State, err)
				}
			})
		}
	}
	x := flowTask(t, s, s.pool, "decide", StateResearching, true)
	if _, err := s.pool.Exec(t.Context(), `UPDATE tasks SET awaiting_decision=true WHERE id=$1`, x.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTaskWith(t.Context(), x.ID, TransitionInput{To: StateCompleted, By: "tester", Evidence: "https://example.com/result"}); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("flagged err=%v", err)
	}
}

func TestTaskClosure(t *testing.T) {
	for _, to := range []string{StateMerged, StateDeployed, StateCompleted, StateDropped, StateDuplicate} {
		for _, typ := range []string{"resolved", "cancelled", "obsolete", "superseded", "bogus"} {
			t.Run(fmt.Sprintf("%s/%s", to, typ), func(t *testing.T) {
				valid := ((to == StateMerged || to == StateDeployed || to == StateCompleted) && typ == "resolved") || (to == StateDropped && (typ == "cancelled" || typ == "obsolete" || typ == "superseded")) || (to == StateDuplicate && typ == "superseded")
				x := Task{}
				err := applyLifecycle(&x, StateReviewing, to, time.Now().UTC(), &TaskClosure{Type: typ, Evidence: "https://example.com/r"})
				if valid && (err != nil || x.Refs.Closure == nil) {
					t.Fatalf("valid closure: %+v %v", x.Refs.Closure, err)
				}
				if !valid && !errors.Is(err, ErrClosureInvalid) {
					t.Fatalf("invalid closure: %v", err)
				}
			})
		}
	}
	for _, tc := range []struct {
		evidence string
		ok       bool
	}{{"notes", false}, {"hk:doc research/x", true}} {
		x := Task{}
		err := applyLifecycle(&x, StateBacklog, StateCompleted, time.Now().UTC(), &TaskClosure{Type: "resolved", Evidence: tc.evidence})
		if (err == nil) != tc.ok {
			t.Fatalf("%q: %v", tc.evidence, err)
		}
	}
	t.Run("settled transitions retain and reopen clears closure", func(t *testing.T) {
		now := time.Now().UTC()
		x := Task{}
		if err := applyLifecycle(&x, StateReviewing, StateMerged, now, &TaskClosure{Type: "resolved", Evidence: "  result  "}); err != nil {
			t.Fatal(err)
		}
		if x.Refs.Closure == nil || x.Refs.Closure.Evidence != "result" {
			t.Fatalf("closure=%+v", x.Refs.Closure)
		}
		if err := applyLifecycle(&x, StateMerged, StateDeployed, now, &TaskClosure{Type: "bogus", Evidence: "ignored"}); err != nil {
			t.Fatal(err)
		}
		if x.Refs.Closure.Evidence != "result" {
			t.Fatalf("closure changed: %+v", x.Refs.Closure)
		}
		if err := applyLifecycle(&x, StateDeployed, StateUnstarted, now, nil); err != nil || x.Refs.Closure != nil {
			t.Fatalf("reopen closure=%+v err=%v", x.Refs.Closure, err)
		}
	})
	for _, evidence := range []string{" ", strings.Repeat("x", 1025)} {
		x := Task{}
		if err := applyLifecycle(&x, StateBacklog, StateDropped, time.Now().UTC(), &TaskClosure{Type: "cancelled", Evidence: evidence}); !errors.Is(err, ErrClosureInvalid) {
			t.Fatalf("invalid evidence length: %v", err)
		}
	}
	t.Run("history", func(t *testing.T) {
		s := schemaStore(t, "closure_history")
		x := flowTask(t, s, s.pool, "decide", StateBacklog, false)
		if _, err := s.TransitionTaskWith(t.Context(), x.ID, TransitionInput{To: StateCompleted, By: "tester", Evidence: "https://example.com/r", Refs: &TaskRefs{Closure: &TaskClosure{Type: "resolved", Evidence: "x"}}}); err == nil {
			t.Fatal("refs patch accepted")
		}
		if _, err := s.TransitionTaskWith(t.Context(), x.ID, TransitionInput{To: StateCompleted, By: "tester", Evidence: "https://example.com/r"}); err != nil {
			t.Fatal(err)
		}
		y, err := s.ReopenTask(t.Context(), x.ID, "tester", "again")
		if err != nil || y.Refs.Closure != nil {
			t.Fatalf("reopen %+v %v", y.Refs.Closure, err)
		}
		y = stateOf(t, s, x.ID)
		found := false
		for _, ev := range y.Events {
			if ev.To == StateCompleted && ev.Refs != nil && ev.Refs.Closure != nil {
				found = true
			}
		}
		if !found {
			t.Fatal("closing event lost closure")
		}
	})
}

func TestEveryCloseRecordsClosure(t *testing.T) {
	s := schemaStore(t, "every_close")
	check := func(t *testing.T, x Task, state, typ string) {
		t.Helper()
		if x.State != state || x.Refs.Closure == nil || x.Refs.Closure.Type != typ {
			t.Fatalf("task state=%s closure=%+v", x.State, x.Refs.Closure)
		}
	}
	for _, tc := range []struct{ name, kind, from, to, typ string }{
		{"manual completed", "decide", StateBacklog, StateCompleted, "resolved"},
		{"manual merged", "implement", StateReviewing, StateMerged, "resolved"},
		{"manual dropped", "implement", StateBacklog, StateDropped, "cancelled"},
		{"manual obsolete", "implement", StateBacklog, StateDropped, "obsolete"},
		{"duplicate", "implement", StateBacklog, StateDuplicate, "superseded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := flowTask(t, s, s.pool, tc.kind, tc.from, true)
			in := TransitionInput{To: tc.to, By: "tester", Note: "reason", Evidence: "https://example.com/r"}
			if tc.name == "manual obsolete" {
				in.Closure = "obsolete"
			}
			if tc.to == StateDuplicate {
				in.DuplicateOf = flowTask(t, s, s.pool, "fix", StateBacklog, false).ID
			}
			got, err := s.TransitionTaskWith(t.Context(), x.ID, in)
			if err != nil {
				t.Fatal(err)
			}
			check(t, got, tc.to, tc.typ)
		})
	}
	t.Run("mergewake merge and revert", func(t *testing.T) {
		x := flowTask(t, s, s.pool, "implement", StateReviewing, true)
		pr := testPR(503)
		got := send(t, s, mergeEvent(x.ID, pr))
		check(t, got.Task, StateMerged, "resolved")
		reverted := send(t, s, revertEvent(x.ID, pr, testPR(1503)))
		if reverted.Task.Refs.Closure != nil {
			t.Fatalf("revert retained closure: %+v", reverted.Task.Refs.Closure)
		}
	})
	for _, tc := range []struct{ name, second, want string }{{"all completed", StateCompleted, StateCompleted}, {"mixed", StateMerged, StateMerged}} {
		t.Run("auto close "+tc.name, func(t *testing.T) {
			parent := flowTask(t, s, s.pool, "ops", StateBacklog, false)
			a := flowTask(t, s, s.pool, "decide", StateBacklog, false)
			b := flowTask(t, s, s.pool, "ops", StateBacklog, false)
			setParent(t, s, a, parent)
			setParent(t, s, b, parent)
			if _, err := s.TransitionTaskWith(t.Context(), a.ID, TransitionInput{To: StateCompleted, By: "tester", Evidence: "hk:doc research/x"}); err != nil {
				t.Fatal(err)
			}
			if tc.second == StateMerged {
				if _, err := s.pool.Exec(t.Context(), `UPDATE tasks SET state='reviewing' WHERE id=$1`, b.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.TransitionTaskWith(t.Context(), b.ID, TransitionInput{To: tc.second, By: "tester", Evidence: "https://example.com/r"}); err != nil {
				t.Fatal(err)
			}
			check(t, stateOf(t, s, parent.ID), tc.want, "resolved")
		})
	}
	for _, tc := range []struct{ key, state, typ string }{{"A", StateMerged, "resolved"}, {"E", StateDropped, "cancelled"}} {
		t.Run("disposition "+tc.state, func(t *testing.T) {
			merged := time.Now().UTC().Add(-time.Hour)
			x, _, err := s.CreateDisposition(t.Context(), DispositionInput{Lane: relaneLane(t, "disposition"), Project: testProjectName, Title: "disposition", OriginPR: testPR(map[string]int{"A": 801, "E": 802}[tc.key]), MergeSHA: strings.Repeat("a", 40), MergedAt: &merged, Install: DispositionInstall{State: "unknown"}, Recommended: tc.key, CreatedBy: "tester"})
			if err != nil {
				t.Fatal(err)
			}
			x = stateOf(t, s, x.ID)
			_, err = s.AnswerDisposition(t.Context(), DispositionAnswerInput{ID: x.ID, Gen: x.Events[len(x.Events)-1].ID, Key: tc.key, OperatorEmail: "tester@example.com", EventID: "answer-" + tc.key})
			if err != nil {
				t.Fatal(err)
			}
			x, err = s.ApplyDisposition(t.Context(), x.ID, "tester", "")
			if err != nil {
				t.Fatal(err)
			}
			check(t, x, tc.state, tc.typ)
			ev := lastEvent(t, s, x.ID)
			if ev.Refs == nil || ev.Refs.Closure == nil {
				t.Fatal("closing event has no closure")
			}
		})
	}
	t.Run("triage decline", func(t *testing.T) {
		x, _, err := s.IntakeTask(t.Context(), IntakeInput{Source: "dots", SourceRef: "closure-decline", Title: "decline", CreatedBy: "tester"})
		if err != nil {
			t.Fatal(err)
		}
		x, err = s.TransitionTaskWith(t.Context(), x.ID, TransitionInput{To: StateDropped, By: "tester", Note: "not needed"})
		if err != nil {
			t.Fatal(err)
		}
		check(t, x, StateDropped, "cancelled")
	})
	t.Run("ticket set cascade", func(t *testing.T) {
		set, _, err := s.EmitTicketSet(t.Context(), ticketSetFixture("closure"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.TransitionTaskWith(t.Context(), set.ParentID, TransitionInput{To: StateDropped, By: "tester", Note: "not needed"}); err != nil {
			t.Fatal(err)
		}
		for _, child := range set.Children {
			check(t, stateOf(t, s, child.ID), StateDropped, "cancelled")
		}
	})
}

func TestAutoCloseCompletedParentKinds(t *testing.T) {
	s := schemaStore(t, "auto_close_completed_kind")
	for _, kind := range []string{"decide", "ops", "verify", "implement", "fix"} {
		t.Run(kind, func(t *testing.T) {
			parent := flowTask(t, s, s.pool, kind, StateBacklog, false)
			child := flowTask(t, s, s.pool, "decide", StateBacklog, false)
			setParent(t, s, child, parent)
			closed, err := s.TransitionTaskWith(t.Context(), child.ID, TransitionInput{To: StateCompleted, By: "tester", Evidence: "hk:doc research/x"})
			if err != nil || closed.State != StateCompleted {
				t.Fatalf("child state=%s err=%v", closed.State, err)
			}
			got := stateOf(t, s, parent.ID)
			if kind == "implement" || kind == "fix" {
				if got.State != StateBacklog || got.Refs.Closure != nil {
					t.Fatalf("parent changed: state=%s closure=%+v", got.State, got.Refs.Closure)
				}
				events := eventsOfKind(got, TaskEventAutoClose)
				if len(events) != 1 || events[0].Applied || events[0].From != StateBacklog || events[0].To != StateBacklog {
					t.Fatalf("skip event=%+v", events)
				}
				return
			}
			if got.State != StateCompleted || got.Refs.Closure == nil || got.Refs.Closure.Type != "resolved" {
				t.Fatalf("parent state=%s closure=%+v", got.State, got.Refs.Closure)
			}
		})
	}
	t.Run("implement with merged child", func(t *testing.T) {
		parent := flowTask(t, s, s.pool, "implement", StateBacklog, false)
		a := flowTask(t, s, s.pool, "decide", StateBacklog, false)
		b := flowTask(t, s, s.pool, "implement", StateReviewing, true)
		setParent(t, s, a, parent)
		setParent(t, s, b, parent)
		if _, err := s.TransitionTaskWith(t.Context(), a.ID, TransitionInput{To: StateCompleted, By: "tester", Evidence: "hk:doc research/x"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.TransitionTaskWith(t.Context(), b.ID, TransitionInput{To: StateMerged, By: "tester", Evidence: "merged change"}); err != nil {
			t.Fatal(err)
		}
		if got := stateOf(t, s, parent.ID); got.State != StateMerged {
			t.Fatalf("parent state=%s, want merged", got.State)
		}
	})
}
