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

// TestRefusedReopenNamesAllowedStates (hk 2863): a reopen of an open task is
// refused as task_conflict and names the states the task can move to instead.
func TestRefusedReopenNamesAllowedStates(t *testing.T) {
	s, pool := searchTestStore(t)
	x := flowTask(t, s, pool, "fix", StateBacklog, false)
	_, err := s.transitionTask(t.Context(), x.ID, TransitionInput{To: StateUnstarted, By: "tester", Note: "reopen"}, true)
	if !errors.Is(err, ErrTaskConflict) || !strings.Contains(err.Error(), "from backlog a task can move to unstarted, researching, developing, completed, dropped, duplicate, icebox") {
		t.Fatalf("reopen of backlog: %v", err)
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

// TestDeliveredCloseKeepsTheMergeRules (PR #351 review H2): the one-step close
// refuses a PR that was reverted, and it holds as a merge event does while the
// task has another implements change open; a refusal leaves the task and its
// change links as they were.
func TestDeliveredCloseKeepsTheMergeRules(t *testing.T) {
	s, pool := searchTestStore(t)
	reverted := testPR(4331)
	carrier := flowTask(t, s, pool, "implement", StateReviewing, true)
	send(t, s, mergeEvent(carrier.ID, reverted))
	send(t, s, revertEvent(carrier.ID, reverted, testPR(4332)))
	x := flowTask(t, s, pool, "fix", StateDeveloping, true)
	if _, err := s.TransitionTask(t.Context(), x.ID, StateMerged, "tester", "", &TaskRefs{PR: reverted}, ""); !errors.Is(err, ErrTaskConflict) || !strings.Contains(err.Error(), "reverted") {
		t.Fatalf("close by a reverted PR: err=%v", err)
	}
	if got := stateOf(t, s, x.ID).State; got != StateDeveloping {
		t.Fatalf("task closed by a reverted PR: %s", got)
	}

	merged, open := testPR(4333), testPR(4334)
	other := flowTask(t, s, pool, "implement", StateReviewing, true)
	send(t, s, mergeEvent(other.ID, merged))
	y := flowTask(t, s, pool, "fix", StateDeveloping, true)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: open, TaskID: y.ID})
	// The PR event moved y to reviewing; put it back where the one-step close
	// applies.
	if _, err := pool.Exec(t.Context(), `UPDATE tasks SET state='developing' WHERE id=$1`, y.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTask(t.Context(), y.ID, StateMerged, "tester", "", &TaskRefs{PR: merged}, ""); !errors.Is(err, ErrTaskConflict) || !strings.Contains(err.Error(), open) {
		t.Fatalf("close with another implements PR open: err=%v, want task_conflict naming %s", err, open)
	}
	if got := stateOf(t, s, y.ID).State; got != StateDeveloping {
		t.Fatalf("held task moved to %s", got)
	}
	if links := changeLinks(t, s, mustChange(t, s, "example/app", 4333).ID); links[y.ID] != "" {
		t.Fatalf("a refused close left the link %v", links)
	}

	// A task already linked to the PR as references (an earlier revert event
	// named it with this PR as the revert PR) implements it once it closes on
	// it, so a later revert of the PR reaches it.
	z := flowTask(t, s, pool, "fix", StateDeveloping, true)
	change := mustChange(t, s, "example/app", 4333)
	if _, err := pool.Exec(t.Context(), `INSERT INTO change_tasks(change_id,task_id,relation,created_at) VALUES($1,$2,'references',now())`, change.ID, z.ID); err != nil {
		t.Fatal(err)
	}
	nonCanonical := "https://github.com/example/app/pull/+4333"
	if _, err := s.TransitionTask(t.Context(), z.ID, StateMerged, "tester", "", &TaskRefs{PR: nonCanonical}, ""); err != nil {
		t.Fatal(err)
	}
	if links := changeLinks(t, s, change.ID); links[z.ID] != ChangeImplements {
		t.Fatalf("links %v, want the references link reclassified as implements", links)
	}
	if got := stateOf(t, s, z.ID); got.Refs.PR != merged {
		t.Fatalf("stored PR %q, want canonical %q", got.Refs.PR, merged)
	}
}

// TestDeliveredCloseOfASetParent (PR #351 review, --set): a ticket set's
// parent with children still in triage is held like a merge event's parent,
// with or without --set, and the children are not declined.
func TestDeliveredCloseOfASetParent(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	pr := testPR(4335)
	carrier := flowTask(t, s, pool, "implement", StateReviewing, true)
	send(t, s, mergeEvent(carrier.ID, pr))
	set, _, err := s.EmitTicketSet(ctx, ticketSetFixture("delivered"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET kind='implement' WHERE id=$1`, set.ParentID); err != nil {
		t.Fatal(err)
	}
	for _, withSet := range []bool{false, true} {
		_, err := s.TransitionTaskWith(ctx, set.ParentID, TransitionInput{To: StateMerged, By: "desk", Refs: &TaskRefs{PR: pr}, Set: withSet})
		if !errors.Is(err, ErrTaskConflict) || !strings.Contains(err.Error(), "sub-tasks") {
			t.Fatalf("set=%t: err=%v, want held on its sub-tasks", withSet, err)
		}
		for _, ch := range set.Children {
			if got := stateOf(t, s, ch.ID).State; got != StateTriage {
				t.Fatalf("set=%t: child %d moved to %s", withSet, ch.ID, got)
			}
		}
	}
}
