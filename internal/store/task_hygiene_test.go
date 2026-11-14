package store

import (
	"fmt"
	"testing"
	"time"
)

func TestTaskHygieneSweep(t *testing.T) {
	s, pool := searchTestStore(t)

	// 1. Set up a task whose merged PR is held, then unheld so it is mechanical:
	x := flowTask(t, s, pool, "implement", StateDeveloping, true)
	first, second := testPR(9501), testPR(9502)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: first, TaskID: x.ID})
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: second, TaskID: x.ID})
	if res := send(t, s, mergeEvent(x.ID, first)); res.Applied {
		t.Fatalf("first merge applied: %+v", res)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE changes SET state='closed' WHERE repo='example/app' AND pr_number=9502`); err != nil {
		t.Fatal(err)
	}

	// 2. Set up a parent whose sub-tasks have all settled and unheld:
	parent := flowTask(t, s, pool, "implement", StateDeveloping, true)
	child := flowTask(t, s, pool, "implement", StateReviewing, true)
	setParent(t, s, child, parent)
	open := testPR(9503)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: open, TaskID: parent.ID})
	if res := send(t, s, mergeEvent(child.ID, testPR(9504))); !res.Applied {
		t.Fatalf("child merge %+v", res)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE changes SET state='closed' WHERE repo='example/app' AND pr_number=9503`); err != nil {
		t.Fatal(err)
	}

	// 3. Set up a settled task with an open decision request:
	settled := flowTask(t, s, pool, "implement", StateMerged, false)
	if _, err := pool.Exec(t.Context(), `UPDATE tasks SET refs=refs||jsonb_build_object('decision_request',jsonb_build_object('id',$2::text,'status','open')) WHERE id=$1`, settled.ID, fmt.Sprintf("dr-%d-1", settled.ID)); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()

	// Dry run: reports mechanical closes and findings, but modifies no tasks
	repDry, err := s.TaskHygiene(t.Context(), now, "", true)
	if err != nil {
		t.Fatalf("dry run failed: %v", err)
	}
	if !repDry.DryRun {
		t.Fatalf("dry run report has DryRun=false")
	}
	if len(repDry.MechanicalCloses) != 2 {
		t.Fatalf("expected 2 mechanical closes in dry run, got %d: %+v", len(repDry.MechanicalCloses), repDry.MechanicalCloses)
	}
	// Verify database tasks were not changed
	if got := stateOf(t, s, x.ID); got.State != StateReviewing {
		t.Fatalf("x changed in dry run: %s", got.State)
	}
	if got := stateOf(t, s, parent.ID); got.State != StateReviewing {
		t.Fatalf("parent changed in dry run: %s", got.State)
	}

	// Verify findings contain the settled_open_decision
	foundSettled := false
	for _, f := range repDry.Findings.SettledOpenDecisions {
		if f.ID == settled.ID {
			foundSettled = true
			break
		}
	}
	if !foundSettled {
		t.Fatalf("settled task %d not found in SettledOpenDecisions findings", settled.ID)
	}

	// Real run: applies the mechanical closes
	repRun, err := s.TaskHygiene(t.Context(), now, "", false)
	if err != nil {
		t.Fatalf("real run failed: %v", err)
	}
	if repRun.DryRun {
		t.Fatalf("real run report has DryRun=true")
	}
	if len(repRun.MechanicalCloses) != 2 {
		t.Fatalf("expected 2 mechanical closes applied, got %d: %+v", len(repRun.MechanicalCloses), repRun.MechanicalCloses)
	}
	// Verify tasks moved to merged
	if got := stateOf(t, s, x.ID); got.State != StateMerged {
		t.Fatalf("x state after real run: %s, want %s", got.State, StateMerged)
	}
	if got := stateOf(t, s, parent.ID); got.State != StateMerged {
		t.Fatalf("parent state after real run: %s, want %s", got.State, StateMerged)
	}

	// Re-run: idempotent sweep finds nothing left to mechanically close
	repAgain, err := s.TaskHygiene(t.Context(), now, "", false)
	if err != nil {
		t.Fatalf("re-run failed: %v", err)
	}
	if len(repAgain.MechanicalCloses) != 0 {
		t.Fatalf("expected 0 mechanical closes on second run, got %d: %+v", len(repAgain.MechanicalCloses), repAgain.MechanicalCloses)
	}
}

