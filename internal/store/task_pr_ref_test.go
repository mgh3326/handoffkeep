package store

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// setParent links child under parent, the bundle shape: parent_id names the
// bundle task.
func linkUnderParent(t *testing.T, s *Store, child, parent int64) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), `UPDATE tasks SET parent_id=$2 WHERE id=$1`, child, parent); err != nil {
		t.Fatal(err)
	}
}

func setTitle(t *testing.T, s *Store, id int64, title string) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), `UPDATE tasks SET title=$2 WHERE id=$1`, id, title); err != nil {
		t.Fatal(err)
	}
}

// closeNoJob closes a task to merged with no PR of its own, the way the
// --no-job chain of a bundle close does (wake, §Escalation and notifications).
func closeNoJob(t *testing.T, s *Store, id int64) {
	t.Helper()
	if _, err := s.TransitionTaskWith(t.Context(), id, TransitionInput{To: StateMerged, By: "tester", Note: "delivered by the bundle", NoJob: "bundle close"}); err != nil {
		t.Fatalf("close %d to merged: %v", id, err)
	}
}

// TestBarePRRefIsStoredAsURL needs a database: a bare PR number written to a
// task of a project whose PRs live in one repo is stored as that repo's URL.
func TestBarePRRefIsStoredAsURL(t *testing.T) {
	s, pool := searchTestStore(t)
	send(t, s, mergeEvent(flowTask(t, s, pool, "implement", StateDeveloping, false).ID, testPR(301)))
	x := flowTask(t, s, pool, "implement", StateDeveloping, false)
	if _, err := s.TransitionTaskWith(t.Context(), x.ID, TransitionInput{To: StateReviewing, By: "tester", Refs: &TaskRefs{PR: "302"}}); err != nil {
		t.Fatalf("transition with a bare PR number: %v", err)
	}
	if got := stateOf(t, s, x.ID); got.Refs.PR != testPR(302) {
		t.Fatalf("refs.pr %q, want %q", got.Refs.PR, testPR(302))
	}
	created, err := s.CreateTask(t.Context(), Task{Lane: relaneLane(t, "flow"), Title: "bare create", Kind: "implement", Priority: 5, CreatedBy: "tester", Project: projectPtr(testProjectName), Refs: TaskRefs{PR: "303"}})
	if err != nil {
		t.Fatalf("create with a bare PR number: %v", err)
	}
	if created.Refs.PR != testPR(303) {
		t.Fatalf("created refs.pr %q, want %q", created.Refs.PR, testPR(303))
	}
}

// TestBarePRRefRefusedWithoutOneRepo needs a database: when the project's PRs
// live in two repos, a bare number names no PR, so the transition is refused
// and the task is left as it was.
func TestBarePRRefRefusedWithoutOneRepo(t *testing.T) {
	s, pool := searchTestStore(t)
	send(t, s, mergeEvent(flowTask(t, s, pool, "implement", StateDeveloping, false).ID, testPR(311)))
	other := changeRepo("bare_pr_other")
	send(t, s, mergeEvent(flowTask(t, s, pool, "implement", StateDeveloping, false).ID, repoPR(other, 5)))
	x := flowTask(t, s, pool, "implement", StateDeveloping, false)
	_, err := s.TransitionTaskWith(t.Context(), x.ID, TransitionInput{To: StateReviewing, By: "tester", Refs: &TaskRefs{PR: "302"}})
	if !errors.Is(err, ErrInvalidChangeWrite) {
		t.Fatalf("bare PR number with two repos in the project: err %v, want invalid_context", err)
	}
	if got := stateOf(t, s, x.ID); got.State != StateDeveloping || got.Refs.PR != "" {
		t.Fatalf("refused transition left state %s refs.pr %q", got.State, got.Refs.PR)
	}
}

