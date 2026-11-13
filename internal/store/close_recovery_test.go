package store

import (
	"strings"
	"testing"
	"time"
)

// The pure tests below run without a database. The tests after them reach
// Postgres through searchTestStore and run in CI.

// TestSubTaskRollupCloseTarget (hk 4172): a parent's close target from its
// sub-tasks. A completed sub-task never deploys, so it does not hold the
// parent back from deployed; a merged one does.
func TestSubTaskRollupCloseTarget(t *testing.T) {
	cases := []struct {
		name string
		sub  []string
		want string
	}{
		{"only dropped or duplicate: no close", []string{StateDropped, StateDuplicate}, ""},
		{"an unsettled sub-task: no close", []string{StateDeveloping, StateMerged}, ""},
		{"only completed: completed", []string{StateCompleted, StateCompleted}, StateCompleted},
		{"completed and deployed: deployed (completed does not wait)", []string{StateCompleted, StateDeployed}, StateDeployed},
		{"deployed only: deployed", []string{StateDeployed, StateDeployed, StateDropped}, StateDeployed},
		{"merged waits for its deploy: merged", []string{StateDeployed, StateMerged}, StateMerged},
		{"merged alone: merged", []string{StateMerged}, StateMerged},
		{"merged with completed: merged", []string{StateCompleted, StateMerged}, StateMerged},
	}
	for _, tc := range cases {
		if got := rollupStates(tc.sub).closeTarget(); got != tc.want {
			t.Errorf("%s: closeTarget(%v)=%q, want %q", tc.name, tc.sub, got, tc.want)
		}
	}
}

// TestTaskHoldsReasons (hk 4172): the hold reasons keep their wording, and a
// merge's own changes are named before its sub-tasks.
func TestTaskHoldsReasons(t *testing.T) {
	open := taskHolds{OpenPRs: []string{testPR(1)}, OpenSubTasks: []int64{9}}
	if got := open.mergeReason(); !strings.Contains(got, testPR(1)) || strings.Contains(got, "#9") {
		t.Fatalf("open PR should be the reason, got %q", got)
	}
	reverted := taskHolds{RevertedPRs: []string{testPR(2)}}
	if got := reverted.pieceReason(); !strings.Contains(got, "reverted and not relanded") || !strings.Contains(got, testPR(2)) {
		t.Fatalf("reverted reason %q", got)
	}
	subs := taskHolds{OpenSubTasks: []int64{3, 4}}
	if got := subs.mergeReason(); got != "sub-tasks #3, #4 are not settled; the parent closes when its last sub-task does (auto_close_parent)" {
		t.Fatalf("sub-task reason %q", got)
	}
	if subs.pieceReason() != "" {
		t.Fatalf("sub-tasks are not a piece hold: %q", subs.pieceReason())
	}
	if !(taskHolds{}).none() || subs.none() {
		t.Fatal("none() disagrees with the holds")
	}
}

// TestDriftMechanicalNeedsEveryCondition (hk 4172): a close is mechanical only
// when nothing holds it, the state is one the event applies from, and the
// close gate passes.
func TestDriftMechanicalNeedsEveryCondition(t *testing.T) {
	none := taskHolds{}
	cases := []struct {
		name  string
		h     taskHolds
		gate  string
		state string
		want  bool
	}{
		{"nothing holds it, state applies, gate passes", none, "", StateReviewing, true},
		{"an open PR holds it", taskHolds{OpenPRs: []string{testPR(3)}}, "", StateReviewing, false},
		{"a revert holds it", taskHolds{RevertedPRs: []string{testPR(4)}}, "", StateReviewing, false},
		{"an unsettled sub-task holds it", taskHolds{OpenSubTasks: []int64{5}}, "", StateReviewing, false},
		{"the gate refuses (decision open)", none, "parent has an open decision request", StateReviewing, false},
		{"the state is not one a merge applies from", none, "", StateIcebox, false},
	}
	for _, tc := range cases {
		if got := driftMechanical(tc.h, tc.gate, tc.state, mergeAppliesFrom); got != tc.want {
			t.Errorf("%s: mechanical=%t, want %t", tc.name, got, tc.want)
		}
	}
}

