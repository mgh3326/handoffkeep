package store

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Waits behaviour on both dialects (hk 5128): every derived source, the hand
// waits, the four sections, the pending_confirmation display and the backlog.
// The cases use the public store methods; the one raw insert is the
// pending_confirmation attempt, which only the verified operator path writes.

func init() {
	registerTaskAreaCase("waits/hand-lifecycle", waitsCaseHandLifecycle)
	registerTaskAreaCase("waits/hand-refusals", waitsCaseHandRefusals)
	registerTaskAreaCase("waits/decision-and-pending-answer", waitsCaseDecisionAndPendingAnswer)
	registerTaskAreaCase("waits/derived-blocks", waitsCaseDerivedBlocks)
	registerTaskAreaCase("waits/derived-spawn-start", waitsCaseDerivedSpawnStart)
	registerTaskAreaCase("waits/derived-review", waitsCaseDerivedReview)
	registerTaskAreaCase("waits/failed-start-section", waitsCaseFailedStartSection)
	registerTaskAreaCase("waits/spawn-failure-comment", waitsCaseSpawnFailureComment)
	registerTaskAreaCase("waits/unacknowledged-order", waitsCaseUnacknowledgedOrder)
	registerTaskAreaCase("waits/backlog-count-exact", waitsCaseBacklogCountExact)
	registerTaskAreaCase("waits/idle-started", waitsCaseIdleStarted)
	registerTaskAreaCase("waits/backlog-apart-and-filter", waitsCaseBacklogApartAndFilter)
}

// waitsRead reads the waits view at the present, for every section.
func waitsRead(t *testing.T, s *Store, section string) WaitsView {
	t.Helper()
	view, err := s.ListWaits(t.Context(), time.Now().UTC(), section)
	flowMust(t, err)
	return view
}

// waitsSection returns the section with key from view.
func waitsSection(t *testing.T, view WaitsView, key string) WaitSection {
	t.Helper()
	for _, sec := range view.Sections {
		if sec.Key == key {
			return sec
		}
	}
	t.Fatalf("no section %q in %+v", key, view.Sections)
	return WaitSection{}
}

// waitsFind returns the row of task taskID in section key, if there is one.
func waitsFind(t *testing.T, view WaitsView, key string, taskID int64) (WaitRow, bool) {
	t.Helper()
	for _, r := range waitsSection(t, view, key).Rows {
		if r.TaskID == taskID {
			return r, true
		}
	}
	return WaitRow{}, false
}

