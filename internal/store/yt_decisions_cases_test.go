package store

import (
	"errors"
	"testing"
	"time"
)

// The decision projection on both dialects: the create op the feed prepares
// for an unbound decision, the binding `youtrack decisions-backfill` publishes
// and its guards, and the two pages the CLI walks (hk 5077, hk 4899 B4b). A
// decision row is opened through the public task flow (a needs_decision flag),
// so the fixtures need no dialect-specific SQL; the decision's id is read back
// from UnboundYTDecisions.
func init() {
	// ytDecisionFixture opens one decision by flagging a task, and returns the
	// decision id the task flow recorded.
	ytDecisionFixture := func(t *testing.T, s *Store) int64 {
		t.Helper()
		ctx := t.Context()
		task, err := s.CreateTask(ctx, Task{Lane: "yt-decisions-case", Project: projectPtr("handoffkeep"), Title: "a decision",
			Kind: "implement", State: StateBacklog, CreatedBy: "tester"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.TransitionTaskWith(ctx, task.ID, TransitionInput{To: "needs_decision", By: "tester", Note: "which way?"}); err != nil {
			t.Fatal(err)
		}
		cands, err := s.UnboundYTDecisions(ctx, 0, YTDecisionCandidatesMax)
		if err != nil {
			t.Fatal(err)
		}
		if len(cands) == 0 {
			t.Fatal("the flagged task recorded no unbound decision")
		}
		// Newest first is not guaranteed by the page order, so take the max.
		id := cands[0].ID
		for _, c := range cands[1:] {
			if c.ID > id {
				id = c.ID
			}
		}
		return id
	}

	registerYTAreaCase("decisions/create-prepared-once", func(t *testing.T, s *Store) {
		ctx := t.Context()
		s.SetYTDecisionsMode(YTDecisionsOn, "yt.example", "0-1", "DEC")
		id := ytDecisionFixture(t, s)
		now := time.Now().UTC()

		outcome, err := s.PrepareYTDecision(ctx, id, now)
		if err != nil || outcome != YTDecisionCreatePrepared {
			t.Fatalf("first prepare: outcome=%q err=%v", outcome, err)
		}
		// The op exists now: the rule prepares no second create.
		outcome, err = s.PrepareYTDecision(ctx, id, now)
		if err != nil || outcome != YTDecisionCreatePending {
			t.Fatalf("second prepare: outcome=%q err=%v", outcome, err)
		}
		// An unresolved create op holds the backfill's binding.
		_, err = s.BindYTDecision(ctx, YTDecisionBinding{DecisionID: id, Tenant: "yt.example", IssueID: "3-500",
			IssueReadable: "DEC-500", ProjectID: "0-1", BoundBy: "tester"}, now)
		if !errors.Is(err, ErrYTDecisionOpPending) {
			t.Fatalf("bind against a pending create = %v, want ErrYTDecisionOpPending", err)
		}
		if _, found, err := s.YTDecisionBinding(ctx, id); err != nil || found {
			t.Fatalf("binding after the refused bind: found=%v err=%v", found, err)
		}
	})

	registerYTAreaCase("decisions/bind-then-bound-page", func(t *testing.T, s *Store) {
		ctx := t.Context()
		s.SetYTDecisionsMode(YTDecisionsOn, "yt.example", "0-1", "DEC")
		id := ytDecisionFixture(t, s)
		now := time.Now().UTC()
		b := YTDecisionBinding{DecisionID: id, Tenant: "yt.example", IssueID: "3-501", IssueReadable: "DEC-501",
			ProjectID: "0-1", BoundBy: "tester"}

		created, err := s.BindYTDecision(ctx, b, now)
		if err != nil || !created {
			t.Fatalf("bind: created=%v err=%v", created, err)
		}
		// The backfill's origin is the literal 'import_verified' (the
		// yt_decision_bindings CHECK), with no op.
		got, found, err := s.YTDecisionBinding(ctx, id)
		if err != nil || !found || got.Tenant != b.Tenant || got.IssueID != b.IssueID || got.Origin != "import_verified" || got.OpID != 0 {
			t.Fatalf("binding read back %+v found=%v err=%v", got, found, err)
		}
		bound, err := s.BoundYTDecisions(ctx, b.Tenant, 0, YTDecisionCandidatesMax)
		if err != nil || len(bound) != 1 || bound[0] != id {
			t.Fatalf("bound page = %v err=%v, want [%d]", bound, err, id)
		}
		// The bound decision left the unbound page.
		cands, err := s.UnboundYTDecisions(ctx, 0, YTDecisionCandidatesMax)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cands {
			if c.ID == id {
				t.Fatalf("bound decision still unbound: %+v", cands)
			}
		}
		// The same binding is kept, not written twice.
		if created, err := s.BindYTDecision(ctx, b, now); err != nil || created {
			t.Fatalf("repeat bind: created=%v err=%v", created, err)
		}
	})

	registerYTAreaCase("decisions/binding-conflicts", func(t *testing.T, s *Store) {
		ctx := t.Context()
		s.SetYTDecisionsMode(YTDecisionsOff, "", "", "")
		now := time.Now().UTC()
		first, second := ytDecisionFixture(t, s), ytDecisionFixture(t, s)
		if _, err := s.BindYTDecision(ctx, YTDecisionBinding{DecisionID: first, Tenant: "yt.example", IssueID: "3-511",
			ProjectID: "0-1", BoundBy: "tester"}, now); err != nil {
			t.Fatal(err)
		}
		// The decision is already bound to another issue.
		_, err := s.BindYTDecision(ctx, YTDecisionBinding{DecisionID: first, Tenant: "yt.example", IssueID: "3-512",
			ProjectID: "0-1", BoundBy: "tester"}, now)
		if !errors.Is(err, ErrYTDecisionBindingConflict) {
			t.Fatalf("second issue for one decision = %v, want ErrYTDecisionBindingConflict", err)
		}
		// The issue is already bound to another decision.
		_, err = s.BindYTDecision(ctx, YTDecisionBinding{DecisionID: second, Tenant: "yt.example", IssueID: "3-511",
			ProjectID: "0-1", BoundBy: "tester"}, now)
		if !errors.Is(err, ErrYTDecisionBindingConflict) {
			t.Fatalf("shared issue = %v, want ErrYTDecisionBindingConflict", err)
		}
		if _, found, err := s.YTDecisionBinding(ctx, second); err != nil || found {
			t.Fatalf("refused binding was written: found=%v err=%v", found, err)
		}
	})
}
