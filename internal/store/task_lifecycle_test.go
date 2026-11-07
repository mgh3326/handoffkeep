package store

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
)

const lifecycleEvidence = "https://github.com/example/repo/pull/7"

// TestCloseTaskMergedPaths covers the one-call close: reviewing closes with
// one event, developing and researching record the intermediate reviewing
// step and then merged in the same call.
func TestCloseTaskMergedPaths(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	for _, tc := range []struct {
		state  string
		path   []string
		events int
	}{
		{"developing", []string{StateDeveloping, StateReviewing, StateMerged}, 2},
		{"researching", []string{StateResearching, StateReviewing, StateMerged}, 2},
		{"reviewing", []string{StateReviewing, StateMerged}, 1},
	} {
		t.Run(tc.state, func(t *testing.T) {
			x := flowTask(t, s, pool, "implement", tc.state, true)
			before := len(x.Events)
			got, path, err := s.CloseTaskMerged(ctx, x.ID, lifecycleEvidence, "tester", "closed by operator", "close-"+tc.state)
			if err != nil {
				t.Fatalf("close from %s: %v", tc.state, err)
			}
			if got.State != StateMerged {
				t.Fatalf("state=%q want merged", got.State)
			}
			if !slices.Equal(path, tc.path) {
				t.Fatalf("path=%v want %v", path, tc.path)
			}
			if got.Refs.Closure == nil || got.Refs.Closure.Type != "resolved" || got.Refs.Closure.Evidence != lifecycleEvidence {
				t.Fatalf("closure=%+v", got.Refs.Closure)
			}
			events := stateOf(t, s, x.ID).Events
			if len(events)-before != tc.events {
				t.Fatalf("recorded %d events, want %d: %+v", len(events)-before, tc.events, events[before:])
			}
			last := events[len(events)-1]
			if last.From != StateReviewing || last.To != StateMerged || last.Source != lifecycleSourceCloseMerged || last.SourceRef != "close-"+tc.state {
				t.Fatalf("last event=%+v", last)
			}
			if tc.events == 2 {
				step := events[len(events)-2]
				if step.From != tc.state || step.To != StateReviewing || step.Source != lifecycleSourceCloseMerged || step.SourceRef != "close-"+tc.state+":reviewing" {
					t.Fatalf("intermediate event=%+v", step)
				}
			}
		})
	}
}

func TestCloseTaskMergedEvidenceDefaultsAndRefusals(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()

	withPR := flowTask(t, s, pool, "implement", "developing", true)
	if _, err := pool.Exec(ctx, `UPDATE tasks SET refs=refs||'{"pr":"https://github.com/example/repo/pull/9"}'::jsonb WHERE id=$1`, withPR.ID); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.CloseTaskMerged(ctx, withPR.ID, "", "tester", "", "close-default")
	if err != nil {
		t.Fatalf("default evidence: %v", err)
	}
	if got.Refs.Closure == nil || got.Refs.Closure.Evidence != "https://github.com/example/repo/pull/9" {
		t.Fatalf("evidence defaulted to %+v, want the task's PR", got.Refs.Closure)
	}

	noEvidence := flowTask(t, s, pool, "implement", "developing", true)
	if _, _, err := s.CloseTaskMerged(ctx, noEvidence.ID, "", "tester", "", "close-empty"); !errors.Is(err, ErrClosureInvalid) {
		t.Fatalf("empty evidence err=%v, want ErrClosureInvalid", err)
	}

	notStarted := flowTask(t, s, pool, "implement", "backlog", false)
	if _, _, err := s.CloseTaskMerged(ctx, notStarted.ID, lifecycleEvidence, "tester", "nope", "close-backlog"); !errors.Is(err, ErrTaskMergePath) {
		t.Fatalf("backlog err=%v, want ErrTaskMergePath", err)
	}
	if after := stateOf(t, s, notStarted.ID); after.State != StateBacklog {
		t.Fatalf("refused close changed state to %q", after.State)
	}
}

func TestCloseTaskMergedReplay(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	x := flowTask(t, s, pool, "implement", "developing", true)
	first, firstPath, err := s.CloseTaskMerged(ctx, x.ID, lifecycleEvidence, "tester", "first", "replay-close")
	if err != nil {
		t.Fatal(err)
	}
	events := len(stateOf(t, s, x.ID).Events)
	again, againPath, err := s.CloseTaskMerged(ctx, x.ID, "https://example.com/second", "tester", "second", "replay-close")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if again.State != StateMerged || !slices.Equal(againPath, firstPath) {
		t.Fatalf("replay state=%q path=%v, first %v", again.State, againPath, firstPath)
	}
	if again.Refs.Closure == nil || again.Refs.Closure.Evidence != first.Refs.Closure.Evidence {
		t.Fatalf("replay rewrote closure: %+v", again.Refs.Closure)
	}
	if after := len(stateOf(t, s, x.ID).Events); after != events {
		t.Fatalf("replay recorded %d extra events", after-events)
	}
}

