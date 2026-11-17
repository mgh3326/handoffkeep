package store

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Dialect-neutral behaviour cases for the task write path (hk 5074, B2): the
// state machine, insertEvent's (source, source_ref) idempotence and activity
// clock, writeTaskTx, and the lifecycle and priority writes. They run on
// PostgreSQL in TestTasksAreaPostgres and on MySQL in TestMySQLTasksArea.
//
// A step that reaches an area the MySQL port has not covered yet (a settled
// state's decision mirror, a relane, a change link) answers
// ErrDialectUnsupported there; flowSkipUnported turns exactly that answer into
// a skip on MySQL, so the case runs in full once the area is ported. A method
// of this file's own path never goes through it: flowMust fails the case.

func init() {
	registerTaskAreaCase("flow/transitions", flowCaseTransitions)
	registerTaskAreaCase("flow/event-insert", flowCaseEventInsert)
	registerTaskAreaCase("flow/write-task", flowCaseWriteTask)
	registerTaskAreaCase("flow/hold-release", flowCaseHoldRelease)
	registerTaskAreaCase("flow/hold-replay-concurrent", flowCaseHoldReplayConcurrent)
	registerTaskAreaCase("flow/priority", flowCasePriority)
	registerTaskAreaCase("flow/claim", flowCaseClaim)
	registerTaskAreaCase("flow/settled-transitions", flowCaseSettled)
	registerTaskAreaCase("flow/awaiting-decision", flowCaseAwaiting)
	registerTaskAreaCase("flow/relane-and-lanes", flowCaseRelane)
}

const flowCaseProject = "flow-case"

var flowCaseSeq atomic.Int64

// flowSkipUnported skips a MySQL case at a step that needs another area; any
// other error fails it. On PostgreSQL ErrDialectUnsupported never occurs.
func flowSkipUnported(t *testing.T, s *Store, err error) {
	t.Helper()
	if err == nil {
		return
	}
	if s.dialect == mysqlDialect && errors.Is(err, ErrDialectUnsupported) {
		t.Skipf("MySQL: %v", err)
	}
	t.Fatal(err)
}

func flowMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func flowCaseName(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), flowCaseSeq.Add(1))
}

// flowCaseTask files a task in backlog through CreateTask.
func flowCaseTask(t *testing.T, s *Store, kind string) Task {
	t.Helper()
	ctx := t.Context()
	_, err := s.AddTaskProject(ctx, flowCaseProject, "flow-case")
	flowSkipUnported(t, s, err)
	project := flowCaseProject
	x, err := s.CreateTask(ctx, Task{Lane: flowCaseName("lane"), Title: flowCaseName("task"), Kind: kind, Priority: 5, CreatedBy: "flow-case", Project: &project})
	flowSkipUnported(t, s, err)
	if x.State != StateBacklog {
		t.Fatalf("new task state=%q, want backlog", x.State)
	}
	return x
}

func flowCaseGet(t *testing.T, s *Store, id int64) Task {
	t.Helper()
	x, found, err := s.GetTask(t.Context(), id)
	flowSkipUnported(t, s, err)
	if !found {
		t.Fatalf("task %d not found", id)
	}
	return x
}

// flowCaseEvents returns the events of a task with the given source and ref.
func flowCaseEvents(x Task, source, ref string) []TaskEvent {
	var out []TaskEvent
	for _, e := range x.Events {
		if e.Source == source && e.SourceRef == ref {
			out = append(out, e)
		}
	}
	return out
}

