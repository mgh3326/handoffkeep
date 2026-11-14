package store

import (
	"strings"
	"testing"
)

// TestMergeWaitsForEveryImplementsChange (hk 3294): a task with two open
// implements changes stays where it is when the first merges, with the
// still-open PR named in the not-applied row, and moves to merged when the
// last one merges. A change closed without merging does not hold the task.
func TestMergeWaitsForEveryImplementsChange(t *testing.T) {
	s, pool := searchTestStore(t)
	x := flowTask(t, s, pool, "implement", StateDeveloping, true)
	first, second := testPR(3294), testPR(3295)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: first, TaskID: x.ID})
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: second, TaskID: x.ID})

	res := send(t, s, mergeEvent(x.ID, first))
	if res.Applied || res.State != StateReviewing || !strings.Contains(res.Reason, second) {
		t.Fatalf("first merge: %+v, want held at reviewing naming %s", res, second)
	}
	if rows := sourceRows(t, s, x.ID, SourceMergewakeMerge); len(rows) != 1 || rows[0].Applied || !strings.Contains(rows[0].Note, second) {
		t.Fatalf("held merge rows %+v", rows)
	}
	if got := stateOf(t, s, x.ID); got.State != StateReviewing || got.Refs.PR != "" {
		t.Fatalf("held task state=%s refs.pr=%q", got.State, got.Refs.PR)
	}
	// The held merge still marks its change merged.
	if c := mustChange(t, s, "example/app", 3294); c.State != ChangeMerged {
		t.Fatalf("first change %+v", c)
	}
	if res := send(t, s, mergeEvent(x.ID, second)); !res.Applied || res.State != StateMerged || res.Task.Refs.PR != second {
		t.Fatalf("last merge: %+v", res)
	}

	y := flowTask(t, s, pool, "implement", StateDeveloping, true)
	abandoned, landed := testPR(3296), testPR(3297)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: abandoned, TaskID: y.ID})
	if _, err := pool.Exec(t.Context(), `UPDATE changes SET state='closed' WHERE repo='example/app' AND pr_number=3296`); err != nil {
		t.Fatal(err)
	}
	if res := send(t, s, mergeEvent(y.ID, landed)); !res.Applied || res.State != StateMerged {
		t.Fatalf("merge beside a closed change: %+v", res)
	}
}

// TestMergeHoldsAParentWithOpenSubTasks (hk 3294): a PR bound to a bundle
// parent does not close the parent while a sub-task is open; the PR's merge
// to the sub-tasks it delivers settles them, and the parent then closes
// through auto_close_parent when its last sub-task settles.
func TestMergeHoldsAParentWithOpenSubTasks(t *testing.T) {
	s, pool := searchTestStore(t)
	parent := flowTask(t, s, pool, "implement", StateBacklog, false)
	delivered := flowTask(t, s, pool, "implement", StateDeveloping, true)
	later := flowTask(t, s, pool, "fix", StateBacklog, false)
	setParent(t, s, delivered, parent)
	setParent(t, s, later, parent)

	pr := testPR(4059)
	res := send(t, s, TaskSourceEvent{Source: SourceMergewakeMerge, SourceRef: pr, TaskID: parent.ID, Tasks: []int64{delivered.ID}})
	if res.Applied || res.State != StateBacklog || !strings.Contains(res.Reason, "sub-tasks") {
		t.Fatalf("parent merge: %+v, want held", res)
	}
	if len(res.Tasks) != 1 || !res.Tasks[0].Applied || res.Tasks[0].State != StateMerged || res.Tasks[0].Task.ID != delivered.ID {
		t.Fatalf("named sub-task result %+v", res.Tasks)
	}
	if got := stateOf(t, s, parent.ID).State; got != StateBacklog {
		t.Fatalf("parent closed with an open sub-task: %s", got)
	}
	if got := stateOf(t, s, later.ID).State; got != StateBacklog {
		t.Fatalf("an unnamed sub-task moved: %s", got)
	}

	next := testPR(4060)
	send(t, s, TaskSourceEvent{Source: SourceMergewakeMerge, SourceRef: next, TaskID: later.ID, Tasks: []int64{parent.ID}})
	if got := stateOf(t, s, later.ID).State; got != StateMerged {
		t.Fatalf("last sub-task: %s", got)
	}
	if got := stateOf(t, s, parent.ID).State; got != StateMerged {
		t.Fatalf("parent after its last sub-task: %s, want merged", got)
	}
}