// TestCloseRulesApplyFrom (hk 4172): the merge event and the automatic close
// name the same states the drift report reads, so a row it calls mechanical is
// one an event would move.
func TestCloseRulesApplyFrom(t *testing.T) {
	for _, state := range []string{StateBacklog, StateUnstarted, StateResearching, StateDeveloping, StateReviewing} {
		if !mergeAppliesFrom(state) || !autoCloseAppliesFrom(state) {
			t.Errorf("%s: merge=%t autoClose=%t, want both", state, mergeAppliesFrom(state), autoCloseAppliesFrom(state))
		}
	}
	for _, state := range []string{StateTriage, StateIcebox, StateMerged, StateDeployed, StateCompleted, StateDropped} {
		if mergeAppliesFrom(state) || autoCloseAppliesFrom(state) {
			t.Errorf("%s: merge=%t autoClose=%t, want neither", state, mergeAppliesFrom(state), autoCloseAppliesFrom(state))
		}
	}
}

// driftEntry returns the row of task id in class c.
func driftEntry(c TaskDriftClass, id int64) (TaskDriftTask, bool) {
	for _, x := range c.Tasks {
		if x.ID == id {
			return x, true
		}
	}
	return TaskDriftTask{}, false
}

// TestDriftMergedPRFindsTheTaskThroughItsChange (hk 4172): a task whose merged
// PR is held (its merge did not reach it) is found through change_tasks, names
// the open PR that holds it, and is mechanical once that PR is closed.
func TestDriftMergedPRFindsTheTaskThroughItsChange(t *testing.T) {
	s, pool := searchTestStore(t)
	x := flowTask(t, s, pool, "implement", StateDeveloping, true)
	first, second := testPR(9101), testPR(9102)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: first, TaskID: x.ID})
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: second, TaskID: x.ID})
	if res := send(t, s, mergeEvent(x.ID, first)); res.Applied {
		t.Fatalf("first merge applied beside an open PR: %+v", res)
	}
	if got := stateOf(t, s, x.ID); got.Refs.PR != "" {
		t.Fatalf("held merge set refs.pr=%q; the test needs the task to name no PR", got.Refs.PR)
	}

	row, ok := driftEntry(driftAt(t, s, time.Now().UTC(), DriftDefaultMergedDays).MergedPRNotMerged, x.ID)
	if !ok {
		t.Fatal("a task with a merged implements change was not listed in merged_pr_not_merged")
	}
	if len(row.OpenPRs) != 1 || row.OpenPRs[0] != second || row.Mechanical {
		t.Fatalf("held row %+v, want open_prs=[%s] and not mechanical", row, second)
	}

	if _, err := pool.Exec(t.Context(), `UPDATE changes SET state='closed' WHERE repo='example/app' AND pr_number=9102`); err != nil {
		t.Fatal(err)
	}
	row, ok = driftEntry(driftAt(t, s, time.Now().UTC(), DriftDefaultMergedDays).MergedPRNotMerged, x.ID)
	if !ok || len(row.OpenPRs) != 0 || !row.Mechanical {
		t.Fatalf("after the open PR closed: row=%+v listed=%t, want mechanical with no open PRs", row, ok)
	}
}

// TestDriftMergedPRSkipsAnUnrelandedRevert (hk 4172): a revert holds the task
// until it is relanded, so the merged-PR class does not name it.
func TestDriftMergedPRSkipsAnUnrelandedRevert(t *testing.T) {
	s, pool := searchTestStore(t)
	x := flowTask(t, s, pool, "implement", StateDeveloping, true)
	piece := testPR(9201)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: piece, TaskID: x.ID})
	if res := send(t, s, mergeEvent(x.ID, piece)); !res.Applied {
		t.Fatalf("merge: %+v", res)
	}
	send(t, s, revertEvent(x.ID, piece, testPR(9202)))
	if _, ok := driftEntry(driftAt(t, s, time.Now().UTC(), DriftDefaultMergedDays).MergedPRNotMerged, x.ID); ok {
		t.Fatal("a task with an unrelanded revert was listed in merged_pr_not_merged")
	}
}