func flowCaseTransitions(t *testing.T, s *Store) {
	ctx := t.Context()
	x := flowCaseTask(t, s, "implement")

	got, err := s.TransitionTask(ctx, x.ID, StateUnstarted, "tester", "ready", nil, "")
	flowMust(t, err)
	if got.State != StateUnstarted {
		t.Fatalf("state=%q, want unstarted", got.State)
	}

	// A refused edge changes nothing.
	if _, err = s.TransitionTask(ctx, x.ID, StateMerged, "tester", "skip ahead", nil, ""); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("unstarted -> merged: err=%v, want ErrTaskConflict", err)
	}
	// Entering a started state needs a job link or a recorded no-job reason.
	if _, err = s.TransitionTask(ctx, x.ID, StateDeveloping, "tester", "go", nil, ""); !errors.Is(err, ErrTaskJobRequired) {
		t.Fatalf("unlinked start: err=%v, want ErrTaskJobRequired", err)
	}
	got, err = s.TransitionTask(ctx, x.ID, StateDeveloping, "tester", "go", nil, "started by hand in the flow case")
	flowMust(t, err)
	if got.State != StateDeveloping || got.StartedAt == nil {
		t.Fatalf("after start: state=%q started_at=%v", got.State, got.StartedAt)
	}
	// An old name resolves to the state it maps to; a task already there is a no-op.
	got, err = s.TransitionTask(ctx, x.ID, "verifying", "tester", "review", nil, "")
	flowMust(t, err)
	if got.State != StateReviewing {
		t.Fatalf("verifying -> %q, want reviewing", got.State)
	}
	// Moves inside started are unguarded and the lifecycle keeps started_at.
	back, err := s.TransitionTaskWith(ctx, x.ID, TransitionInput{To: StateDeveloping, By: "tester", Note: "rework"})
	flowMust(t, err)
	if back.StartedAt == nil || !back.StartedAt.Equal(*got.StartedAt) {
		t.Fatalf("started_at moved: %v -> %v", got.StartedAt, back.StartedAt)
	}

	// Refusals before any write.
	if _, err = s.TransitionTask(ctx, 1<<40, StateUnstarted, "tester", "x", nil, ""); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing task: err=%v, want ErrTaskNotFound", err)
	}
	if _, err = s.TransitionTask(ctx, x.ID, "no-such-state", "tester", "x", nil, ""); err == nil {
		t.Fatal("an unknown state was accepted")
	}
	if _, err = s.TransitionTask(ctx, x.ID, StateUnstarted, "", "x", nil, ""); err == nil {
		t.Fatal("a transition without an actor was accepted")
	}

	full := flowCaseGet(t, s, x.ID)
	if full.State != StateDeveloping {
		t.Fatalf("state after refusals=%q, want developing", full.State)
	}
	var moves []string
	var noJob string
	for _, e := range full.Events {
		if e.Kind == TaskEventTransition {
			moves = append(moves, e.From+">"+e.To)
			if e.To == StateDeveloping && e.NoJob != "" {
				noJob = e.NoJob
			}
		}
	}
	want := []string{"backlog>unstarted", "unstarted>developing", "developing>reviewing", "reviewing>developing"}
	if fmt.Sprint(moves) != fmt.Sprint(want) {
		t.Fatalf("transition events=%v, want %v", moves, want)
	}
	if noJob != "started by hand in the flow case" {
		t.Fatalf("no_job on the start event=%q", noJob)
	}
}