// TestEventNamesSeveralTasks (hk 3294): every task an event names gets it the
// way task_id does (own change link, own recorded row), a missing named task
// is reported and skipped, and a redelivery changes nothing on any of them.
func TestEventNamesSeveralTasks(t *testing.T) {
	s, pool := searchTestStore(t)
	a := flowTask(t, s, pool, "implement", StateReviewing, true)
	b := flowTask(t, s, pool, "fix", StateBacklog, false)
	c := flowTask(t, s, pool, "decide", StateResearching, false)
	pr := testPR(3921)
	in := TaskSourceEvent{Source: SourceMergewakeMerge, SourceRef: pr, TaskID: a.ID, Tasks: []int64{b.ID, 987654321, c.ID}}
	res := send(t, s, in)
	if !res.Applied || res.State != StateMerged || len(res.Tasks) != 3 {
		t.Fatalf("result %+v", res)
	}
	if r := res.Tasks[0]; !r.Applied || r.State != StateMerged || r.Task.Refs.PR != pr {
		t.Fatalf("named fix task %+v", r)
	}
	if r := res.Tasks[1]; r.Applied || r.State != "" || r.Reason != "task_not_found" {
		t.Fatalf("missing named task %+v", r)
	}
	if r := res.Tasks[2]; !r.Applied || r.State != StateCompleted {
		t.Fatalf("named decide task %+v", r)
	}
	links := changeLinks(t, s, mustChange(t, s, "example/app", 3921).ID)
	for _, id := range []int64{a.ID, b.ID, c.ID} {
		if links[id] != ChangeImplements {
			t.Fatalf("links %v, want every named task implements", links)
		}
		if rows := sourceRows(t, s, id, SourceMergewakeMerge); len(rows) != 1 || !rows[0].Applied {
			t.Fatalf("task %d merge rows %+v", id, rows)
		}
	}
	again := send(t, s, in)
	if !again.Redelivered || len(again.Tasks) != 3 || !again.Tasks[0].Redelivered || !again.Tasks[2].Redelivered {
		t.Fatalf("redelivery %+v", again)
	}
	if rows := sourceRows(t, s, b.ID, SourceMergewakeMerge); len(rows) != 1 {
		t.Fatalf("redelivery recorded again: %+v", rows)
	}
}

// TestEventTasksPayload: tasks must be positive, distinct, not task_id and at
// most MaxEventTasks.
func TestEventTasksPayload(t *testing.T) {
	pr := testPR(1)
	ok := TaskSourceEvent{Source: SourceMergewakeMerge, SourceRef: pr, TaskID: 1, Tasks: []int64{2, 3}}
	if _, err := normalizeTaskSourceEvent(ok); err != nil {
		t.Fatalf("valid tasks refused: %v", err)
	}
	many := make([]int64, MaxEventTasks+1)
	for i := range many {
		many[i] = int64(i + 2)
	}
	for name, tasks := range map[string][]int64{"zero": {0}, "negative": {-2}, "task_id": {1}, "duplicate": {2, 2}, "too many": many} {
		in := ok
		in.Tasks = tasks
		if _, err := normalizeTaskSourceEvent(in); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

// TestHeldMergeStillPropagates (PR #331 review): a merge held on its own task
// still reaches the other tasks whose refs.pr is that PR, each under its own
// rules.
func TestHeldMergeStillPropagates(t *testing.T) {
	s, pool := searchTestStore(t)
	pr, other := testPR(6001), testPR(6002)
	a := flowTask(t, s, pool, "implement", StateDeveloping, true)
	b := flowTask(t, s, pool, "implement", StateReviewing, true)
	setRefsPR(t, s, a.ID, pr)
	setRefsPR(t, s, b.ID, pr)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: other, TaskID: a.ID})
	if res := send(t, s, mergeEvent(a.ID, pr)); res.Applied {
		t.Fatalf("a: %+v, want held on %s", res, other)
	}
	if got := stateOf(t, s, b.ID).State; got != StateMerged {
		t.Fatalf("b on the held PR: %s, want merged", got)
	}
}

// TestRevertedPieceHoldsUntilRelanded (PR #331 review): a piece reverted
// while the task is still open holds the task, so a merge of a PR opened
// before the revert does not close it; a reland PR opened after the revert
// does.
func TestRevertedPieceHoldsUntilRelanded(t *testing.T) {
	s, pool := searchTestStore(t)
	x := flowTask(t, s, pool, "implement", StateDeveloping, true)
	a, b, revert, reland := testPR(6101), testPR(6102), testPR(6103), testPR(6104)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: a, TaskID: x.ID})
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: b, TaskID: x.ID})
	send(t, s, mergeEvent(x.ID, a))
	send(t, s, revertEvent(x.ID, a, revert))
	res := send(t, s, mergeEvent(x.ID, b))
	if res.Applied || !strings.Contains(res.Reason, "reverted") || !strings.Contains(res.Reason, a) {
		t.Fatalf("merge of b after a's revert: %+v, want held on %s", res, a)
	}
	if res := send(t, s, mergeEvent(x.ID, reland)); !res.Applied || res.State != StateMerged {
		t.Fatalf("reland: %+v", res)
	}
}