func waitsCaseHandLifecycle(t *testing.T, s *Store) {
	ctx := t.Context()
	a, other := flowCaseTask(t, s, "implement"), flowCaseTask(t, s, "implement")

	w, err := s.AddTaskWait(ctx, a.ID, TaskWaitInput{OnKind: WaitOnOperator, Reason: "register the device key by hand"}, "tester")
	flowMust(t, err)
	if w.OnKind != WaitOnOperator || w.OnRef != "" || w.Source != "session:tester" || w.ClearedAt != nil {
		t.Fatalf("hand wait=%+v", w)
	}
	row, ok := waitsFind(t, waitsRead(t, s, ""), WaitSectionNeedsOperator, a.ID)
	if !ok || row.OnKind != WaitOnOperator || row.Reason != "register the device key by hand" || row.Source != "session:tester" || row.NextAction != "operator_step" {
		t.Fatalf("operator hand wait row=%+v found=%t", row, ok)
	}

	// Repeating an open wait updates its reason and keeps one row.
	again, err := s.AddTaskWait(ctx, a.ID, TaskWaitInput{OnKind: WaitOnOperator, Reason: "register the device key by hand (retry)"}, "tester")
	flowMust(t, err)
	if again.ID != w.ID || again.Reason != "register the device key by hand (retry)" {
		t.Fatalf("repeated wait=%+v, first id %d", again, w.ID)
	}
	if n := waitsSection(t, waitsRead(t, s, ""), WaitSectionNeedsOperator).Count; n != 1 {
		t.Fatalf("needs_operator count after a repeat=%d, want 1", n)
	}

	// Clearing it ends the wait and keeps the row; clearing again changes nothing.
	cleared, changed, err := s.ClearTaskWait(ctx, a.ID, w.ID, "tester", "device registered")
	flowMust(t, err)
	if !changed || cleared.ClearedAt == nil || cleared.ClearedBy != "tester" || cleared.ClearNote != "device registered" {
		t.Fatalf("clear: changed=%t wait=%+v", changed, cleared)
	}
	if _, changed, err = s.ClearTaskWait(ctx, a.ID, w.ID, "tester", ""); err != nil || changed {
		t.Fatalf("second clear: changed=%t err=%v", changed, err)
	}
	if _, ok := waitsFind(t, waitsRead(t, s, ""), WaitSectionNeedsOperator, a.ID); ok {
		t.Fatal("a cleared wait is still listed")
	}

	// A wait of the same kind and ref can be set again after it ended.
	second, err := s.AddTaskWait(ctx, a.ID, TaskWaitInput{OnKind: WaitOnOperator, Reason: "still needed"}, "tester")
	flowMust(t, err)
	if second.ID == w.ID {
		t.Fatal("a re-set wait reused the cleared row")
	}

	// A wait names its own task: clearing it through another task is not found.
	if _, _, err := s.ClearTaskWait(ctx, other.ID, second.ID, "tester", ""); !errors.Is(err, ErrTaskWaitNotFound) {
		t.Fatalf("clear through another task: %v", err)
	}
	if _, _, err := s.ClearTaskWait(ctx, a.ID, second.ID+1000, "tester", ""); !errors.Is(err, ErrTaskWaitNotFound) {
		t.Fatalf("clear of an unknown wait: %v", err)
	}
}

func waitsCaseHandRefusals(t *testing.T, s *Store) {
	ctx := t.Context()
	a := flowCaseTask(t, s, "implement")
	if _, err := s.AddTaskWait(ctx, a.ID, TaskWaitInput{OnKind: "robot", OnRef: "x", Reason: "r"}, "tester"); !errors.Is(err, ErrTaskWaitInvalid) {
		t.Fatalf("unknown kind: %v", err)
	}
	if _, err := s.AddTaskWait(ctx, a.ID, TaskWaitInput{OnKind: WaitOnTask, OnRef: "999999999", Reason: "r"}, "tester"); !errors.Is(err, ErrTaskWaitInvalid) {
		t.Fatalf("task wait on a missing task: %v", err)
	}
	blocker := flowCaseTask(t, s, "implement")
	if _, err := s.AddTaskWait(ctx, a.ID, TaskWaitInput{OnKind: WaitOnTask, OnRef: strconv.FormatInt(blocker.ID, 10), Reason: ""}, "tester"); !errors.Is(err, ErrTaskWaitInvalid) {
		t.Fatalf("task wait with an empty reason: %v", err)
	}
	if _, err := s.AddTaskWait(ctx, 999999999, TaskWaitInput{OnKind: WaitOnOperator, Reason: "r"}, "tester"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("wait on a missing task: %v", err)
	}
	if _, err := s.AddTaskWait(ctx, a.ID, TaskWaitInput{OnKind: WaitOnOperator, Reason: "r"}, ""); !errors.Is(err, ErrTaskWaitInvalid) {
		t.Fatalf("missing author: %v", err)
	}
	if _, err := s.ListWaits(ctx, time.Now().UTC(), "all"); !errors.Is(err, ErrTaskWaitInvalid) {
		t.Fatalf("unknown section: %v", err)
	}
}

