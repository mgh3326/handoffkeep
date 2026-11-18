package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Dialect-neutral cases for the close rule (hk 5112, track B-2). Each runs on
// PostgreSQL and on MySQL. An explicit task's holds are checked before any
// change or decision read, so they hold on MySQL too, where a merge to a closed
// state of an auto task is still refused (srcEvDecisionGate).

func init() {
	registerTaskAreaCase("close_rule/set_and_read", ctrCaseSetAndRead)
	registerTaskAreaCase("close_rule/explicit_holds_a_merge", ctrCaseHoldsMerge)
	registerTaskAreaCase("close_rule/explicit_parent_holds_auto_close", ctrCaseHoldsParent)
}

func ctrCaseSetAndRead(t *testing.T, s *Store) {
	ctx := t.Context()
	x := srcEvTask(t, s, "close rule")
	if x.CloseRule != CloseRuleAuto {
		t.Fatalf("a new task's close_rule=%q, want auto", x.CloseRule)
	}
	if _, _, err := s.SetTaskCloseRule(ctx, x.ID, "sometimes", "tester", ""); !errors.Is(err, ErrInvalidCloseRule) {
		t.Fatalf("unknown rule: err=%v, want ErrInvalidCloseRule", err)
	}
	got, changed, err := s.SetTaskCloseRule(ctx, x.ID, CloseRuleExplicit, "tester", "programme task")
	if err != nil || !changed || got.CloseRule != CloseRuleExplicit {
		t.Fatalf("set explicit: task=%+v changed=%t err=%v", got, changed, err)
	}
	if _, changed, err = s.SetTaskCloseRule(ctx, x.ID, CloseRuleExplicit, "tester", ""); err != nil || changed {
		t.Fatalf("set the same rule again: changed=%t err=%v, want no change", changed, err)
	}
	after := srcEvGet(t, s, x.ID)
	if after.CloseRule != CloseRuleExplicit || after.State != x.State {
		t.Fatalf("after set: close_rule=%q state=%q, want explicit and %q", after.CloseRule, after.State, x.State)
	}
	var recorded bool
	for _, e := range after.Events {
		if e.Kind == "metadata" && e.Field == "close_rule" && e.FromValue == CloseRuleAuto && e.ToValue == CloseRuleExplicit && e.Note == "close rule auto → explicit: programme task" {
			recorded = true
		}
	}
	if !recorded {
		t.Fatalf("no close_rule event: %+v", after.Events)
	}
}

// ctrCaseHoldsMerge is the hk 4896 shape: a programme task whose only linked PR
// merges. The task keeps its state and the merge is recorded as not applied.
// Its forward move, the PR opening, still applies.
func ctrCaseHoldsMerge(t *testing.T, s *Store) {
	ctx := t.Context()
	x := srcEvTask(t, s, "programme")
	if _, _, err := s.SetTaskCloseRule(ctx, x.ID, CloseRuleExplicit, "tester", "programme task"); err != nil {
		t.Fatal(err)
	}
	x = srcEvGet(t, s, x.ID)
	pr := srcEvPR(srcEvUnique())
	res, err := srcEvApplyTx(s, t, x, srcEvPREvent(pr))
	if err != nil || !res.Applied || res.State != StateReviewing {
		t.Fatalf("PR opened on an explicit task: res=%+v err=%v, want the forward move to reviewing", res, err)
	}
	x = srcEvGet(t, s, x.ID)
	merge := sourceEvent{Source: SourceMergewakeMerge, SourceRef: pr, PR: pr, MergeSHA: "0123456789abcdef", Named: map[int64]bool{}}
	res, err = srcEvApplyTx(s, t, x, merge)
	if err != nil {
		t.Fatalf("merge of an explicit task: err=%v", err)
	}
	if res.Applied || res.State != StateReviewing || !strings.Contains(res.Reason, "by hand") {
		t.Fatalf("merge of an explicit task: res=%+v, want held at reviewing by hand", res)
	}
	after := srcEvGet(t, s, x.ID)
	if after.State != StateReviewing || after.CompletedAt != nil {
		t.Fatalf("explicit task closed by a merge: state=%q completed_at=%v", after.State, after.CompletedAt)
	}
	rows := srcEvEventsOf(after, SourceMergewakeMerge)
	if len(rows) != 1 || rows[0].Applied || !strings.Contains(rows[0].Note, "by hand") || rows[0].Field != heldMergeField {
		t.Fatalf("merge rows %+v, want one not-applied held-merge row naming the hold", rows)
	}
	again, err := srcEvApplyTx(s, t, after, merge)
	if err != nil || !again.Redelivered || again.Applied {
		t.Fatalf("redelivered merge: res=%+v err=%v", again, err)
	}
}

// ctrCaseHoldsParent is the hk 4899 shape: an explicit ops parent whose only
// child closes. The parent stays open, the held rollup is recorded, and drift
// lists it once its pieces are settled.
func ctrCaseHoldsParent(t *testing.T, s *Store) {
	ctx := t.Context()
	parent := flowCaseTask(t, s, "ops")
	if _, _, err := s.SetTaskCloseRule(ctx, parent.ID, CloseRuleExplicit, "tester", "programme parent"); err != nil {
		t.Fatal(err)
	}
	child := flowCaseTask(t, s, "ops")
	_, _, err := s.SetTaskParent(ctx, child.ID, &parent.ID, "tester")
	flowMust(t, err)

	_, err = s.CloseTaskCompleted(ctx, child.ID, "hk:doc research/close-rule", "tester", "child done", "ctr-parent-1")
	flowSkipUnported(t, s, err)
	got := flowCaseGet(t, s, parent.ID)
	if got.State != StateBacklog || got.Refs.Closure != nil {
		t.Fatalf("explicit parent closed by its child: state=%q closure=%v", got.State, got.Refs.Closure)
	}
	var held bool
	for _, e := range got.Events {
		if e.Kind == TaskEventAutoClose && !e.Applied && strings.Contains(e.Note, "by hand") {
			held = true
		}
	}
	if !held {
		t.Fatalf("no held auto_close record on the explicit parent: %+v", got.Events)
	}

	needTables(t, s, "decision_requests and jobs")
	now := time.Now().UTC()
	d, err := s.TaskDrift(ctx, now, DriftDefaultMergedDays)
	if err != nil {
		t.Fatal(err)
	}
	if !driftIDs(d.ExplicitAllSettled)[parent.ID] {
		t.Fatalf("explicit_all_settled does not list the parent %d: %+v", parent.ID, d.ExplicitAllSettled.Tasks)
	}
	if driftIDs(d.AutoCloseHeld)[parent.ID] {
		t.Fatalf("auto_close_held lists an explicit parent %d", parent.ID)
	}
	rep, err := s.TaskHygiene(ctx, now, "", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rep.Findings.AutoCloseHeldParents {
		if f.ID == parent.ID {
			t.Fatalf("hygiene lists the explicit parent %d as a finding", parent.ID)
		}
	}
}
