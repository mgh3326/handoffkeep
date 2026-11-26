package store

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Dialect-neutral cases for task_source_events.go (registry:
// tasks_area_cases_test.go). ApplyTaskSourceEvent runs on both dialects. The
// event rules run through applySourceEventTx on both, the way
// every producer other than the API reaches them (settle-merged, the job
// role, the deploy record, the automatic closes), inside the task-mutation
// lock.

func init() {
	registerTaskAreaCase("source_events/invalid_events_are_refused", srcEvCaseInvalid)
	registerTaskAreaCase("source_events/unknown_task", srcEvCaseUnknownTask)
	registerTaskAreaCase("source_events/public_apply", srcEvCasePublicApply)
	registerTaskAreaCase("source_events/pr_opened_applies_and_redelivers", srcEvCasePROpened)
	registerTaskAreaCase("source_events/not_applied_is_recorded", srcEvCaseNotApplied)
	registerTaskAreaCase("source_events/merge_event", srcEvCaseMerge)
	registerTaskAreaCase("source_events/followup_subtasks_do_not_hold", srcEvCaseFollowupSubtasks)
	registerTaskAreaCase("source_events/concurrent_redelivery_applies_once", srcEvCaseConcurrent)
	registerTaskAreaCase("source_events/deploy_record_without_merged_task", srcEvCaseDeployRecordNoTask)
}

var srcEvSeq int64
var srcEvSeqMu sync.Mutex

// srcEvUnique returns a number no other call of this process returns.
func srcEvUnique() int64 {
	srcEvSeqMu.Lock()
	defer srcEvSeqMu.Unlock()
	srcEvSeq++
	return time.Now().UnixNano()%1_000_000 + srcEvSeq
}

// srcEvPR is a PR URL whose number fits the changes column.
func srcEvPR(n int64) string {
	return fmt.Sprintf("https://github.com/example/app/pull/%d", n)
}

// srcEvTask files a task in a fresh project and claims it without a job, so
// it is in a started state an event can move forward.
func srcEvTask(t *testing.T, s *Store, title string) Task {
	t.Helper()
	ctx := t.Context()
	n := srcEvUnique()
	project := fmt.Sprintf("srcev-%d", n)
	if _, err := s.AddTaskProject(ctx, project, "tester"); err != nil {
		t.Fatal(err)
	}
	x, err := s.CreateTask(ctx, Task{Lane: fmt.Sprintf("srcev-lane-%d", n), Title: title, Kind: "implement", Priority: 5, CreatedBy: "tester", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	if x, err = s.ClaimTask(ctx, x.ID, "tester", "", "source event case"); err != nil {
		t.Fatal(err)
	}
	return x
}

func srcEvGet(t *testing.T, s *Store, id int64) Task {
	t.Helper()
	x, found, err := s.GetTask(t.Context(), id)
	if err != nil || !found {
		t.Fatalf("task %d found=%t err=%v", id, found, err)
	}
	return x
}

// srcEvEventsOf lists the task's rows of one source.
func srcEvEventsOf(x Task, source string) []TaskEvent {
	var out []TaskEvent
	for _, e := range x.Events {
		if e.Source == source {
			out = append(out, e)
		}
	}
	return out
}

// srcEvCommentCount counts the task's system comments. It is the one raw
// statement of these cases: no public reader of comments is common to both
// dialects yet.
func srcEvCommentCount(t *testing.T, s *Store, id int64) int {
	t.Helper()
	var n int
	err := s.db().QueryRow(t.Context(), s.q(`SELECT count(*) FROM task_comments WHERE task_id=$1 AND system_event_key IS NOT NULL`, `SELECT count(*) FROM task_comments WHERE task_id=? AND system_event_key IS NOT NULL`), id).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// srcEvApplyTx runs one event under the task-mutation lock in its own
// transaction, as the producers do. x is the task as the caller read it.
func srcEvApplyTx(s *Store, t *testing.T, x Task, ev sourceEvent) (TaskEventResult, error) {
	t.Helper()
	ctx := t.Context()
	tx, release, err := s.begin(ctx)
	if err != nil {
		return TaskEventResult{}, err
	}
	defer release()
	defer tx.Rollback(ctx)
	if err = s.lockTx(ctx, tx, 8241873001); err != nil {
		return TaskEventResult{}, err
	}
	now := time.Now().UTC()
	if ev.At.IsZero() {
		ev.At = now
	}
	res, err := s.applySourceEventTx(ctx, tx, x, ev, now)
	if err != nil {
		return TaskEventResult{}, err
	}
	return res, tx.Commit(ctx)
}

func srcEvPREvent(pr string) sourceEvent {
	return sourceEvent{Source: SourceMergewakePR, SourceRef: pr, PR: pr, Named: map[int64]bool{}}
}

func srcEvCaseInvalid(t *testing.T, s *Store) {
	ctx := t.Context()
	pr := srcEvPR(srcEvUnique())
	for name, in := range map[string]TaskSourceEvent{
		"no task":            {Source: SourceMergewakePR, SourceRef: pr},
		"source_ref not PR":  {Source: SourceMergewakePR, SourceRef: "not a url", TaskID: 1},
		"unknown source":     {Source: "hk.sweep", SourceRef: pr, TaskID: 1},
		"pr differs":         {Source: SourceMergewakePR, SourceRef: pr, PR: srcEvPR(srcEvUnique()), TaskID: 1},
		"revert without pr":  {Source: SourceMergewakeRevert, SourceRef: pr, TaskID: 1},
		"duplicate task ids": {Source: SourceMergewakePR, SourceRef: pr, TaskID: 1, Tasks: []int64{2, 2}},
	} {
		if _, err := s.ApplyTaskSourceEvent(ctx, in); err == nil || errors.Is(err, ErrTaskNotFound) {
			t.Errorf("%s: err=%v, want a validation error", name, err)
		}
	}
}

func srcEvCaseUnknownTask(t *testing.T, s *Store) {
	pr := srcEvPR(srcEvUnique())
	_, err := s.ApplyTaskSourceEvent(t.Context(), TaskSourceEvent{Source: SourceMergewakePR, SourceRef: pr, TaskID: 1 << 40})
	if !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("event for a missing task: err=%v, want ErrTaskNotFound", err)
	}
}

func srcEvCasePublicApply(t *testing.T, s *Store) {
	ctx := t.Context()
	x := srcEvTask(t, s, "public apply")
	pr := srcEvPR(srcEvUnique())
	in := TaskSourceEvent{Source: SourceMergewakePR, SourceRef: pr, TaskID: x.ID}
	res, err := s.ApplyTaskSourceEvent(ctx, in)
	if err != nil || !res.Applied || res.State != StateReviewing {
		t.Fatalf("pr event: res=%+v err=%v", res, err)
	}
	again, err := s.ApplyTaskSourceEvent(ctx, in)
	if err != nil || again.Applied || !again.Redelivered {
		t.Fatalf("redelivery: res=%+v err=%v", again, err)
	}
	if rows := srcEvEventsOf(srcEvGet(t, s, x.ID), SourceMergewakePR); len(rows) != 1 || !rows[0].Applied {
		t.Fatalf("event rows after a redelivery: %+v", rows)
	}
}

func srcEvCasePROpened(t *testing.T, s *Store) {
	x := srcEvTask(t, s, "pr opened")
	pr := srcEvPR(srcEvUnique())
	res, err := srcEvApplyTx(s, t, x, srcEvPREvent(pr))
	if err != nil || !res.Applied || res.Redelivered || res.State != StateReviewing {
		t.Fatalf("pr event: res=%+v err=%v", res, err)
	}
	after := srcEvGet(t, s, x.ID)
	if after.State != StateReviewing {
		t.Fatalf("state=%s, want reviewing", after.State)
	}
	rows := srcEvEventsOf(after, SourceMergewakePR)
	if len(rows) != 1 || !rows[0].Applied || rows[0].SourceRef != pr || rows[0].To != StateReviewing || !rows[0].Auto {
		t.Fatalf("event rows: %+v", rows)
	}
	if n := srcEvCommentCount(t, s, x.ID); n != 1 {
		t.Fatalf("system comments=%d, want 1", n)
	}
	// The same (task, source, source_ref) again changes nothing.
	again, err := srcEvApplyTx(s, t, after, srcEvPREvent(pr))
	if err != nil || again.Applied || !again.Redelivered || again.State != StateReviewing {
		t.Fatalf("redelivery: res=%+v err=%v", again, err)
	}
	if got := srcEvGet(t, s, x.ID); len(srcEvEventsOf(got, SourceMergewakePR)) != 1 || srcEvCommentCount(t, s, x.ID) != 1 {
		t.Fatalf("a redelivery wrote rows: events %+v", got.Events)
	}
}

func srcEvCaseNotApplied(t *testing.T, s *Store) {
	x := srcEvTask(t, s, "not applied")
	first := srcEvPR(srcEvUnique())
	if res, err := srcEvApplyTx(s, t, x, srcEvPREvent(first)); err != nil || !res.Applied {
		t.Fatalf("first pr event: res=%+v err=%v", res, err)
	}
	reviewing := srcEvGet(t, s, x.ID)
	// A second PR event cannot move a task that is already reviewing: it is
	// recorded with applied=false and the reason, and the state stays.
	second := srcEvPR(srcEvUnique())
	res, err := srcEvApplyTx(s, t, reviewing, srcEvPREvent(second))
	if err != nil || res.Applied || res.Redelivered || res.Reason == "" || res.State != StateReviewing {
		t.Fatalf("second pr event: res=%+v err=%v", res, err)
	}
	after := srcEvGet(t, s, x.ID)
	var recorded *TaskEvent
	for i := range after.Events {
		if after.Events[i].SourceRef == second {
			recorded = &after.Events[i]
		}
	}
	if after.State != StateReviewing || recorded == nil || recorded.Applied || recorded.Kind != TaskEventSource || recorded.From != StateReviewing || recorded.To != StateReviewing {
		t.Fatalf("not-applied row: state=%s row=%+v", after.State, recorded)
	}
	// The recorded row is that event's turn: redelivering it answers again.
	if res, err = srcEvApplyTx(s, t, after, srcEvPREvent(second)); err != nil || !res.Redelivered {
		t.Fatalf("redelivery of a not-applied event: res=%+v err=%v", res, err)
	}
}

func srcEvCaseMerge(t *testing.T, s *Store) {
	x := srcEvTask(t, s, "merge")
	pr := srcEvPR(srcEvUnique())
	ev := sourceEvent{Source: SourceMergewakeMerge, SourceRef: pr, PR: pr, MergeSHA: "0123456789abcdef", Named: map[int64]bool{}}
	res, err := srcEvApplyTx(s, t, x, ev)
	if err != nil || !res.Applied || res.State != StateMerged {
		t.Fatalf("merge: res=%+v err=%v", res, err)
	}
	if after := srcEvGet(t, s, x.ID); after.State != StateMerged || after.Refs.PR != pr {
		t.Fatalf("after merge: state=%s refs.pr=%q", after.State, after.Refs.PR)
	}
}

func srcEvCaseFollowupSubtasks(t *testing.T, s *Store) {
	ctx := t.Context()
	openAt := time.Now().UTC().Add(-time.Minute)

	// A child filed after the parent's PR opened is a follow-up. The merge
	// applies and its deploy record is applied in the same turn.
	parent := srcEvTask(t, s, "follow-up parent deploy")
	parentPR := srcEvPR(srcEvUnique())
	if res, err := srcEvApplyPublicAt(s, t, parent.ID, SourceMergewakePR, parentPR, openAt); err != nil || !res.Applied {
		t.Fatalf("open parent PR: res=%+v err=%v", res, err)
	}
	followup := srcEvTask(t, s, "PR follow-ups")
	if _, _, err := s.SetTaskParent(ctx, followup.ID, &parent.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	putDeploy(t, s, srcEvDeployKey("followup"), "followup", "success", []string{parentPR})
	if res, err := srcEvApplyPublicAt(s, t, parent.ID, SourceMergewakeMerge, parentPR, time.Now().UTC()); err != nil || !res.Applied || res.State != StateDeployed {
		t.Fatalf("merge with follow-up: res=%+v err=%v, want deployed", res, err)
	}

	// The same later child cannot move an already merged parent when it is
	// eventually deployed.
	mergedParent := srcEvTask(t, s, "merged parent with follow-up")
	mergedPR := srcEvPR(srcEvUnique())
	if _, err := srcEvApplyPublicAt(s, t, mergedParent.ID, SourceMergewakePR, mergedPR, openAt); err != nil {
		t.Fatal(err)
	}
	lateChild := srcEvTask(t, s, "late follow-up")
	if _, _, err := s.SetTaskParent(ctx, lateChild.ID, &mergedParent.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	if res, err := srcEvApplyPublicAt(s, t, mergedParent.ID, SourceMergewakeMerge, mergedPR, time.Now().UTC()); err != nil || !res.Applied || res.State != StateMerged {
		t.Fatalf("merge with late child: res=%+v err=%v", res, err)
	}
	childPR := srcEvPR(srcEvUnique())
	if _, err := srcEvApplyPublicAt(s, t, lateChild.ID, SourceMergewakeMerge, childPR, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	putDeploy(t, s, srcEvDeployKey("child"), "child", "success", []string{childPR})
	if _, err := s.SettleMerged(ctx, "", false); err != nil {
		t.Fatal(err)
	}
	if got := srcEvGet(t, s, lateChild.ID); got.State != StateDeployed {
		t.Fatalf("follow-up child state=%s, want deployed before checking its parent", got.State)
	}
	if got := srcEvGet(t, s, mergedParent.ID); got.State != StateMerged {
		t.Fatalf("follow-up deployment moved parent to %s, want merged", got.State)
	}

	// A child filed before the PR opened still holds the merge. Settling that
	// piece preserves hk 3294/4172: the parent adopts the held PR.
	pieceParent := srcEvTask(t, s, "planned piece parent")
	piece := srcEvTask(t, s, "planned piece")
	if _, _, err := s.SetTaskParent(ctx, piece.ID, &pieceParent.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	piecePR := srcEvPR(srcEvUnique())
	if _, err := srcEvApplyPublicAt(s, t, pieceParent.ID, SourceMergewakePR, piecePR, time.Now().UTC().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if res, err := srcEvApplyPublicAt(s, t, pieceParent.ID, SourceMergewakeMerge, piecePR, time.Now().UTC().Add(2*time.Minute)); err != nil || res.Applied || !strings.Contains(res.Reason, "sub-tasks") {
		t.Fatalf("planned piece merge: res=%+v err=%v, want held", res, err)
	}
	pieceMergePR := srcEvPR(srcEvUnique())
	if res, err := srcEvApplyPublicAt(s, t, piece.ID, SourceMergewakeMerge, pieceMergePR, time.Now().UTC()); err != nil || !res.Applied {
		t.Fatalf("settle planned piece: res=%+v err=%v", res, err)
	}
	if got := srcEvGet(t, s, pieceParent.ID); got.State != StateMerged || got.Refs.PR != piecePR {
		t.Fatalf("planned piece parent state=%s pr=%q, want merged adopting %s", got.State, got.Refs.PR, piecePR)
	}

	// If another implements PR becomes the parent's current ref after a held
	// merge, settling the children must still check that unrelated open PR.
	heldParent := srcEvTask(t, s, "held PR before another open PR")
	heldChild := srcEvTask(t, s, "piece holding PR A")
	if _, _, err := s.SetTaskParent(ctx, heldChild.ID, &heldParent.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	heldPR, otherOpenPR := srcEvPR(srcEvUnique()), srcEvPR(srcEvUnique())
	srcEvExecTx(t, s, s.q(`UPDATE tasks SET created_at=$1 WHERE id=$2`, `UPDATE tasks SET created_at=? WHERE id=?`), time.Now().UTC().Add(-3*time.Minute), heldChild.ID)
	if _, err := srcEvApplyPublicAt(s, t, heldParent.ID, SourceMergewakePR, heldPR, time.Now().UTC().Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if res, err := srcEvApplyPublicAt(s, t, heldParent.ID, SourceMergewakeMerge, heldPR, time.Now().UTC().Add(-time.Minute)); err != nil || res.Applied {
		t.Fatalf("merge PR A while its piece is open: res=%+v err=%v, want held", res, err)
	}
	if res, err := srcEvApplyPublicAt(s, t, heldParent.ID, SourceMergewakePR, otherOpenPR, time.Now().UTC()); err != nil || res.Applied {
		t.Fatalf("record another open PR B: res=%+v err=%v, want recorded without moving", res, err)
	}
	srcEvExecTx(t, s, s.q(`UPDATE tasks SET refs=refs||jsonb_build_object('pr',$1::text) WHERE id=$2`, `UPDATE tasks SET refs=JSON_SET(refs,'$.pr',?) WHERE id=?`), otherOpenPR, heldParent.ID)
	heldChildPR := srcEvPR(srcEvUnique())
	if res, err := srcEvApplyPublicAt(s, t, heldChild.ID, SourceMergewakeMerge, heldChildPR, time.Now().UTC()); err != nil || !res.Applied {
		t.Fatalf("settle the child: res=%+v err=%v", res, err)
	}
	if got := srcEvGet(t, s, heldParent.ID); got.State != StateReviewing || got.Refs.PR != otherOpenPR {
		t.Fatalf("parent after child close: state=%s refs.pr=%q, want reviewing with open PR B %s", got.State, got.Refs.PR, otherOpenPR)
	}
	drift, err := s.TaskDrift(ctx, time.Now().UTC(), DriftDefaultMergedDays)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := driftEntry(drift.AutoCloseHeld, heldParent.ID); !ok || row.Mechanical || len(row.OpenPRs) != 1 || row.OpenPRs[0] != otherOpenPR {
		t.Fatalf("held-parent drift=%+v listed=%t, want open PR B to hold the parent", row, ok)
	}
	hygiene, err := s.TaskHygiene(ctx, time.Now().UTC(), "", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, close := range hygiene.MechanicalCloses {
		if close.TaskID == heldParent.ID {
			t.Fatalf("hygiene preview would close parent with open PR B: %+v", close)
		}
	}
	foundHeldParent := false
	for _, finding := range hygiene.Findings.AutoCloseHeldParents {
		if finding.ID == heldParent.ID && !finding.Mechanical && len(finding.OpenPRs) == 1 && finding.OpenPRs[0] == otherOpenPR {
			foundHeldParent = true
		}
	}
	if !foundHeldParent {
		t.Fatalf("hygiene did not report open PR B as parent hold: %+v", hygiene.Findings.AutoCloseHeldParents)
	}
	otherRepo, otherNumber, ok := parsePRURL(otherOpenPR)
	if !ok {
		t.Fatalf("parse open PR B %q", otherOpenPR)
	}
	srcEvExecTx(t, s, s.q(`UPDATE changes SET state='closed' WHERE repo=$1 AND pr_number=$2`, `UPDATE changes SET state='closed' WHERE repo=? AND pr_number=?`), otherRepo, otherNumber)
	hygiene, err = s.TaskHygiene(ctx, time.Now().UTC(), "", true)
	if err != nil {
		t.Fatal(err)
	}
	foundHeldClose := false
	for _, close := range hygiene.MechanicalCloses {
		if close.TaskID == heldParent.ID && close.ToState == StateMerged && close.PR == heldPR {
			foundHeldClose = true
		}
	}
	if !foundHeldClose {
		t.Fatalf("after closing PR B, hygiene preview did not select held PR A: %+v", hygiene.MechanicalCloses)
	}
	hygiene, err = s.TaskHygiene(ctx, time.Now().UTC(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := srcEvGet(t, s, heldParent.ID); got.State != StateMerged || got.Refs.PR != heldPR {
		t.Fatalf("parent after hygiene retry: state=%s refs.pr=%q, want merged with held PR A %s", got.State, got.Refs.PR, heldPR)
	}

	// The old held shape becomes mechanical when the child's timestamp proves
	// it was filed after the merged PR opened. The planned-piece counterpart
	// remains held in both drift and hygiene.
	legacy := srcEvTask(t, s, "legacy held follow-up")
	legacyChild := srcEvTask(t, s, "legacy late child")
	if _, _, err := s.SetTaskParent(ctx, legacyChild.ID, &legacy.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	legacyPR := srcEvPR(srcEvUnique())
	legacyOpened := time.Now().UTC().Add(time.Minute)
	if _, err := srcEvApplyPublicAt(s, t, legacy.ID, SourceMergewakePR, legacyPR, legacyOpened); err != nil {
		t.Fatal(err)
	}
	if res, err := srcEvApplyPublicAt(s, t, legacy.ID, SourceMergewakeMerge, legacyPR, legacyOpened.Add(time.Minute)); err != nil || res.Applied {
		t.Fatalf("legacy merge: res=%+v err=%v, want held before timestamp correction", res, err)
	}
	srcEvExecTx(t, s, s.q(`UPDATE tasks SET created_at=$1 WHERE id=$2`, `UPDATE tasks SET created_at=? WHERE id=?`), legacyOpened.Add(time.Second), legacyChild.ID)
	drift, err = s.TaskDrift(ctx, time.Now().UTC(), DriftDefaultMergedDays)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := driftEntry(drift.MergedPRNotMerged, legacy.ID); !ok || !row.Mechanical || len(row.OpenSubTasks) != 0 {
		t.Fatalf("legacy follow-up drift=%+v listed=%t, want mechanical without open_sub_tasks", row, ok)
	}
	planned := srcEvTask(t, s, "planned piece drift")
	plannedChild := srcEvTask(t, s, "open planned piece")
	if _, _, err := s.SetTaskParent(ctx, plannedChild.ID, &planned.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	plannedPR := srcEvPR(srcEvUnique())
	plannedOpened := time.Now().UTC().Add(time.Minute)
	if _, err := srcEvApplyPublicAt(s, t, planned.ID, SourceMergewakePR, plannedPR, plannedOpened); err != nil {
		t.Fatal(err)
	}
	if _, err := srcEvApplyPublicAt(s, t, planned.ID, SourceMergewakeMerge, plannedPR, plannedOpened.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	drift, err = s.TaskDrift(ctx, plannedOpened.Add(2*time.Minute), DriftDefaultMergedDays)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := driftEntry(drift.MergedPRNotMerged, planned.ID); !ok || row.Mechanical || len(row.OpenSubTasks) != 1 {
		t.Fatalf("planned piece drift=%+v listed=%t, want held by its open child", row, ok)
	}
	hygiene, err = s.TaskHygiene(ctx, time.Now().UTC(), "", true)
	if err != nil {
		t.Fatal(err)
	}
	foundClose := false
	foundPlannedClose := false
	for _, close := range hygiene.MechanicalCloses {
		if close.TaskID == legacy.ID && close.Rule == "merged_pr" {
			foundClose = true
		}
		if close.TaskID == planned.ID {
			foundPlannedClose = true
		}
	}
	if !foundClose {
		t.Fatalf("hygiene mechanical closes do not include legacy follow-up task %d: %+v", legacy.ID, hygiene.MechanicalCloses)
	}
	if foundPlannedClose {
		t.Fatalf("hygiene listed a planned piece as mechanical: %+v", hygiene.MechanicalCloses)
	}
	foundPlannedFinding := false
	for _, finding := range hygiene.Findings.MergedPROpenTasks {
		if finding.ID == planned.ID && !finding.Mechanical && len(finding.OpenSubTasks) == 1 {
			foundPlannedFinding = true
		}
	}
	if !foundPlannedFinding {
		t.Fatalf("hygiene did not report the planned-piece hold: %+v", hygiene.Findings.MergedPROpenTasks)
	}

	// With multiple implements PRs, holds are evaluated against the PR hygiene
	// will close. The child is after A but before B: it is a follow-up to A and
	// a planned piece for B, the newest merged change hygiene selects.
	multi := srcEvTask(t, s, "two PR parent")
	prA, prB := srcEvPR(srcEvUnique()), srcEvPR(srcEvUnique())
	if _, err := srcEvApplyPublicAt(s, t, multi.ID, SourceMergewakePR, prA, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	multiChild := srcEvTask(t, s, "piece for second PR")
	if _, _, err := s.SetTaskParent(ctx, multiChild.ID, &multi.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	if _, err := srcEvApplyPublicAt(s, t, multi.ID, SourceMergewakePR, prB, time.Now().UTC().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if res, err := srcEvApplyPublicAt(s, t, multi.ID, SourceMergewakeMerge, prA, time.Now().UTC().Add(2*time.Minute)); err != nil || res.Applied {
		t.Fatalf("merge PR A while B is open: res=%+v err=%v, want held", res, err)
	}
	if res, err := srcEvApplyPublicAt(s, t, multi.ID, SourceMergewakeMerge, prB, time.Now().UTC().Add(3*time.Minute)); err != nil || res.Applied || !strings.Contains(res.Reason, "sub-tasks") {
		t.Fatalf("merge PR B with its planned piece open: res=%+v err=%v, want held", res, err)
	}
	multiAsOf := time.Now().UTC().Add(4 * time.Minute)
	drift, err = s.TaskDrift(ctx, multiAsOf, DriftDefaultMergedDays)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := driftEntry(drift.MergedPRNotMerged, multi.ID); !ok || row.Mechanical || len(row.OpenSubTasks) != 1 {
		t.Fatalf("two-PR drift=%+v listed=%t, want held by child relative to PR B", row, ok)
	}
	hygiene, err = s.TaskHygiene(ctx, multiAsOf, "", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, close := range hygiene.MechanicalCloses {
		if close.TaskID == multi.ID {
			t.Fatalf("hygiene preview would close two-PR task despite its planned child: %+v", close)
		}
	}
	foundMultiFinding := false
	for _, finding := range hygiene.Findings.MergedPROpenTasks {
		if finding.ID == multi.ID && !finding.Mechanical && len(finding.OpenSubTasks) == 1 {
			foundMultiFinding = true
		}
	}
	if !foundMultiFinding {
		t.Fatalf("hygiene did not report the planned child relative to PR B: %+v", hygiene.Findings.MergedPROpenTasks)
	}
	hygiene, err = s.TaskHygiene(ctx, multiAsOf, "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, close := range hygiene.MechanicalCloses {
		if close.TaskID == multi.ID {
			t.Fatalf("hygiene execution closed two-PR task despite its planned child: %+v", close)
		}
	}
	if got := srcEvGet(t, s, multi.ID); got.State != StateReviewing {
		t.Fatalf("hygiene execution moved two-PR task to %s, want reviewing", got.State)
	}
}

func srcEvApplyPublicAt(s *Store, t *testing.T, taskID int64, source, pr string, at time.Time) (TaskEventResult, error) {
	t.Helper()
	return s.ApplyTaskSourceEvent(t.Context(), TaskSourceEvent{Source: source, SourceRef: pr, PR: pr, TaskID: taskID, At: &at})
}

func srcEvExecTx(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	tx, release, err := s.begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func srcEvDeployKey(service string) string {
	stamp := time.Now().UTC().Add(time.Duration(srcEvUnique()%100000) * time.Second)
	return fmt.Sprintf("deploy/%s/%s", service, stamp.Format("20060102T150405Z"))
}

func srcEvCaseConcurrent(t *testing.T, s *Store) {
	x := srcEvTask(t, s, "concurrent")
	pr := srcEvPR(srcEvUnique())
	const workers = 4
	results := make([]TaskEventResult, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A MySQL deadlock or lock timeout is retried once, as the public
			// mutating methods do.
			errs[i] = s.retryTx(t.Context(), func() error {
				var err error
				results[i], err = srcEvApplyTx(s, t, x, srcEvPREvent(pr))
				return err
			})
		}()
	}
	wg.Wait()
	applied, redelivered := 0, 0
	for i := range workers {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if results[i].Applied {
			applied++
		}
		if results[i].Redelivered {
			redelivered++
		}
	}
	if applied != 1 || redelivered != workers-1 {
		t.Fatalf("applied=%d redelivered=%d of %d, want 1 and %d", applied, redelivered, workers, workers-1)
	}
	after := srcEvGet(t, s, x.ID)
	if after.State != StateReviewing || len(srcEvEventsOf(after, SourceMergewakePR)) != 1 || srcEvCommentCount(t, s, x.ID) != 1 {
		t.Fatalf("after %d concurrent events: state=%s events=%+v", workers, after.State, after.Events)
	}
}

// A deploy record that lists a PR no merged task delivers locks and reads
// the merged tasks, finds none and writes nothing.
func srcEvCaseDeployRecordNoTask(t *testing.T, s *Store) {
	ctx := t.Context()
	x := srcEvTask(t, s, "deploy record")
	before := srcEvGet(t, s, x.ID)
	pr := srcEvPR(srcEvUnique())
	body := fmt.Sprintf(`{"schema":"deploy-record/v0","service":"svc","target":"test","result":"success","included_prs":[%q],"source":"test"}`, pr)
	record := parseStoredDeployRecord("deploy/svc/20261105T000000Z", "", body, time.Now().UTC())
	tx, release, err := s.begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	defer tx.Rollback(ctx)
	if err = s.applyDeployRecordTx(ctx, tx, record, time.Now().UTC()); err != nil {
		t.Fatalf("applyDeployRecordTx: %v", err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if after := srcEvGet(t, s, x.ID); after.State != before.State || len(after.Events) != len(before.Events) || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("the record changed an unrelated task: %+v", after)
	}
}