func TestCloseTaskCompleted(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	x := flowTask(t, s, pool, "ops", "reviewing", true)
	got, err := s.CloseTaskCompleted(ctx, x.ID, "https://example.com/run/1", "tester", "done", "complete-1")
	if err != nil {
		t.Fatalf("completed: %v", err)
	}
	if got.State != StateCompleted || got.Refs.Closure == nil || got.Refs.Closure.Evidence != "https://example.com/run/1" {
		t.Fatalf("completed=%+v", got)
	}

	implement := flowTask(t, s, pool, "implement", "reviewing", true)
	if _, err := s.CloseTaskCompleted(ctx, implement.ID, "https://example.com/run/2", "tester", "done", "complete-2"); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("implement close err=%v, want ErrTaskConflict", err)
	}

	badEvidence := flowTask(t, s, pool, "decide", "reviewing", true)
	if _, err := s.CloseTaskCompleted(ctx, badEvidence.ID, "not-a-link", "tester", "done", "complete-3"); !errors.Is(err, ErrClosureInvalid) {
		t.Fatalf("bad evidence err=%v, want ErrClosureInvalid", err)
	}
}

func TestDropAndDuplicateTask(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	dropped := flowTask(t, s, pool, "implement", "developing", true)
	got, err := s.DropTask(ctx, dropped.ID, "obsolete", "no longer needed", "tester", "drop-1")
	if err != nil {
		t.Fatalf("drop: %v", err)
	}
	if got.State != StateDropped || got.Refs.Closure == nil || got.Refs.Closure.Type != "obsolete" || got.Refs.Closure.Evidence != "no longer needed" {
		t.Fatalf("dropped=%+v closure=%+v", got, got.Refs.Closure)
	}
	if _, err := s.DropTask(ctx, dropped.ID, "resolved", "x", "tester", "drop-bad"); !errors.Is(err, ErrClosureInvalid) {
		t.Fatalf("bad closure err=%v, want ErrClosureInvalid", err)
	}

	target := flowTask(t, s, pool, "implement", "backlog", false)
	dup := flowTask(t, s, pool, "implement", "developing", true)
	duplicate, err := s.DuplicateTask(ctx, dup.ID, target.ID, "same as the target", "tester", "duplicate-1")
	if err != nil {
		t.Fatalf("duplicate: %v", err)
	}
	if duplicate.State != StateDuplicate || duplicate.Refs.Closure == nil || duplicate.Refs.Closure.Type != "superseded" {
		t.Fatalf("duplicate=%+v closure=%+v", duplicate, duplicate.Refs.Closure)
	}
	if duplicate.Refs.Closure.Evidence != "duplicate of #"+strconv.FormatInt(target.ID, 10) {
		t.Fatalf("evidence=%q", duplicate.Refs.Closure.Evidence)
	}
	if live, err := hasLiveRelationTx(ctx, pool, dup.ID, RelationDuplicateOf); err != nil || !live {
		t.Fatalf("duplicate_of live=%t err=%v", live, err)
	}
	if _, err := s.DuplicateTask(ctx, dup.ID+999, target.ID, "missing", "tester", "duplicate-missing"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing task err=%v, want ErrTaskNotFound", err)
	}
}

func TestHoldTaskAndRelease(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	x := flowTask(t, s, pool, "implement", "backlog", false)
	held, err := s.HoldTask(ctx, x.ID, true, "", "tester", "보류", "hold-1")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if held.Previous != StateBacklog || held.State != StateIcebox || stateOf(t, s, x.ID).State != StateIcebox {
		t.Fatalf("held=%+v state=%q", held, stateOf(t, s, x.ID).State)
	}
	replay, err := s.HoldTask(ctx, x.ID, false, "", "tester", "다른", "hold-1")
	if err != nil || replay != held {
		t.Fatalf("replay=%+v err=%v, want %+v", replay, err, held)
	}
	released, err := s.HoldTask(ctx, x.ID, false, StateUnstarted, "tester", "보류 해제", "hold-2")
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if released.Previous != StateIcebox || released.State != StateUnstarted {
		t.Fatalf("released=%+v", released)
	}
	notHeld := flowTask(t, s, pool, "implement", "backlog", false)
	if _, err := s.HoldTask(ctx, notHeld.ID, false, "", "tester", "", "hold-3"); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("release backlog err=%v, want ErrTaskConflict", err)
	}
}

