package store

import (
	"errors"
	"strings"
	"testing"
)

// TestTransitionToMergedByDeliveredPR (hk 4322): a task a merged PR
// delivered closes as merged in one step from every open state before
// reviewing when the transition names the PR. The PR is the closure
// evidence, and the task is linked to the change as implements.
func TestTransitionToMergedByDeliveredPR(t *testing.T) {
	s, pool := searchTestStore(t)
	pr := testPR(4322)
	// The merge event of another task writes the PR's change record as merged.
	carrier := flowTask(t, s, pool, "implement", StateReviewing, true)
	if res := send(t, s, mergeEvent(carrier.ID, pr)); !res.Applied {
		t.Fatalf("carrier merge %+v", res)
	}
	change := mustChange(t, s, "example/app", 4322)
	for _, from := range []string{StateTriage, StateBacklog, StateUnstarted, StateResearching, StateDeveloping} {
		x := flowTask(t, s, pool, "fix", from, from == StateResearching || from == StateDeveloping)
		got, err := s.TransitionTask(t.Context(), x.ID, StateMerged, "tester", "", &TaskRefs{PR: pr}, "")
		if err != nil || got.State != StateMerged || got.Refs.PR != pr || got.CompletedAt == nil {
			t.Fatalf("%s -> merged --pr: %+v err=%v", from, got, err)
		}
		if c := got.Refs.Closure; c == nil || c.Evidence != pr {
			t.Fatalf("%s -> merged closure %+v, want the PR as evidence", from, c)
		}
		if links := changeLinks(t, s, change.ID); links[x.ID] != ChangeImplements {
			t.Fatalf("%s -> merged links %v, want implements", from, links)
		}
	}
}

// TestTransitionToMergedRefusesAnUnverifiedPR (hk 4322): the one-step close
// needs the change record to show the PR merged; an open PR or a PR hk has no
// record of is refused, and the task stays where it is.
func TestTransitionToMergedRefusesAnUnverifiedPR(t *testing.T) {
	s, pool := searchTestStore(t)
	open := testPR(4323)
	opener := flowTask(t, s, pool, "implement", StateDeveloping, true)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: open, TaskID: opener.ID})
	for pr, want := range map[string]string{open: "not merged", testPR(4324): "no change record"} {
		x := flowTask(t, s, pool, "fix", StateBacklog, false)
		_, err := s.TransitionTask(t.Context(), x.ID, StateMerged, "tester", "", &TaskRefs{PR: pr}, "")
		if !errors.Is(err, ErrTaskConflict) || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err=%v, want task_conflict naming %q", pr, err, want)
		}
		if got := stateOf(t, s, x.ID); got.State != StateBacklog || got.Refs.PR != "" {
			t.Fatalf("%s: task moved to %s refs.pr=%q", pr, got.State, got.Refs.PR)
		}
	}
	// A task awaiting a decision still leaves only the decision's way.
	pr := testPR(4325)
	carrier := flowTask(t, s, pool, "implement", StateReviewing, true)
	send(t, s, mergeEvent(carrier.ID, pr))
	x := flowTask(t, s, pool, "fix", StateBacklog, false)
	if _, err := pool.Exec(t.Context(), `UPDATE tasks SET awaiting_decision=true WHERE id=$1`, x.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTask(t.Context(), x.ID, StateMerged, "tester", "", &TaskRefs{PR: pr}, ""); !errors.Is(err, ErrTaskConflict) || !strings.Contains(err.Error(), "awaiting a decision") {
		t.Fatalf("awaiting task: err=%v", err)
	}
}

// TestTransitionConflictNamesAllowedStates (hk 4322): a refused manual move
// says which states the task can move to from where it is.
func TestTransitionConflictNamesAllowedStates(t *testing.T) {
	s, pool := searchTestStore(t)
	x := flowTask(t, s, pool, "fix", StateBacklog, false)
	_, err := s.TransitionTask(t.Context(), x.ID, StateMerged, "tester", "skip", nil, "")
	if !errors.Is(err, ErrTaskConflict) || !strings.Contains(err.Error(), "from backlog a task can move to unstarted, researching, developing, completed, dropped, duplicate, icebox") || !strings.Contains(err.Error(), "--pr") {
		t.Fatalf("backlog -> merged: %v", err)
	}
	y := flowTask(t, s, pool, "fix", StateMerged, false)
	if _, err := s.TransitionTask(t.Context(), y.ID, StateDeveloping, "tester", "again", nil, ""); !errors.Is(err, ErrTaskConflict) || !strings.Contains(err.Error(), "from merged a task can move to unstarted, completed") || !strings.Contains(err.Error(), "tasks reopen") {
		t.Fatalf("merged -> developing: %v", err)
	}
}

// TestTransitionConflictText pins the refusal text without a database.
func TestTransitionConflictText(t *testing.T) {
	for _, c := range []struct{ from, to, want, not string }{
		{StateTriage, StateDeveloping, "triage -> developing is not a transition; from triage a task can move to backlog, unstarted, dropped, duplicate", "--pr"},
		{StateDeveloping, StateMerged, "from developing a task can move to unstarted, researching, reviewing, completed, dropped, duplicate, icebox", ""},
		{StateDeveloping, StateMerged, "or to merged with --pr (refs.pr) naming the merged PR that delivered it", ""},
		{StateReviewing, StateDeployed, "from reviewing a task can move to", "--pr"},
		{StateDropped, StateBacklog, "from dropped a task can move to unstarted (unstarted through tasks reopen)", ""},
	} {
		err := transitionConflict(c.from, c.to)
		if !errors.Is(err, ErrTaskConflict) {
			t.Fatalf("%s -> %s: %v is not task_conflict", c.from, c.to, err)
		}
		if !strings.Contains(err.Error(), c.want) || (c.not != "" && strings.Contains(err.Error(), c.not)) {
			t.Fatalf("%s -> %s: %q, want %q without %q", c.from, c.to, err, c.want, c.not)
		}
	}
}
