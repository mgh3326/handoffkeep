package store

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Dialect-neutral cases for task_source_events.go (registry:
// tasks_area_cases_test.go). ApplyTaskSourceEvent itself refuses on MySQL
// until B5 ports the change records it writes (recordChangeTx), so the cases
// that need it expect ErrDialectUnsupported there and the full behaviour on
// PostgreSQL. The event rules run through applySourceEventTx on both, the way
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
		if _, err := s.ApplyTaskSourceEvent(ctx, in); err == nil || errors.Is(err, ErrDialectUnsupported) || errors.Is(err, ErrTaskNotFound) {
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
	if s.dialect == mysqlDialect {
		// B5 dependency: recordChangeTx. The refusal writes nothing.
		if !errors.Is(err, ErrDialectUnsupported) {
			t.Fatalf("err=%v, want ErrDialectUnsupported", err)
		}
		after := srcEvGet(t, s, x.ID)
		if after.State != x.State || len(srcEvEventsOf(after, SourceMergewakePR)) != 0 {
			t.Fatalf("a refused event changed the task: state %s -> %s, events %v", x.State, after.State, after.Events)
		}
		return
	}
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
	if s.dialect == mysqlDialect {
		// B5/B6 dependency: the merge hold reads the change tables, and a task
		// that merges ends its decisions. The refusal comes before any write.
		if !errors.Is(err, ErrDialectUnsupported) {
			t.Fatalf("err=%v, want ErrDialectUnsupported", err)
		}
		after := srcEvGet(t, s, x.ID)
		if after.State != x.State || len(srcEvEventsOf(after, SourceMergewakeMerge)) != 0 || after.Refs.PR != "" {
			t.Fatalf("a refused merge changed the task: %+v", after)
		}
		return
	}
	if err != nil || !res.Applied || res.State != StateMerged {
		t.Fatalf("merge: res=%+v err=%v", res, err)
	}
	if after := srcEvGet(t, s, x.ID); after.State != StateMerged || after.Refs.PR != pr {
		t.Fatalf("after merge: state=%s refs.pr=%q", after.State, after.Refs.PR)
	}
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