// TestNamedTasksKeepTheirOwnEventKey (PR #331 review): a task the event names
// gets its own turn. Propagation does not apply the merge to it first (its
// result says applied, not redelivered), and a sub-task's revert does not
// write the informational row on a parent the event also names, so the
// parent reverts too.
func TestNamedTasksKeepTheirOwnEventKey(t *testing.T) {
	s, pool := searchTestStore(t)
	pr := testPR(6301)
	a := flowTask(t, s, pool, "implement", StateReviewing, true)
	b := flowTask(t, s, pool, "implement", StateReviewing, true)
	setRefsPR(t, s, b.ID, pr)
	res := send(t, s, TaskSourceEvent{Source: SourceMergewakeMerge, SourceRef: pr, TaskID: a.ID, Tasks: []int64{b.ID}})
	if len(res.Tasks) != 1 || !res.Tasks[0].Applied || res.Tasks[0].Redelivered || res.Tasks[0].State != StateMerged {
		t.Fatalf("named task on the PR: %+v", res.Tasks)
	}

	parent := flowTask(t, s, pool, "implement", StateBacklog, false)
	child := flowTask(t, s, pool, "implement", StateReviewing, true)
	setParent(t, s, child, parent)
	merged := testPR(6302)
	putDeploy(t, s, "deploy/svc/20261113T010000Z", "svc", "success", []string{merged})
	send(t, s, TaskSourceEvent{Source: SourceMergewakeMerge, SourceRef: merged, TaskID: child.ID, Tasks: []int64{parent.ID}})
	if got := stateOf(t, s, parent.ID); got.State != StateDeployed || got.Refs.PR != merged {
		t.Fatalf("named parent after the merge: state=%s refs.pr=%q, want deployed on %s", got.State, got.Refs.PR, merged)
	}
	rev := send(t, s, TaskSourceEvent{Source: SourceMergewakeRevert, SourceRef: testPR(6303), RevertedPR: merged, TaskID: child.ID, Tasks: []int64{parent.ID}})
	if !rev.Applied || len(rev.Tasks) != 1 || !rev.Tasks[0].Applied || rev.Tasks[0].State != StateDeveloping {
		t.Fatalf("revert naming the parent: %+v", rev)
	}
}

// TestParentFirstNamedOrderStillCloses (mergewake review of the named-parent
// deferral): when task_id is the parent and the child comes later in tasks,
// the parent's merge is held while the child is open. The child's turn does
// not defer — the parent's turn already ran — so the child auto-closes the
// parent itself.
func TestParentFirstNamedOrderStillCloses(t *testing.T) {
	s, pool := searchTestStore(t)
	parent := flowTask(t, s, pool, "implement", StateBacklog, false)
	child := flowTask(t, s, pool, "implement", StateReviewing, true)
	setParent(t, s, child, parent)
	merged := testPR(6305)
	putDeploy(t, s, "deploy/svc/20261114T010000Z", "svc", "success", []string{merged})
	res := send(t, s, TaskSourceEvent{Source: SourceMergewakeMerge, SourceRef: merged, TaskID: parent.ID, Tasks: []int64{child.ID}})
	if res.Applied || len(res.Tasks) != 1 || !res.Tasks[0].Applied || res.Tasks[0].State != StateDeployed {
		t.Fatalf("parent-first merge: %+v / %+v", res, res.Tasks)
	}
	if got := stateOf(t, s, parent.ID); got.State != StateDeployed {
		t.Fatalf("parent after the child's merge: state=%s, want deployed", got.State)
	}
	if got := stateOf(t, s, child.ID); got.State != StateDeployed {
		t.Fatalf("child after the merge: state=%s, want deployed", got.State)
	}
}