// TestBundleSiblingDeploysWithItsPR needs a database: a child of a bundle
// task ("[bundle ...]") that closes with no PR of its own reaches deployed
// with the bundle's PR, whether it closes before the deploy record lands or
// after. A replayed record changes nothing.
func TestBundleSiblingDeploysWithItsPR(t *testing.T) {
	s, pool := searchTestStore(t)
	bundle := flowTask(t, s, pool, "implement", StateDeveloping, false)
	setTitle(t, s, bundle.ID, "[bundle hk4844 B01] deploy reach")
	pr := testPR(401)
	send(t, s, mergeEvent(bundle.ID, pr))

	early := flowTask(t, s, pool, "implement", StateReviewing, false)
	linkUnderParent(t, s, early.ID, bundle.ID)
	closeNoJob(t, s, early.ID)
	key := "deploy/svc/20261105T070000Z"
	putDeploy(t, s, key, "svc", "success", []string{pr})
	if got := stateOf(t, s, early.ID); got.State != StateDeployed {
		t.Fatalf("sibling closed before the deploy: state %s, want deployed", got.State)
	}
	if rows := sourceRows(t, s, early.ID, SourceDeploy); len(rows) != 1 || !rows[0].Applied || rows[0].SourceRef != deployRef(key, pr) {
		t.Fatalf("sibling deploy rows %+v", rows)
	}

	late := flowTask(t, s, pool, "implement", StateReviewing, false)
	linkUnderParent(t, s, late.ID, bundle.ID)
	putDeploy(t, s, "deploy/svc/20261105T075000Z", "svc", "success", []string{pr})
	closeNoJob(t, s, late.ID)
	if got := stateOf(t, s, late.ID); got.State != StateDeployed {
		t.Fatalf("sibling closed after the deploy: state %s, want deployed", got.State)
	}

	putDeploy(t, s, key, "svc", "success", []string{pr})
	if rows := sourceRows(t, s, early.ID, SourceDeploy); len(rows) != 1 {
		t.Fatalf("replayed deploy record wrote %d deploy rows, want 1", len(rows))
	}
}

// TestEpicPRDoesNotDeliverChild needs a database: only a bundle parent passes
// its PR down, so a child of an ordinary parent that closes with no PR of its
// own stays merged when the parent's PR deploys.
func TestEpicPRDoesNotDeliverChild(t *testing.T) {
	s, pool := searchTestStore(t)
	epic := flowTask(t, s, pool, "implement", StateDeveloping, false)
	setTitle(t, s, epic.ID, "epic: deploy reach")
	pr := testPR(402)
	send(t, s, mergeEvent(epic.ID, pr))
	child := flowTask(t, s, pool, "implement", StateReviewing, false)
	linkUnderParent(t, s, child.ID, epic.ID)
	closeNoJob(t, s, child.ID)
	putDeploy(t, s, "deploy/svc/20261105T080000Z", "svc", "success", []string{pr})
	if got := stateOf(t, s, epic.ID); got.State != StateDeployed {
		t.Fatalf("epic with its own PR: state %s, want deployed", got.State)
	}
	if got := stateOf(t, s, child.ID); got.State != StateMerged {
		t.Fatalf("child of an epic: state %s, want merged", got.State)
	}
}

