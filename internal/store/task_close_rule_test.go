package store

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestExplicitMergeLeavesProgrammeTaskOpen is the hk 4896 shape through the
// public event path: an explicit task whose only linked PR merges. The change
// is merged and linked to the task, and the task keeps its state. An auto task
// with the same PR closes, as it always has.
func TestExplicitMergeLeavesProgrammeTaskOpen(t *testing.T) {
	s, pool := searchTestStore(t)
	x := flowTask(t, s, pool, "implement", StateDeveloping, true)
	if _, _, err := s.SetTaskCloseRule(t.Context(), x.ID, CloseRuleExplicit, "tester", "programme task"); err != nil {
		t.Fatal(err)
	}
	pr := testPR(5112101)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: pr, TaskID: x.ID})
	res := send(t, s, mergeEvent(x.ID, pr))
	if res.Applied || res.State != StateReviewing || !strings.Contains(res.Reason, "by hand") {
		t.Fatalf("merge of an explicit task: %+v, want held at reviewing by hand", res)
	}
	if got := stateOf(t, s, x.ID); got.State != StateReviewing || got.Refs.PR != "" {
		t.Fatalf("explicit task state=%s refs.pr=%q", got.State, got.Refs.PR)
	}
	c := mustChange(t, s, "example/app", 5112101)
	if c.State != ChangeMerged {
		t.Fatalf("the change did not record its merge: %+v", c)
	}
	if links := changeLinks(t, s, c.ID); links[x.ID] != ChangeImplements {
		t.Fatalf("the change is not linked to the explicit task: %v", links)
	}

	y := flowTask(t, s, pool, "implement", StateDeveloping, true)
	autoPR := testPR(5112102)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: autoPR, TaskID: y.ID})
	if res := send(t, s, mergeEvent(y.ID, autoPR)); !res.Applied || res.State != StateMerged {
		t.Fatalf("merge of an auto task: %+v, want merged", res)
	}
}

// TestDriftSeparatesExplicitFromMergedPRNotMerged: the same drift shape, a
// merged change linked to an open task, is reported as merged_pr_not_merged for
// an auto task and as explicit_all_settled for an explicit one. Neither class
// lists the other's task.
func TestDriftSeparatesExplicitFromMergedPRNotMerged(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	explicit := flowTask(t, s, pool, "implement", StateDeveloping, true)
	auto := flowTask(t, s, pool, "implement", StateDeveloping, true)
	parked := flowTask(t, s, pool, "implement", StateIcebox, true)
	for _, x := range []Task{explicit, parked} {
		if _, _, err := s.SetTaskCloseRule(ctx, x.ID, CloseRuleExplicit, "tester", "programme task"); err != nil {
			t.Fatal(err)
		}
	}
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: testPR(5112201), TaskID: explicit.ID})
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: testPR(5112202), TaskID: auto.ID})
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: testPR(5112203), TaskID: parked.ID})
	if _, err := pool.Exec(ctx, `UPDATE changes SET state='merged' WHERE repo='example/app' AND pr_number IN (5112201,5112202,5112203)`); err != nil {
		t.Fatal(err)
	}
	// The icebox task must really implement its merged change, or the check
	// below would pass without testing the state filter.
	if links := changeLinks(t, s, mustChange(t, s, "example/app", 5112203).ID); links[parked.ID] != ChangeImplements {
		t.Fatalf("setup: the icebox task is not linked to its change: %v", links)
	}

	d := driftAt(t, s, time.Now().UTC(), DriftDefaultMergedDays)
	settled, lagging := driftIDs(d.ExplicitAllSettled), driftIDs(d.MergedPRNotMerged)
	if !settled[explicit.ID] || settled[auto.ID] {
		t.Fatalf("explicit_all_settled ids %v, want the explicit task only", settled)
	}
	if settled[parked.ID] {
		t.Fatalf("explicit_all_settled lists an icebox task %d, where no close is due", parked.ID)
	}
	if lagging[explicit.ID] || !lagging[auto.ID] {
		t.Fatalf("merged_pr_not_merged ids %v, want the auto task only", lagging)
	}
}

