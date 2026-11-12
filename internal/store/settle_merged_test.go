package store

import (
	"slices"
	"testing"
)

// TestMergedCompletesOnlyWithEvidence needs a database: a merged task closes
// to completed only with closure evidence, the same validation completed uses.
func TestMergedCompletesOnlyWithEvidence(t *testing.T) {
	s := schemaStore(t, "merged_edge")
	x := flowTask(t, s, s.pool, "implement", StateBacklog, false)
	send(t, s, mergeEvent(x.ID, repoPR(changeRepo("merged_edge"), 11)))
	if got, _, err := s.GetTask(t.Context(), x.ID); err != nil || got.State != StateMerged {
		t.Fatalf("task %d state %s err %v, want merged", x.ID, got.State, err)
	}
	if _, err := s.TransitionTaskWith(t.Context(), x.ID, TransitionInput{To: StateCompleted, By: "tester"}); err == nil {
		t.Fatal("merged to completed without evidence was accepted")
	}
	if got, _, _ := s.GetTask(t.Context(), x.ID); got.State != StateMerged {
		t.Fatalf("state %s after refused close, want merged", got.State)
	}
	if _, err := s.TransitionTaskWith(t.Context(), x.ID, TransitionInput{To: StateCompleted, By: "tester", Evidence: "hk:doc research/x"}); err != nil {
		t.Fatalf("merged to completed with evidence: %v", err)
	}
	if got, _, _ := s.GetTask(t.Context(), x.ID); got.State != StateCompleted {
		t.Fatalf("state %s after close, want completed", got.State)
	}
}

// mergedWithSHA is a merged task whose merge event carries a merge commit, so
// settle-merged can match it to a deploy.
func (f deployFixture) mergedWithSHA(t *testing.T, n int) Task {
	t.Helper()
	x := flowTask(t, f.s, f.s.pool, "implement", StateBacklog, false)
	send(t, f.s, TaskSourceEvent{Source: SourceMergewakeMerge, SourceRef: repoPR(f.repo, n), TaskID: x.ID, MergeSHA: testSHA1})
	got, _, err := f.s.GetTask(t.Context(), x.ID)
	if err != nil || got.State != StateMerged {
		t.Fatalf("task %d state %s err %v", x.ID, got.State, err)
	}
	return got
}

func settleState(t *testing.T, s *Store, id int64) string {
	t.Helper()
	got, _, err := s.GetTask(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got.State
}

// TestSettleMergedReasons needs a database: one merged task per reason, a
// dry run that writes nothing, an apply that moves only the covered task, and
// a re-run that changes nothing.
func TestSettleMergedReasons(t *testing.T) {
	f := newDeployFixture(t, "settle", "prod")
	covered := f.mergedWithSHA(t, 1)
	notYet := f.mergedWithSHA(t, 2)
	noMergeSHA := f.mergedTask(t, 3)
	otherRepo := changeRepo("settle_other")
	noStage := flowTask(t, f.s, f.s.pool, "implement", StateBacklog, false)
	send(t, f.s, TaskSourceEvent{Source: SourceMergewakeMerge, SourceRef: repoPR(otherRepo, 7), TaskID: noStage.ID, MergeSHA: testSHA1})

	f.start(t, "s-1", "deploy", []string{"prod"}, f.cand(1, "internal/x.go"))
	f.finish(t, "s-1", "success")

	dry, err := f.s.SettleMerged(t.Context(), "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !dry.DryRun || dry.Counts[SettleDeployed] != 1 || dry.Counts[SettleNotYetDeployed] != 1 || dry.Counts[SettleNoDeployStage] != 1 || dry.Counts[SettleNoMergeCommit] != 1 {
		t.Fatalf("dry-run counts %+v", dry.Counts)
	}
	if !slices.Equal(dry.Tasks[SettleDeployed], []int64{covered.ID}) || !slices.Equal(dry.Tasks[SettleNotYetDeployed], []int64{notYet.ID}) {
		t.Fatalf("dry-run tasks %+v", dry.Tasks)
	}
	if got := settleState(t, f.s, covered.ID); got != StateMerged {
		t.Fatalf("dry run moved covered task to %s", got)
	}

	applied, err := f.s.SettleMerged(t.Context(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if applied.DryRun || applied.Counts[SettleDeployed] != 1 {
		t.Fatalf("apply counts %+v", applied.Counts)
	}
	if got := settleState(t, f.s, covered.ID); got != StateDeployed {
		t.Fatalf("covered task state %s after apply, want deployed", got)
	}
	for _, id := range []int64{notYet.ID, noMergeSHA.ID, noStage.ID} {
		if got := settleState(t, f.s, id); got != StateMerged {
			t.Fatalf("uncovered task %d state %s after apply, want merged", id, got)
		}
	}

	again, err := f.s.SettleMerged(t.Context(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if again.Counts[SettleDeployed] != 0 || again.Counts[SettleNotYetDeployed] != 1 {
		t.Fatalf("re-run counts %+v, want nothing newly deployed", again.Counts)
	}
	if got := settleState(t, f.s, covered.ID); got != StateDeployed {
		t.Fatalf("covered task state %s after re-run, want deployed", got)
	}
}

// TestSettleMergedRejectsBadProject needs no database: a project filter must be
// a project name before any query runs.
func TestSettleMergedRejectsBadProject(t *testing.T) {
	var s *Store
	if _, err := s.SettleMerged(t.Context(), "not a project!", true); err == nil {
		t.Fatal("bad project name was accepted")
	}
}
