package store

import (
	"errors"
	"strings"
	"testing"
)

// TestFixCompletedWithoutOwnChange (hk 5256): a fix task resolved by other
// work — an operator action, another change — closes as completed on closure
// evidence when it has no implements change of its own in flight or merged.
func TestFixCompletedWithoutOwnChange(t *testing.T) {
	s := schemaStore(t, "fix_complete")
	for _, from := range []string{StateBacklog, StateUnstarted, StateResearching, StateDeveloping, StateReviewing} {
		x := flowTask(t, s, s.pool, "fix", from, true)
		got, err := s.TransitionTaskWith(t.Context(), x.ID, TransitionInput{To: StateCompleted, By: "tester", Evidence: "fixed by the YouTrack schema apply"})
		if err != nil || got.State != StateCompleted {
			t.Fatalf("%s -> completed: state=%s err=%v", from, got.State, err)
		}
		if got.Refs.Closure == nil || got.Refs.Closure.Evidence != "fixed by the YouTrack schema apply" || got.CompletedAt == nil {
			t.Fatalf("%s -> completed: %+v", from, got)
		}
	}
}

// TestFixCompletedNeedsEvidence: the completed closure rules apply to fix
// like every other kind — no evidence, no close.
func TestFixCompletedNeedsEvidence(t *testing.T) {
	s := schemaStore(t, "fix_complete_evidence")
	x := flowTask(t, s, s.pool, "fix", StateDeveloping, true)
	if _, err := s.TransitionTaskWith(t.Context(), x.ID, TransitionInput{To: StateCompleted, By: "tester"}); !errors.Is(err, ErrClosureInvalid) {
		t.Fatalf("completed without evidence: err=%v, want ErrClosureInvalid", err)
	}
	if got := stateOf(t, s, x.ID); got.State != StateDeveloping {
		t.Fatalf("refused close moved the task: %s", got.State)
	}
}

// TestFixCompletedWaitsForItsPR: a fix task whose own PR is open or merged
// closes through the merged/deployed path, not by hand — a revert still
// leaves the merged change on record, so relanding takes a new PR. A closed
// change delivered nothing and does not block.
func TestFixCompletedWaitsForItsPR(t *testing.T) {
	s := schemaStore(t, "fix_complete_pr")
	complete := func(id int64) error {
		_, err := s.TransitionTaskWith(t.Context(), id, TransitionInput{To: StateCompleted, By: "tester", Evidence: "https://example.com/result"})
		return err
	}

	x := flowTask(t, s, s.pool, "fix", StateReviewing, true)
	pr := testPR(7201)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: pr, TaskID: x.ID, PR: pr})
	if err := complete(x.ID); !errors.Is(err, ErrTaskConflict) || !strings.Contains(err.Error(), "example/app#7201") {
		t.Fatalf("completed with an open PR: err=%v, want task_conflict naming example/app#7201", err)
	}

	// The merge event closes the task as merged; reopened, the fix still
	// cannot complete by hand — its merged change must run the
	// merged/deployed path again.
	if got := send(t, s, mergeEvent(x.ID, pr)); got.Task.State != StateMerged {
		t.Fatalf("merge event state=%s reason=%q", got.Task.State, got.Reason)
	}
	if _, err := s.ReopenTask(t.Context(), x.ID, "tester", "reland needed"); err != nil {
		t.Fatal(err)
	}
	if err := complete(x.ID); !errors.Is(err, ErrTaskConflict) || !strings.Contains(err.Error(), "example/app#7201") {
		t.Fatalf("completed with a merged PR: err=%v, want task_conflict naming example/app#7201", err)
	}

	// A revert leaves the merged change on record and relanding takes a new
	// PR: the fix still cannot complete by hand.
	send(t, s, revertEvent(x.ID, pr, testPR(15201)))
	if err := complete(x.ID); !errors.Is(err, ErrTaskConflict) || !strings.Contains(err.Error(), "example/app#7201") {
		t.Fatalf("completed after its PR was reverted: err=%v, want task_conflict naming example/app#7201", err)
	}

	// The reland is a new PR: its merge closes the task as merged and the
	// fix completes at the end of that path.
	pr2 := testPR(7204)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: pr2, TaskID: x.ID, PR: pr2})
	if got := send(t, s, mergeEvent(x.ID, pr2)); got.Task.State != StateMerged {
		t.Fatalf("reland merge event state=%s reason=%q", got.Task.State, got.Reason)
	}
	if err := complete(x.ID); err != nil {
		t.Fatalf("completed after reland: %v", err)
	}

	// A fix whose PR closed unmerged has no live delivery and completes too.
	y := flowTask(t, s, s.pool, "fix", StateDeveloping, true)
	closedPR := testPR(7202)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: closedPR, TaskID: y.ID, PR: closedPR})
	if _, err := s.pool.Exec(t.Context(), `UPDATE changes SET state='closed' WHERE repo='example/app' AND pr_number=7202`); err != nil {
		t.Fatal(err)
	}
	if err := complete(y.ID); err != nil {
		t.Fatalf("completed with only a closed PR: %v", err)
	}
}

// TestFixMergedStillCompletes: merged -> completed is the end of the
// merged/deployed path, so the fix guard does not hold it — a merged fix
// task closes on evidence even while its change record shows merged.
func TestFixMergedStillCompletes(t *testing.T) {
	s := schemaStore(t, "fix_complete_merged")
	x := flowTask(t, s, s.pool, "fix", StateReviewing, true)
	pr := testPR(7203)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: pr, TaskID: x.ID, PR: pr})
	if got := send(t, s, mergeEvent(x.ID, pr)); got.Task.State != StateMerged {
		t.Fatalf("merge event state=%s reason=%q", got.Task.State, got.Reason)
	}
	got, err := s.TransitionTaskWith(t.Context(), x.ID, TransitionInput{To: StateCompleted, By: "tester", Evidence: "https://example.com/result"})
	if err != nil || got.State != StateCompleted {
		t.Fatalf("merged -> completed: state=%s err=%v", got.State, err)
	}
}