// TestTaskUpdateSetsCloseRule: the MCP task_update path takes close_rule with
// the same revision check and idempotency as its other fields.
func TestTaskUpdateSetsCloseRule(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	seeded := flowTask(t, s, pool, "implement", StateBacklog, false)
	w := MCPWrite{Tool: mcpTaskUpdateTool, Actor: "tester", Channel: "mcp", Author: "tester", Key: "ctr-update-1"}
	in := TaskUpdateInput{ExpectedUpdatedAt: stateOf(t, s, seeded.ID).UpdatedAt, CloseRule: CloseRuleExplicit, Note: "programme task"}
	res, err := s.UpdateTask(ctx, seeded.ID, in, w)
	if err != nil || !slices.Contains(res.Changed, "close_rule") || res.Task.CloseRule != CloseRuleExplicit {
		t.Fatalf("task_update close_rule: res=%+v err=%v", res, err)
	}
	replay, err := s.UpdateTask(ctx, seeded.ID, in, w)
	if err != nil || !replay.Replayed || replay.Task.CloseRule != CloseRuleExplicit {
		t.Fatalf("replayed task_update: res=%+v err=%v", replay, err)
	}
	bad := TaskUpdateInput{ExpectedUpdatedAt: res.Task.UpdatedAt, CloseRule: "sometimes"}
	if _, err := s.UpdateTask(ctx, seeded.ID, bad, MCPWrite{Tool: mcpTaskUpdateTool, Actor: "tester", Channel: "mcp", Author: "tester", Key: "ctr-update-2"}); !errors.Is(err, ErrInvalidTaskUpdate) {
		t.Fatalf("unknown close_rule: err=%v, want ErrInvalidTaskUpdate", err)
	}
}

// TestExplicitAdoptionDoesNotDeploy: an explicit parent that a rollup or a hand
// move left merged, with no PR of its own, does not adopt a merge's PR: the
// merge is recorded as a held merge, and a deploy record that lists the PR does
// not move the task to deployed. An auto parent in the same shape adopts the PR.
func TestExplicitAdoptionDoesNotDeploy(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	parent := flowTask(t, s, pool, "implement", StateMerged, false)
	if _, _, err := s.SetTaskCloseRule(ctx, parent.ID, CloseRuleExplicit, "tester", "programme parent"); err != nil {
		t.Fatal(err)
	}
	child := flowTask(t, s, pool, "implement", StateBacklog, false)
	setParent(t, s, child, parent)
	pr := testPR(5112301)
	putDeploy(t, s, "deploy/svc/20261118T010000Z", "svc", "success", []string{pr})

	res := send(t, s, mergeEvent(parent.ID, pr))
	if res.Applied || res.State != StateMerged || !strings.Contains(res.Reason, "by hand") {
		t.Fatalf("explicit adoption: %+v, want held by hand, still merged", res)
	}
	got := stateOf(t, s, parent.ID)
	if got.State != StateMerged || got.Refs.PR != "" {
		t.Fatalf("explicit parent state=%s refs.pr=%q, want merged with no PR adopted", got.State, got.Refs.PR)
	}
	rows := sourceRows(t, s, parent.ID, SourceMergewakeMerge)
	if len(rows) != 1 || rows[0].Applied || rows[0].Field != heldMergeField {
		t.Fatalf("merge rows %+v, want one held, not-applied row", rows)
	}

	auto := flowTask(t, s, pool, "implement", StateMerged, false)
	autoChild := flowTask(t, s, pool, "implement", StateBacklog, false)
	setParent(t, s, autoChild, auto)
	autoPR := testPR(5112302)
	putDeploy(t, s, "deploy/svc/20261118T020000Z", "svc", "success", []string{autoPR})
	if res := send(t, s, mergeEvent(auto.ID, autoPR)); !res.Applied {
		t.Fatalf("auto adoption: %+v, want applied", res)
	}
}