func TestReopenTaskByReplay(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	x := flowTask(t, s, pool, "implement", "merged", true)
	got, err := s.ReopenTaskBy(ctx, x.ID, "tester", "reverted", "reopen-1")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got.State != StateUnstarted || got.ClaimedBy != "" {
		t.Fatalf("reopened=%+v", got)
	}
	events := len(stateOf(t, s, x.ID).Events)
	again, err := s.ReopenTaskBy(ctx, x.ID, "tester", "again", "reopen-1")
	if err != nil || again.State != StateUnstarted {
		t.Fatalf("replay=%+v err=%v", again, err)
	}
	if after := len(stateOf(t, s, x.ID).Events); after != events {
		t.Fatalf("replay recorded %d extra events", after-events)
	}

	backlog := flowTask(t, s, pool, "implement", "backlog", false)
	if _, err := s.ReopenTaskBy(ctx, backlog.ID, "tester", "not closed", "reopen-2"); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("reopen backlog err=%v, want ErrTaskConflict", err)
	}
}

func TestOperatorRelaneTaskReplay(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	to := relaneLane(t, "lifecycle-to")
	relaneTask(t, s, to, "lane anchor")
	x := flowTask(t, s, pool, "implement", "backlog", false)
	moved, err := s.OperatorRelaneTask(ctx, x.ID, to, "tester", "운영자 이동", "relane-1", "")
	if err != nil {
		t.Fatalf("relane: %v", err)
	}
	if !moved.Changed || moved.Previous != x.Lane || moved.Lane != to {
		t.Fatalf("moved=%+v", moved)
	}
	events := len(stateOf(t, s, x.ID).Events)
	replay, err := s.OperatorRelaneTask(ctx, x.ID, to, "tester", "again", "relane-1", "")
	if err != nil || !replay.Changed || replay.Previous != x.Lane || replay.Lane != to {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if after := len(stateOf(t, s, x.ID).Events); after != events {
		t.Fatalf("replay recorded %d extra events", after-events)
	}
	last := lastEvent(t, s, x.ID)
	if last.Kind != TaskEventRelane || last.Source != lifecycleSourceRelane || last.SourceRef != "relane-1" {
		t.Fatalf("relane event=%+v", last)
	}

	other := flowTask(t, s, pool, "implement", "backlog", false)
	if _, err := s.OperatorRelaneTask(ctx, other.ID, "no-such-lane-xyz", "tester", "typo", "relane-2", ""); !errors.Is(err, ErrTaskLaneUnknown) {
		t.Fatalf("unknown lane err=%v, want ErrTaskLaneUnknown", err)
	}
}

// TestRelaneUndoRestoresEmptiedLane covers the undo of a move that emptied
// its source lane: the lane no longer has a tasks/parent_lane/relay/chat row,
// so only the recorded from-lane of the relane being undone may restore it.
func TestRelaneUndoRestoresEmptiedLane(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	from, to, elsewhere := relaneLane(t, "undo-from"), relaneLane(t, "undo-to"), relaneLane(t, "undo-elsewhere")
	relaneTask(t, s, to, "lane anchor")
	x, err := s.CreateTask(ctx, Task{Lane: from, Title: "sole member", Kind: "implement", Priority: 3, CreatedBy: "tester", Project: projectPtr(testProjectName)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.OperatorRelaneTask(ctx, x.ID, to, "tester", "move", "undo-1", ""); err != nil {
		t.Fatalf("forward relane: %v", err)
	}
	if lanes, err := s.ListKnownTaskLanes(ctx); err != nil {
		t.Fatal(err)
	} else if slices.Contains(lanes, from) {
		t.Fatalf("source lane %q is still known after the move: %v", from, lanes)
	}
	restored, err := s.OperatorRelaneTask(ctx, x.ID, from, "tester", "undo", "undo-2", "undo-1")
	if err != nil {
		t.Fatalf("undo: %v", err)
	}
	if restored.Lane != from || restored.Previous != to || stateOf(t, s, x.ID).Lane != from {
		t.Fatalf("restored=%+v lane=%q", restored, stateOf(t, s, x.ID).Lane)
	}

	// A restore key from another lane does not open this now-empty lane.
	if _, err := s.OperatorRelaneTask(ctx, x.ID, to, "tester", "move again", "undo-3", ""); err != nil {
		t.Fatalf("second move: %v", err)
	}
	other := relaneTask(t, s, elsewhere, "other lane member")
	if _, err := s.OperatorRelaneTask(ctx, other.ID, to, "tester", "move other", "undo-4", ""); err != nil {
		t.Fatalf("other move: %v", err)
	}
	if _, err := s.OperatorRelaneTask(ctx, x.ID, from, "tester", "bad undo", "undo-5", "undo-4"); !errors.Is(err, ErrTaskLaneUnknown) {
		t.Fatalf("mismatched restore err=%v, want ErrTaskLaneUnknown", err)
	}
}