// waitsRawExec runs one fixture statement in the dialect of s. The fixtures
// below write the rows the public write paths do not yet reach on MySQL, so the
// projections are exercised on both dialects instead of being skipped.
// Each dialect's statement binds its own argument list: PostgreSQL reuses one
// numbered placeholder where MySQL repeats the value, so the lists differ.
//
// The write goes through begin and Commit, as every decision and task write does
// on MySQL: the sync triggers number their rows only inside that transaction.
func waitsRawExec(t *testing.T, s *Store, pg, my string, pgArgs, myArgs []any) {
	t.Helper()
	ctx := t.Context()
	args := pgArgs
	if s.dialect == mysqlDialect {
		args = myArgs
	}
	tx, release, err := s.begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, sqlFor(tx, pg, my), args...); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func waitsCaseDecisionAndPendingAnswer(t *testing.T, s *Store) {
	task := flowCaseTask(t, s, "implement")
	now := time.Now().UTC().Truncate(time.Microsecond)
	ref := "dr-" + strconv.FormatInt(task.ID, 10) + "-1"
	waitsRawExec(t, s,
		`INSERT INTO decision_requests(source,source_ref,task_id,lane,question,created_at,updated_at) VALUES('task_request',$1,$2,$3,'waits case question',$4,$4)`,
		`INSERT INTO decision_requests(source,source_ref,task_id,lane,question,created_at,updated_at) VALUES('task_request',?,?,?,'waits case question',?,?)`,
		[]any{ref, task.ID, task.Lane, now}, []any{ref, task.ID, task.Lane, now, now})

	row, ok := waitsFind(t, waitsRead(t, s, ""), WaitSectionNeedsOperator, task.ID)
	if !ok || row.Source != "derived:decision_request" || row.OnKind != WaitOnOperator || row.NextAction != "answer_decision" || row.RequestID != ref {
		t.Fatalf("decision row=%+v found=%t", row, ok)
	}
	if row.Answer != nil {
		t.Fatalf("an unanswered request carries an answer: %+v", row.Answer)
	}

	// An answer recorded but not yet confirmed (a pending_confirmation attempt)
	// stays in the section as answered, with its words.
	hash := strings.Repeat("a", 64)
	waitsRawExec(t, s,
		`INSERT INTO decision_answer_attempts(channel,event_key,payload_hash,task_id,request_id,revision_request_id,actor,result,received_at,text) VALUES('console',$1,$2,$3,$4,$4,'operator',$5,$6,$7)`,
		`INSERT INTO decision_answer_attempts(channel,event_key,payload_hash,task_id,request_id,revision_request_id,actor,result,received_at,text) VALUES('console',?,?,?,?,?,'operator',?,?,?)`,
		[]any{"waits-case-" + ref, hash, task.ID, ref, "pending_confirmation", now, "범위는 A로"},
		[]any{"waits-case-" + ref, hash, task.ID, ref, ref, "pending_confirmation", now, "범위는 A로"})
	answered, ok := waitsFind(t, waitsRead(t, s, ""), WaitSectionNeedsOperator, task.ID)
	if !ok || answered.NextAction != "confirm_answer" || answered.Answer == nil {
		t.Fatalf("answered-awaiting row=%+v found=%t", answered, ok)
	}
	if answered.Answer.Result != DecisionResultPendingConfirmation || answered.Answer.Words != "범위는 A로" || answered.Answer.Actor != "operator" {
		t.Fatalf("answer=%+v", answered.Answer)
	}
}