// TestReconciledChangeSettlesHeldTask needs a database: the shape of the four
// held tasks of hk 4840. A task merged by PR 1 while PR 2, unreconciled in hk
// (its row merged by the deploy with no merge commit), implements it holds in
// merged. A deploy that covers both PRs, finished
// before the open link, does not move it. Once PR 2 is reconciled to merged,
// settle-merged moves it, and a re-run changes nothing.
func TestReconciledChangeSettlesHeldTask(t *testing.T) {
	f := newDeployFixture(t, "reconcile", "prod")
	held := flowTask(t, f.s, f.s.pool, "implement", StateBacklog, false)
	// The deploy finishes before the task has any change link, so there is
	// nothing for a task rule to act on. The later merge and
	// the open PR 2 link then leave the task merged and held, the state of the
	// four held tasks; settle-merged reaches it only after PR 2 is reconciled.
	f.start(t, "s-reconcile", "deploy", []string{"prod"}, f.cand(1, "internal/a.go"), f.cand(2, "internal/b.go"))
	f.finish(t, "s-reconcile", "success")
	send(t, f.s, TaskSourceEvent{Source: SourceMergewakeMerge, SourceRef: repoPR(f.repo, 1), TaskID: held.ID, MergeSHA: testSHA1})
	send(t, f.s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: repoPR(f.repo, 2), TaskID: held.ID})
	if got := settleState(t, f.s, held.ID); got != StateMerged {
		t.Fatalf("held task state %s, want merged", got)
	}

	before, err := f.s.SettleMerged(t.Context(), "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(before.Tasks[SettleNoMergeCommit], held.ID) {
		t.Fatalf("before reconcile: %+v, want the task under no_merge_commit", before.Tasks)
	}

	open, err := f.s.ListUnreconciledChanges(t.Context(), nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(open, func(c Change) bool { return c.Repo == f.repo && c.PRNumber == 2 }) {
		t.Fatalf("unreconciled changes do not hold PR 2: %+v", open)
	}
	if _, err := f.s.ApplyChangeState(t.Context(), ChangeRef{Repo: f.repo, PR: 2}, ChangeStateWrite{HeadSHA: testSHA2, State: ChangeMerged, MergeSHA: testSHA2, ObservedAt: time.Now().UTC(), Source: "poll"}); err != nil {
		t.Fatalf("reconcile PR 2 to merged: %v", err)
	}
	open, err = f.s.ListUnreconciledChanges(t.Context(), nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(open, func(c Change) bool { return c.Repo == f.repo && c.PRNumber == 2 }) {
		t.Fatalf("reconciled PR 2 is still unreconciled: %+v", open)
	}
	after, err := f.s.SettleMerged(t.Context(), "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(after.Tasks[SettleDeployed], held.ID) {
		t.Fatalf("after reconcile: %+v, want the task under deployed", after.Tasks)
	}
	if _, err := f.s.SettleMerged(t.Context(), "", false); err != nil {
		t.Fatal(err)
	}
	if got := settleState(t, f.s, held.ID); got != StateDeployed {
		t.Fatalf("held task state %s after settle-merged, want deployed", got)
	}
	again, err := f.s.SettleMerged(t.Context(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if again.Counts[SettleDeployed] != 0 {
		t.Fatalf("re-run counts %+v, want nothing newly deployed", again.Counts)
	}
}

// TestBundleChildRevertReachesChild needs a database: a revert of the bundle
// PR that delivered a child with no PR of its own rolls the child back too, as
// it rolls back a task linked through its own refs.pr (propagateRevertTx).
func TestBundleChildRevertReachesChild(t *testing.T) {
	s, pool := searchTestStore(t)
	bundle := flowTask(t, s, pool, "implement", StateDeveloping, false)
	setTitle(t, s, bundle.ID, "[bundle hk4844 B02] revert reach")
	pr := testPR(411)
	send(t, s, mergeEvent(bundle.ID, pr))
	child := flowTask(t, s, pool, "implement", StateReviewing, false)
	linkUnderParent(t, s, child.ID, bundle.ID)
	closeNoJob(t, s, child.ID)
	putDeploy(t, s, "deploy/svc/20261105T090000Z", "svc", "success", []string{pr})
	if got := stateOf(t, s, child.ID); got.State != StateDeployed {
		t.Fatalf("child before the revert: state %s, want deployed", got.State)
	}
	send(t, s, revertEvent(bundle.ID, pr, testPR(412)))
	if got := stateOf(t, s, child.ID); got.State != StateDeveloping {
		t.Fatalf("child after the bundle PR was reverted: state %s, want developing", got.State)
	}
}

// TestTriageExitResolvesBarePRInDestinationProject needs a database: a triage
// exit that sets a project leaves the task there, so a bare PR number on that
// transition resolves against the destination project's repo, not the old one.
func TestTriageExitResolvesBarePRInDestinationProject(t *testing.T) {
	s, pool := searchTestStore(t)
	// experiment's PRs live in example/app (testPR).
	send(t, s, mergeEvent(flowTask(t, s, pool, "implement", StateDeveloping, false).ID, testPR(421)))
	// handoffkeep's PRs live in another repo.
	dest := changeRepo("triage_dest")
	destTask, err := s.CreateTask(t.Context(), Task{Lane: relaneLane(t, "flow"), Title: "dest", Kind: "implement", Priority: 5, CreatedBy: "tester", Project: projectPtr("handoffkeep")})
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, TaskSourceEvent{Source: SourceMergewakeMerge, SourceRef: repoPR(dest, 7), TaskID: destTask.ID, MergeSHA: testSHA1})

	x := flowTask(t, s, pool, "implement", StateTriage, false)
	if _, err := s.TransitionTaskWith(t.Context(), x.ID, TransitionInput{To: StateBacklog, By: "tester", Project: projectPtr("handoffkeep"), Refs: &TaskRefs{PR: "302"}}); err != nil {
		t.Fatalf("triage exit with a bare PR number: %v", err)
	}
	if got := stateOf(t, s, x.ID); got.Refs.PR != repoPR(dest, 302) {
		t.Fatalf("refs.pr %q, want %q (the destination project's repo)", got.Refs.PR, repoPR(dest, 302))
	}
}

// TestBarePRRefLongDigitsRefused needs a database: a run of more than nine
// digits is refused, never stored as text.
func TestBarePRRefLongDigitsRefused(t *testing.T) {
	s, pool := searchTestStore(t)
	send(t, s, mergeEvent(flowTask(t, s, pool, "implement", StateDeveloping, false).ID, testPR(431)))
	x := flowTask(t, s, pool, "implement", StateDeveloping, false)
	_, err := s.TransitionTaskWith(t.Context(), x.ID, TransitionInput{To: StateReviewing, By: "tester", Refs: &TaskRefs{PR: "1234567890"}})
	if !errors.Is(err, ErrInvalidChangeWrite) {
		t.Fatalf("ten-digit PR number: err %v, want invalid_context", err)
	}
}