func TestTaskHygieneAutoCloseParentOff(t *testing.T) {
	s, pool := searchTestStore(t)
	s.SetAutoCloseParent(false)
	defer s.SetAutoCloseParent(true)

	// Set up a parent whose sub-tasks have all settled and unheld
	parent := flowTask(t, s, pool, "implement", StateDeveloping, true)
	child := flowTask(t, s, pool, "implement", StateReviewing, true)
	setParent(t, s, child, parent)
	open := testPR(9601)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: open, TaskID: parent.ID})
	if res := send(t, s, mergeEvent(child.ID, testPR(9602))); !res.Applied {
		t.Fatalf("child merge %+v", res)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE changes SET state='closed' WHERE repo='example/app' AND pr_number=9601`); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()

	// Dry run: does not report parent as prospective mechanical close
	repDry, err := s.TaskHygiene(t.Context(), now, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(repDry.MechanicalCloses) != 0 {
		t.Fatalf("expected 0 mechanical closes when auto_close_parent is off, got %d", len(repDry.MechanicalCloses))
	}
	// Parent should be listed under findings
	found := false
	for _, f := range repDry.Findings.AutoCloseHeldParents {
		if f.ID == parent.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("parent %d not reported under AutoCloseHeldParents findings", parent.ID)
	}

	// Apply run: does not close parent
	repRun, err := s.TaskHygiene(t.Context(), now, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(repRun.MechanicalCloses) != 0 {
		t.Fatalf("expected 0 mechanical closes in apply run when auto_close_parent is off, got %d", len(repRun.MechanicalCloses))
	}
	if got := stateOf(t, s, parent.ID); got.State != StateReviewing {
		t.Fatalf("parent state changed to %s when auto_close_parent was off", got.State)
	}
}

func TestTaskHygieneProjectScope(t *testing.T) {
	s, pool := searchTestStore(t)

	p1Task := flowTask(t, s, pool, "implement", StateDeveloping, true)
	p2Task := flowTask(t, s, pool, "implement", StateDeveloping, true)

	proj1 := "proj-one"
	proj2 := "proj-two"
	if _, err := pool.Exec(t.Context(), `UPDATE tasks SET project=$1 WHERE id=$2`, proj1, p1Task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE tasks SET project=$1 WHERE id=$2`, proj2, p2Task.ID); err != nil {
		t.Fatal(err)
	}

	// Settled open decisions on both
	if _, err := pool.Exec(t.Context(), `UPDATE tasks SET state='merged', refs=refs||jsonb_build_object('decision_request',jsonb_build_object('id',$2::text,'status','open')) WHERE id=$1`, p1Task.ID, fmt.Sprintf("dr-%d-1", p1Task.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE tasks SET state='merged', refs=refs||jsonb_build_object('decision_request',jsonb_build_object('id',$2::text,'status','open')) WHERE id=$1`, p2Task.ID, fmt.Sprintf("dr-%d-1", p2Task.ID)); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	rep, err := s.TaskHygiene(t.Context(), now, proj1, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings.SettledOpenDecisions) != 1 || rep.Findings.SettledOpenDecisions[0].ID != p1Task.ID {
		t.Fatalf("project scoped findings: %+v, want only task %d", rep.Findings.SettledOpenDecisions, p1Task.ID)
	}
}

func TestHygieneConsultFindingCollector(t *testing.T) {
	for _, rule := range []hygieneRule{hygieneMergedRule, hygieneParentRule} {
		t.Run(rule.name(), func(t *testing.T) {
			rep := TaskHygieneReport{}
			for i := 0; i < hygieneFindingsLimit; i++ {
				hygieneAddFinding(&rep, rule, TaskDriftTask{ID: int64(i + 1), Mechanical: true, Hold: "current recheck hold"})
			}
			get := func() ([]TaskDriftTask, bool) {
				if rule == hygieneMergedRule {
					return rep.Findings.MergedPROpenTasks, rep.Findings.MergedPROpenTasksTruncated
				}
				return rep.Findings.AutoCloseHeldParents, rep.Findings.AutoCloseHeldParentsTruncated
			}
			rows, truncated := get()
			if len(rows) != 200 || truncated {
				t.Fatalf("exact cap: %d rows, truncated=%v", len(rows), truncated)
			}
			for _, row := range rows {
				if row.Mechanical || row.Hold != "current recheck hold" {
					t.Fatalf("incorrect finding: %+v", row)
				}
			}
			hygieneAddFinding(&rep, rule, TaskDriftTask{ID: 201, Mechanical: true})
			rows, truncated = get()
			if len(rows) != 200 || !truncated {
				t.Fatalf("overflow: %d rows, truncated=%v", len(rows), truncated)
			}
		})
	}
}