// flowCaseEventInsert drives insertEvent directly: the (source, source_ref)
// key makes a redelivery a no-op that reports no activity, an event without
// both is always inserted, and the activity clock only moves forward and only
// for a human or job event.
func flowCaseEventInsert(t *testing.T, s *Store) {
	ctx := t.Context()
	x := flowCaseTask(t, s, "implement")
	other := flowCaseTask(t, s, "implement")
	at := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)

	tx, release, err := s.begin(ctx)
	flowMust(t, err)
	defer release()
	defer tx.Rollback(ctx)
	ev := func(task Task, source, ref string, when time.Time, into *Task) taskEventInput {
		return taskEventInput{TaskID: task.ID, Kind: "metadata", From: task.State, To: task.State, By: "flow-case", At: when, Source: source, SourceRef: ref, Field: "f", FromValue: "a", ToValue: "b", Into: into}
	}

	var into Task
	first, moved, err := insertEvent(ctx, tx, ev(x, "flow-case", "r1", at, &into))
	flowMust(t, err)
	if first == 0 || !moved || !into.LastActivityAt.Equal(at) {
		t.Fatalf("first insert: id=%d moved=%t into=%v", first, moved, into.LastActivityAt)
	}
	// The same (task, source, ref) again: no row, no clock move, no error.
	dup, moved, err := insertEvent(ctx, tx, ev(x, "flow-case", "r1", at.Add(time.Minute), nil))
	flowMust(t, err)
	if dup != 0 || moved {
		t.Fatalf("redelivered event: id=%d moved=%t, want 0 false", dup, moved)
	}
	// Another task, or another ref, is its own event.
	for _, c := range []struct {
		task Task
		ref  string
	}{{other, "r1"}, {x, "r2"}} {
		id, _, err := insertEvent(ctx, tx, ev(c.task, "flow-case", c.ref, at, nil))
		flowMust(t, err)
		if id == 0 {
			t.Fatalf("event (%d, %s) was treated as a duplicate", c.task.ID, c.ref)
		}
	}
	// No source or no ref: no key, so two identical events both land.
	a, _, err := insertEvent(ctx, tx, ev(x, "", "", at, nil))
	flowMust(t, err)
	b, _, err := insertEvent(ctx, tx, ev(x, "", "", at, nil))
	flowMust(t, err)
	c, _, err := insertEvent(ctx, tx, ev(x, "flow-case", "", at, nil))
	flowMust(t, err)
	d, _, err := insertEvent(ctx, tx, ev(x, "flow-case", "", at, nil))
	flowMust(t, err)
	if a == 0 || b == 0 || c == 0 || d == 0 || a == b || c == d {
		t.Fatalf("keyless events: %d %d %d %d", a, b, c, d)
	}
	// The clock never moves back.
	id, moved, err := insertEvent(ctx, tx, ev(x, "flow-case", "r3", at.Add(-time.Minute), nil))
	flowMust(t, err)
	if id == 0 || moved {
		t.Fatalf("older event: id=%d moved=%t, want an insert that does not move the clock", id, moved)
	}
	// An automatic event of another producer does not count as activity.
	auto := ev(x, "hk.flow-case", "r4", at.Add(time.Hour), nil)
	auto.Auto = true
	id, moved, err = insertEvent(ctx, tx, auto)
	flowMust(t, err)
	if id == 0 || moved {
		t.Fatalf("automatic event: id=%d moved=%t", id, moved)
	}
	flowMust(t, tx.Commit(ctx))

	got := flowCaseGet(t, s, x.ID)
	if !got.LastActivityAt.Equal(at) {
		t.Fatalf("last_activity_at=%v, want %v", got.LastActivityAt, at)
	}
	if n := len(flowCaseEvents(got, "flow-case", "r1")); n != 1 {
		t.Fatalf("%d events for (flow-case, r1), want 1", n)
	}
}

// flowCaseWriteTask drives writeTaskTx: it stores the mutable fields and
// returns the row as written; a missing row is ErrNoRows.
func flowCaseWriteTask(t *testing.T, s *Store) {
	ctx := t.Context()
	x := flowCaseTask(t, s, "implement")
	tx, release, err := s.begin(ctx)
	flowMust(t, err)
	defer release()
	defer tx.Rollback(ctx)

	now := time.Now().UTC().Truncate(time.Microsecond)
	x.ClaimedBy, x.UpdatedAt = "flow-writer", now
	x.Refs.JobID = "flow-job"
	x.StartedAt = &now
	out, err := writeTaskTx(ctx, tx, x)
	flowMust(t, err)
	if out.ID != x.ID || out.ClaimedBy != "flow-writer" || out.Refs.JobID != "flow-job" || out.StartedAt == nil || !out.StartedAt.Equal(now) || !out.UpdatedAt.Equal(now) {
		t.Fatalf("written task=%+v", out)
	}
	if out.Title != x.Title || out.Lane != x.Lane {
		t.Fatalf("writeTaskTx changed immutable fields: %q/%q", out.Title, out.Lane)
	}
	if _, err = writeTaskTx(ctx, tx, Task{ID: 1 << 40, State: StateBacklog, UpdatedAt: now}); !errors.Is(err, ErrNoRows) {
		t.Fatalf("missing row: err=%v, want ErrNoRows", err)
	}
	flowMust(t, tx.Commit(ctx))

	got := flowCaseGet(t, s, x.ID)
	if got.ClaimedBy != "flow-writer" || got.Refs.JobID != "flow-job" {
		t.Fatalf("stored task claimed_by=%q job=%q", got.ClaimedBy, got.Refs.JobID)
	}
}