// TestSettledNamedParentIsClosedByTheChildsTurn (mergewake review of the
// deploy-turn deferral): a parent already merged with its PR cannot move in
// its own event turn, so a settling child's turns must not defer the rollup
// close to it — the child deploys the parent itself.
func TestSettledNamedParentIsClosedByTheChildsTurn(t *testing.T) {
	s, pool := searchTestStore(t)
	parent := flowTask(t, s, pool, "implement", StateMerged, false)
	setRefsPR(t, s, parent.ID, testPR(6306))
	child := flowTask(t, s, pool, "implement", StateReviewing, true)
	setParent(t, s, child, parent)
	merged := testPR(6307)
	putDeploy(t, s, "deploy/svc/20261114T020000Z", "svc", "success", []string{merged})
	send(t, s, TaskSourceEvent{Source: SourceMergewakeMerge, SourceRef: merged, TaskID: child.ID, Tasks: []int64{parent.ID}})
	if got := stateOf(t, s, parent.ID); got.State != StateDeployed {
		t.Fatalf("merged parent after the child's deploy: state=%s, want deployed", got.State)
	}
}

// TestRelandIsJudgedByOpenTime (PR #331 review round 2): a PR opened before
// the revert merged does not reland the reverted piece, even when its row was
// written after the revert PR's row.
func TestRelandIsJudgedByOpenTime(t *testing.T) {
	s, pool := searchTestStore(t)
	x := flowTask(t, s, pool, "implement", StateDeveloping, true)
	a, holder, revert, b := testPR(6401), testPR(6402), testPR(6403), testPR(6404)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: a, TaskID: x.ID})
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: holder, TaskID: x.ID})
	send(t, s, mergeEvent(x.ID, a))
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: revert, TaskID: x.ID})
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: b, TaskID: x.ID})
	send(t, s, revertEvent(x.ID, a, revert))
	if _, err := pool.Exec(t.Context(), `UPDATE changes SET state='closed' WHERE repo='example/app' AND pr_number=6402`); err != nil {
		t.Fatal(err)
	}
	if res := send(t, s, mergeEvent(x.ID, b)); res.Applied || !strings.Contains(res.Reason, "reverted") {
		t.Fatalf("merge of a PR opened before the revert: %+v, want held", res)
	}
}

// TestPropagatedSiblingLinksTheChange (PR #331 review round 2): a sibling the
// merge propagates to is linked to the change as implements, so a later
// revert of the PR holds the sibling too.
func TestPropagatedSiblingLinksTheChange(t *testing.T) {
	s, pool := searchTestStore(t)
	pr := testPR(6701)
	a := flowTask(t, s, pool, "implement", StateReviewing, true)
	b := flowTask(t, s, pool, "implement", StateReviewing, true)
	setRefsPR(t, s, a.ID, pr)
	setRefsPR(t, s, b.ID, pr)
	send(t, s, mergeEvent(a.ID, pr))
	if links := changeLinks(t, s, mustChange(t, s, "example/app", 6701).ID); links[b.ID] != ChangeImplements {
		t.Fatalf("links %v, want the sibling linked as implements", links)
	}
}

// TestAutoCloseKeepsTheDecisionGates (PR #331 review round 4): auto-close
// leaves a parent open while it awaits a decision or its decision request is
// open (closeGateTx).
func TestAutoCloseKeepsTheDecisionGates(t *testing.T) {
	s, pool := searchTestStore(t)
	for i, gate := range []string{
		`UPDATE tasks SET awaiting_decision=true WHERE id=$1`,
		`UPDATE tasks SET refs=refs||jsonb_build_object('decision_request',jsonb_build_object('id','dr-'||$1::text||'-1','status','open')) WHERE id=$1`,
	} {
		parent := flowTask(t, s, pool, "implement", StateDeveloping, true)
		child := flowTask(t, s, pool, "implement", StateReviewing, true)
		setParent(t, s, child, parent)
		if _, err := pool.Exec(t.Context(), gate, parent.ID); err != nil {
			t.Fatal(err)
		}
		send(t, s, mergeEvent(child.ID, testPR(7001+i)))
		if got := stateOf(t, s, parent.ID).State; got != StateDeveloping {
			t.Fatalf("gate %d: parent auto-closed to %s", i, got)
		}
	}
}