func waitsCaseDerivedBlocks(t *testing.T, s *Store) {
	ctx := t.Context()
	blocker, blocked := flowCaseTask(t, s, "implement"), flowCaseTask(t, s, "implement")
	if changed, err := s.AddTaskRelation(ctx, blocker.ID, RelationBlocks, blocked.ID, "tester"); err != nil || !changed {
		t.Fatalf("blocks edge: changed=%t err=%v", changed, err)
	}
	row, ok := waitsFind(t, waitsRead(t, s, ""), WaitSectionWaitingOn, blocked.ID)
	if !ok || row.Source != "derived:blocks" || row.OnKind != WaitOnTask || row.OnRef != strconv.FormatInt(blocker.ID, 10) || row.NextAction != "wait_blocker" {
		t.Fatalf("blocked row=%+v found=%t", row, ok)
	}
	// Settling the blocker ends the wait: a derived wait is never stored.
	if _, err := s.TransitionTaskWith(ctx, blocker.ID, TransitionInput{To: StateDropped, By: "tester", Note: "dropped in the waits case", Evidence: "declined"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := waitsFind(t, waitsRead(t, s, ""), WaitSectionWaitingOn, blocked.ID); ok {
		t.Fatal("a blocks wait outlived its blocker's settlement")
	}
}

func waitsCaseDerivedSpawnStart(t *testing.T, s *Store) {
	ctx := t.Context()
	start, queued := flowCaseTask(t, s, "implement"), flowCaseTask(t, s, "implement")
	_, _, err := s.PutTaskSpawn(ctx, queued.ID, TaskSpawnInput{
		RequestID: "req-waits-1", Job: "job-waits-1", Role: "implement", State: SpawnWaiting, Attempt: 1, UpdatedAt: time.Now().UTC(),
		StartCondition: &TaskSpawnStartInput{TaskID: start.ID, Until: "merged"},
	}, "tester")
	flowMust(t, err)
	row, ok := waitsFind(t, waitsRead(t, s, ""), WaitSectionWaitingOn, queued.ID)
	if !ok || row.Source != "derived:spawn_start" || row.OnRef != strconv.FormatInt(start.ID, 10) || row.NextAction != "wait_start_condition" {
		t.Fatalf("queued spawn row=%+v found=%t", row, ok)
	}
}

func waitsCaseDerivedReview(t *testing.T, s *Store) {
	ctx := t.Context()
	task := flowCaseTask(t, s, "implement")
	now := time.Now().UTC().Truncate(time.Microsecond)
	ref := ChangeRef{Repo: "example/app", PR: 7401}
	state := validState(now)
	if _, err := s.ApplyChangeBatch(ctx, ChangeBatch{ChangeRef: ref, State: &state}); err != nil {
		t.Fatal(err)
	}
	var changeID int64
	if err := s.db().QueryRow(ctx, sqlFor(s.db(),
		`SELECT id FROM changes WHERE repo=$1 AND pr_number=$2`,
		`SELECT id FROM changes WHERE repo=? AND pr_number=?`), ref.Repo, ref.PR).Scan(&changeID); err != nil {
		t.Fatal(err)
	}
	// The change is linked to the task as an implementing change, the link the
	// source-event path writes; the fixture writes it directly so MySQL reads it too.
	waitsRawExec(t, s,
		`INSERT INTO change_tasks(change_id,task_id,relation,created_at) VALUES($1,$2,'implements',$3)`,
		`INSERT INTO change_tasks(change_id,task_id,relation,created_at) VALUES(?,?,'implements',?)`,
		[]any{changeID, task.ID, now}, []any{changeID, task.ID, now})

	row, ok := waitsFind(t, waitsRead(t, s, ""), WaitSectionWaitingOn, task.ID)
	if !ok || row.Source != "derived:change_review" || row.OnKind != WaitOnPR || row.OnRef != "example/app#7401" || row.NextAction != "wait_review" {
		t.Fatalf("review row=%+v found=%t", row, ok)
	}
	waitsRawExec(t, s,
		`UPDATE changes SET state='merged', merged_at=$1 WHERE id=$2`,
		`UPDATE changes SET state='merged', merged_at=? WHERE id=?`,
		[]any{now, changeID}, []any{now, changeID})
	if _, ok := waitsFind(t, waitsRead(t, s, ""), WaitSectionWaitingOn, task.ID); ok {
		t.Fatal("a merged PR still waits for review")
	}
}

func waitsCaseFailedStartSection(t *testing.T, s *Store) {
	ctx := t.Context()
	task := flowCaseTask(t, s, "implement")
	if _, err := s.AddTaskWait(ctx, task.ID, TaskWaitInput{OnKind: WaitOnExternal, OnRef: WaitRefSpawn, Reason: "502 at hub-accept"}, "tester"); err != nil {
		t.Fatal(err)
	}
	row, ok := waitsFind(t, waitsRead(t, s, ""), WaitSectionFailedStart, task.ID)
	if !ok || row.NextAction != "retry_spawn" || row.OnRef != WaitRefSpawn {
		t.Fatalf("failed start row=%+v found=%t", row, ok)
	}
	// An external wait on anything else is a plain waiting_on item.
	if _, err := s.AddTaskWait(ctx, task.ID, TaskWaitInput{OnKind: WaitOnExternal, OnRef: "deploy-window", Reason: "freeze"}, "tester"); err != nil {
		t.Fatal(err)
	}
	if _, ok := waitsFind(t, waitsRead(t, s, ""), WaitSectionWaitingOn, task.ID); !ok {
		t.Fatal("an external wait other than spawn is not in waiting_on")
	}
}

func waitsCaseIdleStarted(t *testing.T, s *Store) {
	task := flowCaseTask(t, s, "implement")
	if _, err := s.TransitionTask(t.Context(), task.ID, StateDeveloping, "tester", "", nil, "waits case: no job"); err != nil {
		t.Fatal(err)
	}
	// Read far enough ahead that the task's last activity is older than the
	// started-stalled window.
	view, err := s.ListWaits(t.Context(), time.Now().UTC().Add(1000*time.Hour), "")
	flowMust(t, err)
	row, ok := waitsFind(t, view, WaitSectionIdleStarted, task.ID)
	if !ok || row.Source != "derived:drift_idle_started" || row.NextAction != "check_progress" {
		t.Fatalf("idle row=%+v found=%t", row, ok)
	}
}

func waitsCaseBacklogApartAndFilter(t *testing.T, s *Store) {
	ctx := t.Context()
	plain, waited := flowCaseTask(t, s, "implement"), flowCaseTask(t, s, "implement")
	if _, err := s.AddTaskWait(ctx, waited.ID, TaskWaitInput{OnKind: WaitOnOperator, Reason: "the operator step"}, "tester"); err != nil {
		t.Fatal(err)
	}
	view := waitsRead(t, s, "")
	listed := false
	for _, b := range view.Backlog.Rows {
		if b.TaskID == waited.ID {
			t.Fatal("a backlog task that waits is listed in the backlog")
		}
		if b.TaskID == plain.ID {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("a plain backlog task is not in the backlog: %+v", view.Backlog)
	}

	filtered := waitsRead(t, s, WaitSectionFailedStart)
	if filtered.Section != WaitSectionFailedStart || len(filtered.Backlog.Rows) != 0 {
		t.Fatalf("a section filter keeps backlog rows: %+v", filtered.Backlog)
	}
	if filtered.Sections[0].Count != 1 || len(filtered.Sections[0].Rows) != 0 {
		t.Fatalf("needs_operator under a failed_start filter: %+v", filtered.Sections[0])
	}
	if len(filtered.Sections[1].Rows) != 0 || filtered.Sections[1].Count != 0 {
		t.Fatalf("failed_start should be empty: %+v", filtered.Sections[1])
	}
	if _, err := s.ListWaits(t.Context(), time.Now().UTC(), "nope"); !errors.Is(err, ErrTaskWaitInvalid) {
		t.Fatalf("an unknown section is accepted: %v", err)
	}
}

// A spawn failure comment (panewire's, or a session's by hand) is a derived
// failed start until the task is started again; the session's external:spawn
// hand wait on the same failure stays alongside it.
func waitsCaseSpawnFailureComment(t *testing.T, s *Store) {
	ctx := t.Context()
	task := flowCaseTask(t, s, "implement")
	_, err := s.CreateTaskComment(ctx, task.ID, "panewire", "spawn failed request=req-9 rc=1 stage=hub-accept reason=502 at hub-accept")
	flowMust(t, err)
	row, ok := waitsFind(t, waitsRead(t, s, ""), WaitSectionFailedStart, task.ID)
	if !ok || row.Source != "derived:spawn_failed_comment" || row.OnKind != WaitOnExternal || row.OnRef != WaitRefSpawn || row.NextAction != "retry_spawn" || !strings.HasPrefix(row.Reason, "spawn failed request=req-9") {
		t.Fatalf("spawn failure row=%+v found=%t", row, ok)
	}
	if _, err := s.AddTaskWait(ctx, task.ID, TaskWaitInput{OnKind: WaitOnExternal, OnRef: WaitRefSpawn, Reason: "502 at hub-accept"}, "tester"); err != nil {
		t.Fatal(err)
	}
	both := 0
	for _, r := range waitsSection(t, waitsRead(t, s, ""), WaitSectionFailedStart).Rows {
		if r.TaskID == task.ID {
			both++
		}
	}
	if both != 2 {
		t.Fatalf("failed_start rows for the task=%d, want the derived failure and the hand wait", both)
	}
	// A transition into a started state after the comment ends the failed start.
	if _, err := s.TransitionTask(ctx, task.ID, StateDeveloping, "tester", "", nil, "waits case: no job"); err != nil {
		t.Fatal(err)
	}
	for _, r := range waitsSection(t, waitsRead(t, s, ""), WaitSectionFailedStart).Rows {
		if r.Source == "derived:spawn_failed_comment" && r.TaskID == task.ID {
			t.Fatal("a started task still shows a derived failed start")
		}
	}
}

// An open order waits on its target lane until the lane acknowledges it.
func waitsCaseUnacknowledgedOrder(t *testing.T, s *Store) {
	ctx := t.Context()
	task := flowCaseTask(t, s, "implement")
	if _, err := s.CreateTaskOrder(ctx, CreateTaskOrderInput{TaskID: task.ID, Words: "set going", Actor: "operator-test", Channel: "console", Verified: true, Key: "waits-order-" + strconv.FormatInt(task.ID, 10)}); err != nil {
		t.Fatal(err)
	}
	orders, err := s.ListTaskOrders(ctx, task.ID, true)
	flowMust(t, err)
	if len(orders) != 1 {
		t.Fatalf("open orders=%d, want 1", len(orders))
	}
	row, ok := waitsFind(t, waitsRead(t, s, ""), WaitSectionWaitingOn, task.ID)
	if !ok || row.Source != "derived:task_order" || row.OnKind != WaitOnLane || row.OnRef != task.Lane || row.NextAction != "wait_lane" {
		t.Fatalf("order row=%+v found=%t", row, ok)
	}
	if _, err := s.AckTaskOrder(ctx, orders[0].ID, task.Lane, "started"); err != nil {
		t.Fatal(err)
	}
	if _, ok := waitsFind(t, waitsRead(t, s, ""), WaitSectionWaitingOn, task.ID); ok {
		t.Fatal("an acknowledged order still waits on its lane")
	}
}

// The ordinary backlog count is exact and the listed rows are capped: with more
// than waitBacklogRows ordinary backlog tasks and one waiting backlog task, the
// count leaves the waiting task out and Truncated says the rows are not all of it.
func waitsCaseBacklogCountExact(t *testing.T, s *Store) {
	ctx := t.Context()
	const plain = waitBacklogRows + 2
	waited := flowCaseTask(t, s, "implement")
	if _, err := s.AddTaskWait(ctx, waited.ID, TaskWaitInput{OnKind: WaitOnOperator, Reason: "the operator step"}, "tester"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < plain; i++ {
		flowCaseTask(t, s, "implement")
	}
	view := waitsRead(t, s, "")
	if view.Backlog.Count != plain {
		t.Fatalf("backlog count=%d, want %d (the waiting task excluded)", view.Backlog.Count, plain)
	}
	if len(view.Backlog.Rows) != waitBacklogRows || !view.Backlog.Truncated {
		t.Fatalf("backlog rows=%d truncated=%t, want %d rows and truncated", len(view.Backlog.Rows), view.Backlog.Truncated, waitBacklogRows)
	}
	for _, b := range view.Backlog.Rows {
		if b.TaskID == waited.ID {
			t.Fatal("the waiting task is listed as ordinary backlog")
		}
	}
}