func flowCaseHoldRelease(t *testing.T, s *Store) {
	ctx := t.Context()
	x := flowCaseTask(t, s, "implement")

	r, err := s.HoldTask(ctx, x.ID, true, "", "tester", "park it", "hold-1")
	flowMust(t, err)
	if r.Previous != StateBacklog || r.State != StateIcebox {
		t.Fatalf("hold=%+v", r)
	}
	// The same key replays the recorded result, and holds nothing twice.
	again, err := s.HoldTask(ctx, x.ID, true, "", "tester", "park it", "hold-1")
	flowMust(t, err)
	if again != r {
		t.Fatalf("replayed hold=%+v, want %+v", again, r)
	}
	// A new key on a held task is a conflict; a bad request is refused.
	if _, err = s.HoldTask(ctx, x.ID, true, "", "tester", "park it", "hold-2"); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("second hold: err=%v, want ErrTaskConflict", err)
	}
	if _, err = s.HoldTask(ctx, x.ID, true, "", "tester", "park it", ""); err == nil {
		t.Fatal("a hold without a key was accepted")
	}
	if _, err = s.HoldTask(ctx, 1<<40, true, "", "tester", "park it", "hold-3"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing task: err=%v, want ErrTaskNotFound", err)
	}

	r, err = s.HoldTask(ctx, x.ID, false, StateUnstarted, "tester", "back", "release-1")
	flowMust(t, err)
	if r.Previous != StateIcebox || r.State != StateUnstarted {
		t.Fatalf("release=%+v", r)
	}
	if _, err = s.HoldTask(ctx, x.ID, false, "", "tester", "again", "release-2"); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("release of a task that is not held: err=%v, want ErrTaskConflict", err)
	}
	got := flowCaseGet(t, s, x.ID)
	if got.State != StateUnstarted {
		t.Fatalf("state=%q, want unstarted", got.State)
	}
	if n := len(flowCaseEvents(got, lifecycleSourceHold, "hold-1")); n != 1 {
		t.Fatalf("%d events for hold-1, want 1", n)
	}
}

// flowCaseHoldReplayConcurrent sends one idempotent write from several
// callers at once: the task-mutation lock lets one perform it and the rest
// replay it, so all see the same result and the task records one event.
func flowCaseHoldReplayConcurrent(t *testing.T, s *Store) {
	ctx := t.Context()
	x := flowCaseTask(t, s, "implement")
	const callers = 6
	results := make([]HoldResult, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = s.HoldTask(ctx, x.ID, true, "", "tester", "park it", "hold-concurrent")
		}()
	}
	wg.Wait()
	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if results[i] != (HoldResult{Previous: StateBacklog, State: StateIcebox}) {
			t.Fatalf("caller %d: %+v", i, results[i])
		}
	}
	got := flowCaseGet(t, s, x.ID)
	if n := len(flowCaseEvents(got, lifecycleSourceHold, "hold-concurrent")); n != 1 {
		t.Fatalf("%d events for the key, want 1", n)
	}
}

