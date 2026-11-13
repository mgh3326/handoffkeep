package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestImplementCompletedWaitsForItsWork (hk 4050): an implement task completes
// only when every change it implements is merged and every sub-task is
// completed or canceled. A refusal names the open PR or the open sub-task.
func TestImplementCompletedWaitsForItsWork(t *testing.T) {
	s := schemaStore(t, "implement_blockers")
	complete := func(id int64) error {
		_, err := s.TransitionTaskWith(t.Context(), id, TransitionInput{To: StateCompleted, By: "tester", Evidence: "https://example.com/result"})
		return err
	}

	x := flowTask(t, s, s.pool, "implement", StateReviewing, true)
	pr := testPR(7001)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: pr, TaskID: x.ID, PR: pr})
	if err := complete(x.ID); !errors.Is(err, ErrTaskConflict) || !strings.Contains(err.Error(), "example/app#7001") {
		t.Fatalf("completed with an open PR: err=%v, want task_conflict naming example/app#7001", err)
	}
	send(t, s, mergeEvent(x.ID, pr))
	if err := complete(x.ID); err != nil {
		t.Fatalf("completed after its PR merged: %v", err)
	}

	y := flowTask(t, s, s.pool, "implement", StateReviewing, true)
	child := flowTask(t, s, s.pool, "decide", StateDeveloping, true)
	setParent(t, s, child, y)
	if err := complete(y.ID); !errors.Is(err, ErrTaskConflict) || !strings.Contains(err.Error(), fmt.Sprintf("open sub-task %d", child.ID)) {
		t.Fatalf("completed with an open sub-task: err=%v, want task_conflict naming sub-task %d", err, child.ID)
	}
	if err := complete(child.ID); err != nil {
		t.Fatalf("sub-task completed: %v", err)
	}
	if err := complete(y.ID); err != nil {
		t.Fatalf("completed with no open PR or sub-task: %v", err)
	}
}