// TestAutoCloseHoldsAParentOnItsOwnPieces (PR #351 review H1): the last
// sub-task settling does not close a parent while one of the parent's own
// implements changes is open, or is reverted and not relanded; the not-applied
// auto_close row names the change.
func TestAutoCloseHoldsAParentOnItsOwnPieces(t *testing.T) {
	s, pool := searchTestStore(t)

	// An open implements PR of the parent's own.
	parent := flowTask(t, s, pool, "implement", StateDeveloping, true)
	child := flowTask(t, s, pool, "implement", StateReviewing, true)
	setParent(t, s, child, parent)
	open := testPR(7201)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: open, TaskID: parent.ID})
	if res := send(t, s, mergeEvent(child.ID, testPR(7202))); !res.Applied || res.State != StateMerged {
		t.Fatalf("child merge %+v", res)
	}
	got := stateOf(t, s, parent.ID)
	if got.State != StateReviewing {
		t.Fatalf("parent with an open implements PR moved to %s, want reviewing", got.State)
	}
	if skips := eventsOfKind(got, TaskEventAutoClose); len(skips) != 1 || skips[0].Applied || !strings.Contains(skips[0].Note, open) {
		t.Fatalf("auto_close rows %+v, want one not-applied naming %s", skips, open)
	}

	// A reverted implements PR of the parent's own, not relanded.
	parent2 := flowTask(t, s, pool, "implement", StateDeveloping, true)
	child2 := flowTask(t, s, pool, "implement", StateReviewing, true)
	setParent(t, s, child2, parent2)
	piece := testPR(7203)
	if res := send(t, s, mergeEvent(parent2.ID, piece)); res.Applied {
		t.Fatalf("parent merge with an open sub-task applied: %+v", res)
	}
	send(t, s, revertEvent(parent2.ID, piece, testPR(7204)))
	send(t, s, mergeEvent(child2.ID, testPR(7205)))
	got = stateOf(t, s, parent2.ID)
	if got.State != StateDeveloping {
		t.Fatalf("parent with a reverted piece auto-closed to %s", got.State)
	}
	if skips := eventsOfKind(got, TaskEventAutoClose); len(skips) != 1 || !strings.Contains(skips[0].Note, "reverted") {
		t.Fatalf("auto_close rows %+v, want one naming the revert", skips)
	}
}

// TestRevertReachesEveryTaskClosedOnThePR (PR #351 review H3): a revert event
// names one task, and it also reopens every other task that implements the
// reverted PR and is closed on it: one closed by naming the PR (hk 4322) and
// a sibling the merge propagated to.
func TestRevertReachesEveryTaskClosedOnThePR(t *testing.T) {
	s, pool := searchTestStore(t)
	pr := testPR(7301)
	carrier := flowTask(t, s, pool, "implement", StateReviewing, true)
	sibling := flowTask(t, s, pool, "implement", StateReviewing, true)
	setRefsPR(t, s, carrier.ID, pr)
	setRefsPR(t, s, sibling.ID, pr)
	send(t, s, mergeEvent(carrier.ID, pr))
	delivered := flowTask(t, s, pool, "fix", StateBacklog, false)
	if got, err := s.TransitionTask(t.Context(), delivered.ID, StateMerged, "tester", "", &TaskRefs{PR: pr}, ""); err != nil || got.State != StateMerged {
		t.Fatalf("delivered close: %+v err=%v", got, err)
	}
	// Closed on the PR without a link: by hand from reviewing with --pr, and a
	// sibling a merge closed before siblings were linked.
	byHand := flowTask(t, s, pool, "implement", StateReviewing, true)
	if got, err := s.TransitionTask(t.Context(), byHand.ID, StateMerged, "tester", "", &TaskRefs{PR: pr}, ""); err != nil || got.State != StateMerged {
		t.Fatalf("reviewing -> merged --pr: %+v err=%v", got, err)
	}
	legacy := flowTask(t, s, pool, "implement", StateMerged, false)
	setRefsPR(t, s, legacy.ID, pr)
	links := changeLinks(t, s, mustChange(t, s, "example/app", 7301).ID)
	if links[byHand.ID] != "" || links[legacy.ID] != "" {
		t.Fatalf("links %v, want the by-hand and legacy tasks unlinked", links)
	}
	revert := testPR(7302)
	if res := send(t, s, revertEvent(carrier.ID, pr, revert)); !res.Applied || res.State != StateDeveloping {
		t.Fatalf("revert %+v", res)
	}
	for _, id := range []int64{sibling.ID, delivered.ID, byHand.ID, legacy.ID} {
		got := stateOf(t, s, id)
		if got.State != StateDeveloping || got.Refs.RevertPR != revert {
			t.Fatalf("task %d after the revert: state=%s revert_pr=%q, want developing", id, got.State, got.Refs.RevertPR)
		}
		if rows := sourceRows(t, s, id, SourceMergewakeRevert); len(rows) != 1 || !rows[0].Applied {
			t.Fatalf("task %d revert rows %+v", id, rows)
		}
	}
	// A redelivery changes nothing.
	if res := send(t, s, revertEvent(carrier.ID, pr, revert)); !res.Redelivered {
		t.Fatalf("redelivery %+v", res)
	}
	if rows := sourceRows(t, s, delivered.ID, SourceMergewakeRevert); len(rows) != 1 {
		t.Fatalf("redelivered revert rows %+v", rows)
	}
}