func flowCasePriority(t *testing.T, s *Store) {
	ctx := t.Context()
	x := flowCaseTask(t, s, "implement")

	r, err := s.SetTaskPriority(ctx, x.ID, 9, "tester", "raise", "prio-1")
	flowMust(t, err)
	if r.Previous != 5 || r.Priority != 9 || r.Replayed {
		t.Fatalf("result=%+v", r)
	}
	// A replay returns the recorded values, not the new request.
	r, err = s.SetTaskPriority(ctx, x.ID, 3, "tester", "lower", "prio-1")
	flowMust(t, err)
	if r.Previous != 5 || r.Priority != 9 || !r.Replayed {
		t.Fatalf("replay=%+v", r)
	}
	if _, err = s.SetTaskPriority(ctx, x.ID, -1, "tester", "bad", "prio-2"); !errors.Is(err, ErrInvalidTaskPriority) {
		t.Fatalf("negative priority: err=%v", err)
	}
	if _, err = s.SetTaskPriority(ctx, 1<<40, 1, "tester", "missing", "prio-3"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing task: err=%v, want ErrTaskNotFound", err)
	}

	got, changed, err := s.SetTaskPriorityDirect(ctx, x.ID, 4, "tester", "direct")
	flowMust(t, err)
	if !changed || got.Priority != 4 {
		t.Fatalf("direct change: changed=%t priority=%d", changed, got.Priority)
	}
	got, changed, err = s.SetTaskPriorityDirect(ctx, x.ID, 4, "tester", "direct")
	flowMust(t, err)
	if changed || got.Priority != 4 {
		t.Fatalf("direct no-op: changed=%t priority=%d", changed, got.Priority)
	}
	if _, _, err = s.SetTaskPriorityDirect(ctx, 1<<40, 4, "tester", "direct"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("direct on a missing task: err=%v", err)
	}
	if _, _, err = s.SetTaskPriorityDirect(ctx, x.ID, -2, "tester", "direct"); !errors.Is(err, ErrInvalidTaskPriority) {
		t.Fatalf("direct negative: err=%v", err)
	}

	full := flowCaseGet(t, s, x.ID)
	if full.Priority != 4 {
		t.Fatalf("stored priority=%d, want 4", full.Priority)
	}
	if n := len(flowCaseEvents(full, "ui-priority", "prio-1")); n != 1 {
		t.Fatalf("%d priority events for prio-1, want 1", n)
	}
}

func flowCaseClaim(t *testing.T, s *Store) {
	ctx := t.Context()
	x := flowCaseTask(t, s, "implement")

	got, err := s.ClaimTask(ctx, x.ID, "worker-a", "job-flow-1", "")
	flowSkipUnported(t, s, err)
	if got.State != StateDeveloping || got.ClaimedBy != "worker-a" || got.Refs.JobID != "job-flow-1" {
		t.Fatalf("claim=%+v", got)
	}
	before := len(flowCaseGet(t, s, x.ID).Events)
	// The same claimant and job replays without a second event.
	again, err := s.ClaimTask(ctx, x.ID, "worker-a", "job-flow-1", "")
	flowMust(t, err)
	if again.State != StateDeveloping || len(flowCaseGet(t, s, x.ID).Events) != before {
		t.Fatalf("replayed claim state=%q events=%d, want %d", again.State, len(flowCaseGet(t, s, x.ID).Events), before)
	}
	if _, err = s.ClaimTask(ctx, x.ID, "worker-b", "job-flow-2", ""); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("second claimant: err=%v, want ErrTaskConflict", err)
	}
	// A claim names a job or a no-job reason.
	y := flowCaseTask(t, s, "implement")
	if _, err = s.ClaimTask(ctx, y.ID, "worker-a", "", ""); !errors.Is(err, ErrTaskJobRequired) {
		t.Fatalf("bare claim: err=%v, want ErrTaskJobRequired", err)
	}
	if _, err = s.ClaimTask(ctx, 1<<40, "worker-a", "job-flow-3", ""); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("claim of a missing task: err=%v, want ErrTaskConflict", err)
	}
}

