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
	if got := stateOf(t, s, x.ID); got.State != StateDeveloping {
		t.Fatalf("x changed in dry run: %s", got.State)
	}
	if got := stateOf(t, s, parent.ID); got.State != StateDeveloping {
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