// TestDriftAutoCloseHeldNamesTheStrandedParent (hk 4172): a parent whose
// sub-tasks have all settled, and whose automatic close a held PR of its own
// blocks, is listed in auto_close_held with that PR. Once the PR closes unmerged
// nothing holds it, so the hygiene job may apply the close. A decide parent
// whose only sub-tasks completed (a completed target) is covered the same way.
func TestDriftAutoCloseHeldNamesTheStrandedParent(t *testing.T) {
	s, pool := searchTestStore(t)

	parent := flowTask(t, s, pool, "implement", StateDeveloping, true)
	child := flowTask(t, s, pool, "implement", StateReviewing, true)
	setParent(t, s, child, parent)
	open := testPR(9301)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: open, TaskID: parent.ID})
	if res := send(t, s, mergeEvent(child.ID, testPR(9302))); !res.Applied {
		t.Fatalf("child merge %+v", res)
	}
	row, ok := driftEntry(driftAt(t, s, time.Now().UTC(), DriftDefaultMergedDays).AutoCloseHeld, parent.ID)
	if !ok || len(row.OpenPRs) != 1 || row.OpenPRs[0] != open || row.Mechanical {
		t.Fatalf("held parent row %+v listed=%t, want open_prs=[%s] and not mechanical", row, ok, open)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE changes SET state='closed' WHERE repo='example/app' AND pr_number=9301`); err != nil {
		t.Fatal(err)
	}
	row, ok = driftEntry(driftAt(t, s, time.Now().UTC(), DriftDefaultMergedDays).AutoCloseHeld, parent.ID)
	if !ok || !row.Mechanical {
		t.Fatalf("after the PR closed unmerged: row=%+v listed=%t, want mechanical", row, ok)
	}

	// A decide parent whose only sub-task completed: its target is completed.
	decide := flowTask(t, s, pool, "decide", StateResearching, true)
	done := flowTask(t, s, pool, "decide", StateReviewing, true)
	setParent(t, s, done, decide)
	if _, err := pool.Exec(t.Context(), `UPDATE tasks SET state='completed' WHERE id=$1`, done.ID); err != nil {
		t.Fatal(err)
	}
	decidePR := testPR(9303)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: decidePR, TaskID: decide.ID})
	if _, err := pool.Exec(t.Context(), `UPDATE changes SET state='closed' WHERE repo='example/app' AND pr_number=9303`); err != nil {
		t.Fatal(err)
	}
	rep := driftAt(t, s, time.Now().UTC(), DriftDefaultMergedDays)
	if row, ok := driftEntry(rep.AutoCloseHeld, decide.ID); !ok || !row.Mechanical {
		t.Fatalf("completed-target decide parent row %+v listed=%t, want mechanical", row, ok)
	}
}

// TestMergedParentDeploysWhenASubTaskDeploys (hk 4172): a parent that closed to
// merged on a sub-task's merge is moved on to deployed when that sub-task
// deploys. A completed sub-task does not hold it back.
func TestMergedParentDeploysWhenASubTaskDeploys(t *testing.T) {
	s, pool := searchTestStore(t)
	parent := flowTask(t, s, pool, "implement", StateDeveloping, true)
	delivered := flowTask(t, s, pool, "implement", StateReviewing, true)
	finished := flowTask(t, s, pool, "implement", StateReviewing, true)
	setParent(t, s, delivered, parent)
	setParent(t, s, finished, parent)
	if _, err := pool.Exec(t.Context(), `UPDATE tasks SET state='completed' WHERE id=$1`, finished.ID); err != nil {
		t.Fatal(err)
	}
	pr := testPR(9401)
	if res := send(t, s, mergeEvent(delivered.ID, pr)); !res.Applied {
		t.Fatalf("sub-task merge %+v", res)
	}
	if got := stateOf(t, s, parent.ID).State; got != StateMerged {
		t.Fatalf("parent=%s after its sub-task merged, want merged", got)
	}

	putDeploy(t, s, "deploy/svc/20261113T010000Z", "svc", "success", []string{pr})
	if got := stateOf(t, s, delivered.ID).State; got != StateDeployed {
		t.Fatalf("sub-task=%s after its deploy record, want deployed", got)
	}
	if got := stateOf(t, s, parent.ID).State; got != StateDeployed {
		t.Fatalf("parent=%s after its sub-task deployed, want deployed", got)
	}
}

// TestAdoptedPRDoesNotDeployPastASubTask (hk 4172): a merged parent with no PR
// of its own adopts the PR a merge names, but the adoption does not deploy it
// past a sub-task that is merged and not yet deployed.
func TestAdoptedPRDoesNotDeployPastASubTask(t *testing.T) {
	s, pool := searchTestStore(t)
	parent := flowTask(t, s, pool, "implement", StateDeveloping, true)
	delivered := flowTask(t, s, pool, "implement", StateReviewing, true)
	finished := flowTask(t, s, pool, "implement", StateReviewing, true)
	setParent(t, s, delivered, parent)
	setParent(t, s, finished, parent)
	if _, err := pool.Exec(t.Context(), `UPDATE tasks SET state='completed' WHERE id=$1`, finished.ID); err != nil {
		t.Fatal(err)
	}
	if res := send(t, s, mergeEvent(delivered.ID, testPR(9501))); !res.Applied {
		t.Fatalf("sub-task merge %+v", res)
	}
	if got := stateOf(t, s, parent.ID); got.State != StateMerged || got.Refs.PR != "" {
		t.Fatalf("parent %s refs.pr=%q, want merged with no PR", got.State, got.Refs.PR)
	}

	own := testPR(9502)
	putDeploy(t, s, "deploy/svc/20261113T020000Z", "svc", "success", []string{own})
	if res := send(t, s, mergeEvent(parent.ID, own)); res.State == StateDeployed {
		t.Fatalf("adoption deployed past an undeployed sub-task: %+v", res)
	}
	got := stateOf(t, s, parent.ID)
	if got.State != StateMerged || got.Refs.PR != own {
		t.Fatalf("after adoption parent=%s refs.pr=%q, want merged naming %s", got.State, got.Refs.PR, own)
	}
}

// TestHeldParentAdoptsThePRWhenItsChildClosesIt (hk 4172): a merge that names a
// parent is held while a sub-task is open, and records no PR on the parent.
// When the sub-task closes the parent, the parent adopts the held PR.
func TestHeldParentAdoptsThePRWhenItsChildClosesIt(t *testing.T) {
	s, pool := searchTestStore(t)
	parent := flowTask(t, s, pool, "implement", StateDeveloping, true)
	child := flowTask(t, s, pool, "implement", StateReviewing, true)
	setParent(t, s, child, parent)
	own := testPR(9601)
	if res := send(t, s, mergeEvent(parent.ID, own)); res.Applied {
		t.Fatalf("parent merge applied beside an open sub-task: %+v", res)
	}
	if got := stateOf(t, s, parent.ID); got.Refs.PR != "" {
		t.Fatalf("held merge set refs.pr=%q", got.Refs.PR)
	}
	if res := send(t, s, mergeEvent(child.ID, testPR(9602))); !res.Applied {
		t.Fatalf("child merge %+v", res)
	}
	got := stateOf(t, s, parent.ID)
	if got.State != StateMerged || got.Refs.PR != own {
		t.Fatalf("parent=%s refs.pr=%q, want merged adopting %s", got.State, got.Refs.PR, own)
	}
}

// TestRefusedMergeIsNotAdopted (hk 4172 review): a merge refused by a gate
// other than mergeHoldTx (here, the task is in triage) is recorded not applied
// but is not a held merge. When the task later leaves triage and an automatic
// close moves it to merged, it does not adopt that refused PR.
func TestRefusedMergeIsNotAdopted(t *testing.T) {
	s, pool := searchTestStore(t)
	parent := flowTask(t, s, pool, "implement", StateTriage, true)
	child := flowTask(t, s, pool, "implement", StateReviewing, true)
	setParent(t, s, child, parent)
	if res := send(t, s, mergeEvent(parent.ID, testPR(9701))); res.Applied {
		t.Fatalf("merge applied in triage: %+v", res)
	}
	move(t, s, parent.ID, StateBacklog)
	if res := send(t, s, mergeEvent(child.ID, testPR(9702))); !res.Applied {
		t.Fatalf("child merge %+v", res)
	}
	got := stateOf(t, s, parent.ID)
	if got.State != StateMerged || got.Refs.PR != "" {
		t.Fatalf("parent=%s refs.pr=%q, want merged with no adopted PR", got.State, got.Refs.PR)
	}
}