// flowCaseSettled covers the writes that settle or reopen a task. Entering a
// settled state mirrors decision records, which the decisions area owns, so
// on MySQL it is skipped until that area is ported.
func flowCaseSettled(t *testing.T, s *Store) {
	ctx := t.Context()
	x := flowCaseTask(t, s, "ops")

	got, err := s.CloseTaskCompleted(ctx, x.ID, "hk:doc research/flow-case", "tester", "done", "close-1")
	flowSkipUnported(t, s, err)
	if got.State != StateCompleted || got.CompletedAt == nil || got.Refs.Closure == nil || got.Refs.Closure.Type != "resolved" {
		t.Fatalf("completed task=%+v", got)
	}
	again, err := s.CloseTaskCompleted(ctx, x.ID, "hk:doc research/flow-case", "tester", "done", "close-1")
	flowMust(t, err)
	if again.State != StateCompleted {
		t.Fatalf("replayed close state=%q", again.State)
	}
	if n := len(flowCaseEvents(flowCaseGet(t, s, x.ID), lifecycleSourceCloseCompleted, "close-1")); n != 1 {
		t.Fatalf("%d close events, want 1", n)
	}

	got, err = s.ReopenTaskBy(ctx, x.ID, "tester", "more to do", "reopen-1")
	flowMust(t, err)
	if got.State != StateUnstarted || got.ClaimedBy != "" || got.Refs.Closure != nil || got.CompletedAt != nil {
		t.Fatalf("reopened task=%+v", got)
	}
	if _, err = s.ReopenTask(ctx, x.ID, "tester", "not settled"); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("reopen of an open task: err=%v, want ErrTaskConflict", err)
	}
	if _, err = s.ReopenTask(ctx, x.ID, "tester", " "); err == nil {
		t.Fatal("a reopen without a reason was accepted")
	}

	dropped, err := s.DropTask(ctx, x.ID, "obsolete", "no longer needed", "tester", "drop-1")
	flowMust(t, err)
	if dropped.State != StateDropped || dropped.CanceledAt == nil || dropped.Refs.Closure == nil || dropped.Refs.Closure.Type != "obsolete" {
		t.Fatalf("dropped task=%+v", dropped)
	}
	if _, err = s.DropTask(ctx, x.ID, "resolved", "bad closure", "tester", "drop-2"); !errors.Is(err, ErrClosureInvalid) {
		t.Fatalf("drop with a completion closure: err=%v, want ErrClosureInvalid", err)
	}
	if _, err = s.ReopenTask(ctx, x.ID, "tester", "again"); err != nil {
		t.Fatalf("reopen of a dropped task: %v", err)
	}

	// close-as-merged records both steps of the path under one key.
	m := flowCaseTask(t, s, "implement")
	if _, _, err = s.CloseTaskMerged(ctx, m.ID, "hk:doc evidence", "tester", "n", "merge-0"); !errors.Is(err, ErrTaskMergePath) {
		t.Fatalf("close-as-merged from backlog: err=%v, want ErrTaskMergePath", err)
	}
	_, err = s.TransitionTask(ctx, m.ID, StateDeveloping, "tester", "go", nil, "flow case start")
	flowMust(t, err)
	merged, path, err := s.CloseTaskMerged(ctx, m.ID, "hk:doc evidence", "tester", "shipped", "merge-1")
	flowMust(t, err)
	if merged.State != StateMerged || fmt.Sprint(path) != fmt.Sprint([]string{StateDeveloping, StateReviewing, StateMerged}) {
		t.Fatalf("merged=%q path=%v", merged.State, path)
	}
	_, replay, err := s.CloseTaskMerged(ctx, m.ID, "hk:doc evidence", "tester", "shipped", "merge-1")
	flowMust(t, err)
	if fmt.Sprint(replay) != fmt.Sprint(path) {
		t.Fatalf("replayed path=%v, want %v", replay, path)
	}

	// A duplicate is closed with the relation it points along.
	a, b := flowCaseTask(t, s, "implement"), flowCaseTask(t, s, "implement")
	dup, err := s.DuplicateTask(ctx, a.ID, b.ID, "same work", "tester", "dup-1")
	flowMust(t, err)
	if dup.State != StateDuplicate {
		t.Fatalf("duplicate state=%q", dup.State)
	}
	rels, err := s.ListTaskRelations(ctx, a.ID)
	flowMust(t, err)
	if len(rels) != 1 || rels[0].Kind != RelationDuplicateOf || rels[0].SrcID != a.ID || rels[0].DstID != b.ID {
		t.Fatalf("relations after duplicate=%+v", rels)
	}
	// Reopening ends the duplicate state and archives its edge together.
	if _, err = s.ReopenTask(ctx, a.ID, "tester", "not a duplicate"); err != nil {
		t.Fatal(err)
	}
	if rels, err = s.ListTaskRelations(ctx, a.ID); err != nil || len(rels) != 0 {
		t.Fatalf("relations after reopen=%+v err=%v", rels, err)
	}
	if _, err = s.DuplicateTask(ctx, b.ID, 0, "no target", "tester", "dup-2"); !errors.Is(err, ErrDuplicateOfRequired) {
		t.Fatalf("duplicate without a target: err=%v, want ErrDuplicateOfRequired", err)
	}
}

func flowCaseAwaiting(t *testing.T, s *Store) {
	ctx := t.Context()
	x := flowCaseTask(t, s, "implement")
	// Clearing a flag that is not set is a conflict and writes nothing.
	if _, err := s.ClearAwaitingDecision(ctx, x.ID, "operator", "answer"); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("clear of an unset flag: err=%v, want ErrTaskConflict", err)
	}
	// A question is required to raise the flag.
	if _, err := s.TransitionTask(ctx, x.ID, "needs_decision", "tester", " ", nil, ""); err == nil {
		t.Fatal("needs_decision without a question was accepted")
	}
	// Raising the flag mirrors a decision record, which MySQL does not have yet.
	got, err := s.TransitionTask(ctx, x.ID, "needs_decision", "tester", "which way?", nil, "")
	flowSkipUnported(t, s, err)
	if !got.AwaitingDecision || got.State != StateBacklog {
		t.Fatalf("flagged task state=%q awaiting=%t", got.State, got.AwaitingDecision)
	}
	if _, err = s.TransitionTask(ctx, x.ID, "needs_decision", "tester", "again?", nil, ""); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("second flag: err=%v, want ErrTaskConflict", err)
	}
	got, err = s.ClearAwaitingDecision(ctx, x.ID, "operator", "go left")
	flowMust(t, err)
	if got.AwaitingDecision || got.State != StateBacklog {
		t.Fatalf("answered task state=%q awaiting=%t", got.State, got.AwaitingDecision)
	}
	var flags int
	for _, e := range flowCaseGet(t, s, x.ID).Events {
		if e.Kind == TaskEventAwaiting {
			flags++
		}
	}
	if flags != 2 {
		t.Fatalf("%d awaiting_decision events, want 2 (raise and clear)", flags)
	}
}

func flowCaseRelane(t *testing.T, s *Store) {
	ctx := t.Context()
	a, b := flowCaseTask(t, s, "implement"), flowCaseTask(t, s, "implement")

	lanes, err := s.ListKnownTaskLanes(ctx)
	flowMust(t, err)
	known := map[string]bool{}
	for _, l := range lanes {
		known[l] = true
	}
	if !known[a.Lane] || !known[b.Lane] {
		t.Fatalf("known lanes %v miss %q or %q", lanes, a.Lane, b.Lane)
	}

	r, err := s.OperatorRelaneTask(ctx, a.ID, b.Lane, "tester", "move", "relane-1", "")
	flowSkipUnported(t, s, err)
	if r.Previous != a.Lane || r.Lane != b.Lane || !r.Changed {
		t.Fatalf("relane=%+v", r)
	}
	again, err := s.OperatorRelaneTask(ctx, a.ID, b.Lane, "tester", "move", "relane-1", "")
	flowMust(t, err)
	if again.Lane != b.Lane || !again.Changed {
		t.Fatalf("replayed relane=%+v", again)
	}
	if _, err = s.OperatorRelaneTask(ctx, a.ID, flowCaseName("nowhere"), "tester", "typo", "relane-2", ""); !errors.Is(err, ErrTaskLaneUnknown) {
		t.Fatalf("unknown lane: err=%v, want ErrTaskLaneUnknown", err)
	}
	if n := len(flowCaseEvents(flowCaseGet(t, s, a.ID), lifecycleSourceRelane, "relane-1")); n != 1 {
		t.Fatalf("%d relane events, want 1", n)
	}
}
