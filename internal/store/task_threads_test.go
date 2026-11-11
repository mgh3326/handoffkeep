package store

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func startedThreadTask(t *testing.T, s *Store, project, title string) Task {
	t.Helper()
	ctx := t.Context()
	p := project
	x, err := s.CreateTask(ctx, Task{Lane: relaneLane(t, "thread"), Title: title, Kind: "implement", Priority: 5, CreatedBy: "tester", Project: &p})
	if err != nil {
		t.Fatal(err)
	}
	if x, err = s.TransitionTask(ctx, x.ID, StateDeveloping, "tester", "start", nil, "started by the thread test"); err != nil {
		t.Fatal(err)
	}
	return x
}

func threadState(t *testing.T, s *Store, id int64) TaskThreadState {
	t.Helper()
	x, found, err := s.GetTaskThread(t.Context(), id)
	if err != nil || !found {
		t.Fatalf("task %d delivery: found=%v err=%v", id, found, err)
	}
	return x
}

func TestTaskThreadQueueEpochAndProjects(t *testing.T) {
	s := schemaStore(t, "task_threads_queue")
	ctx := t.Context()
	if _, err := s.AddTaskProject(ctx, "other-proj", "tester"); err != nil {
		t.Fatal(err)
	}
	projects := []string{testProjectName}
	before := startedThreadTask(t, s, testProjectName, "started before the poster")
	now := time.Now().UTC()
	if n, err := s.QueueTaskThreads(ctx, now, projects); err != nil || n != 0 {
		t.Fatalf("first pass queued %d err=%v", n, err)
	}
	in := startedThreadTask(t, s, testProjectName, "in the list")
	out := startedThreadTask(t, s, "other-proj", "outside the list")
	if n, err := s.QueueTaskThreads(ctx, now, projects); err != nil || n != 1 {
		t.Fatalf("second pass queued %d err=%v", n, err)
	}
	x := threadState(t, s, in.ID)
	if x.State != TaskThreadPending || x.ThreadID != nil || x.LastEventID != x.StartEventID-1 || !strings.HasPrefix(x.ThreadName, "#") {
		t.Fatalf("queued row %+v", x)
	}
	for _, id := range []int64{before.ID, out.ID} {
		if _, found, err := s.GetTaskThread(ctx, id); err != nil || found {
			t.Fatalf("task %d queued: found=%v err=%v", id, found, err)
		}
	}
	// A second start of the same task owes nothing more.
	if _, err := s.TransitionTask(ctx, in.ID, StateUnstarted, "tester", "park", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTask(ctx, in.ID, StateDeveloping, "tester", "again", nil, "restarted"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.QueueTaskThreads(ctx, now, projects); err != nil || n != 0 {
		t.Fatalf("restart queued %d err=%v", n, err)
	}
	if got := threadState(t, s, in.ID); got.StartEventID != x.StartEventID {
		t.Fatalf("restart moved the start: %+v", got)
	}
}

func TestTaskThreadClaimSettleAndRecover(t *testing.T) {
	s := schemaStore(t, "task_threads_claim")
	ctx := t.Context()
	projects := []string{testProjectName}
	now := time.Now().UTC()
	if _, err := s.QueueTaskThreads(ctx, now, projects); err != nil {
		t.Fatal(err)
	}
	task := startedThreadTask(t, s, testProjectName, "claims")
	if _, err := s.QueueTaskThreads(ctx, now, projects); err != nil {
		t.Fatal(err)
	}

	create, found, err := s.ClaimTaskThread(ctx, now)
	if err != nil || !found || create.Type != TaskThreadCreate || create.TaskID != task.ID || create.Project != testProjectName {
		t.Fatalf("claim create %+v found=%v err=%v", create, found, err)
	}
	if x := threadState(t, s, task.ID); x.State != TaskThreadCreating || x.ClaimKey != TaskThreadCreate {
		t.Fatalf("claimed row %+v", x)
	}
	if _, found, _ := s.ClaimTaskThread(ctx, now); found {
		t.Fatal("a claimed create was claimed again")
	}
	// A 500: back to pending with backoff and one attempt.
	if gaveUp, err := s.SettleTaskThread(ctx, now, create, TaskThreadResult{Outcome: TaskThreadRetry, Err: errors.New("discord webhook status 500")}); err != nil || gaveUp {
		t.Fatalf("settle retry gaveUp=%v err=%v", gaveUp, err)
	}
	if x := threadState(t, s, task.ID); x.State != TaskThreadPending || x.Attempts != 1 || !x.NextAttemptAt.After(now) || x.ClaimKey != "" {
		t.Fatalf("after retry %+v", x)
	}
	if _, found, _ := s.ClaimTaskThread(ctx, now); found {
		t.Fatal("claimed before the backoff ended")
	}
	later := now.Add(time.Hour)
	create, found, err = s.ClaimTaskThread(ctx, later)
	if err != nil || !found || create.Attempts != 1 {
		t.Fatalf("second claim %+v found=%v err=%v", create, found, err)
	}
	if _, err = s.SettleTaskThread(ctx, later, create, TaskThreadResult{Outcome: TaskThreadSent, ThreadID: "9001"}); err != nil {
		t.Fatal(err)
	}
	x := threadState(t, s, task.ID)
	if x.State != TaskThreadActive || x.ThreadID == nil || *x.ThreadID != "9001" || x.Attempts != 0 || x.LastEventID != x.StartEventID-1 {
		t.Fatalf("after create %+v", x)
	}
	// Settling a claim that is no longer held fails and changes nothing.
	if _, err = s.SettleTaskThread(ctx, later, create, TaskThreadResult{Outcome: TaskThreadSent, ThreadID: "9002"}); err == nil {
		t.Fatal("a stale claim settled")
	}

	pr := testPR(301)
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: pr, TaskID: task.ID})
	line, found, err := s.ClaimTaskThread(ctx, later)
	if err != nil || !found || line.Type != TaskLinePROpened || line.PR != pr || line.ThreadID != "9001" || !strings.HasPrefix(line.Key, "e:") {
		t.Fatalf("claim line %+v found=%v err=%v", line, found, err)
	}
	// The worker dies mid-send. Within the lease nothing happens; after it
	// the line counts as posted, so it is never sent twice.
	if got, err := s.RecoverTaskThreadClaims(ctx, later.Add(time.Minute)); err != nil || len(got) != 0 {
		t.Fatalf("recovered inside the lease: %+v err=%v", got, err)
	}
	got, err := s.RecoverTaskThreadClaims(ctx, later.Add(TaskThreadClaimLease+time.Second))
	if err != nil || len(got) != 1 || got[0].Key != line.Key || got[0].State != TaskThreadActive {
		t.Fatalf("recovered %+v err=%v", got, err)
	}
	x = threadState(t, s, task.ID)
	if x.ClaimKey != "" || "e:"+strconv.FormatInt(x.LastEventID, 10) != line.Key || !strings.Contains(x.LastError, "unknown") {
		t.Fatalf("after line recovery %+v", x)
	}
	if extra, found, _ := s.ClaimTaskThread(ctx, later.Add(time.Hour)); found {
		t.Fatalf("recovered line claimed again: %+v", extra)
	}

	// A refused line stops the row.
	send(t, s, mergeEvent(task.ID, pr))
	line, found, err = s.ClaimTaskThread(ctx, later.Add(time.Hour))
	if err != nil || !found || line.Type != TaskLineMerged || line.PR != pr {
		t.Fatalf("merged line %+v found=%v err=%v", line, found, err)
	}
	if gaveUp, err := s.SettleTaskThread(ctx, later.Add(time.Hour), line, TaskThreadResult{Outcome: TaskThreadRejected, Err: errors.New("discord webhook status 404")}); err != nil || !gaveUp {
		t.Fatalf("settle rejected gaveUp=%v err=%v", gaveUp, err)
	}
	if x = threadState(t, s, task.ID); x.State != TaskThreadFailed {
		t.Fatalf("after rejection %+v", x)
	}
}

// A create whose outcome is unknown is never created blind: it waits
// TaskThreadLookupDelay, then comes back as a lookup. A lookup that cannot
// tell stops the row needs_operator; one that finds the thread adopts it.
func TestTaskThreadUnknownCreateIsLookedUp(t *testing.T) {
	s := schemaStore(t, "task_threads_unknown")
	ctx := t.Context()
	projects := []string{testProjectName}
	now := time.Now().UTC()
	if _, err := s.QueueTaskThreads(ctx, now, projects); err != nil {
		t.Fatal(err)
	}
	a := startedThreadTask(t, s, testProjectName, "timeout on create")
	b := startedThreadTask(t, s, testProjectName, "worker died on create")
	if _, err := s.QueueTaskThreads(ctx, now, projects); err != nil {
		t.Fatal(err)
	}
	first, _, err := s.ClaimTaskThread(ctx, now)
	if err != nil || first.TaskID != a.ID || first.Lookup {
		t.Fatalf("first claim %+v err=%v", first, err)
	}
	if gaveUp, err := s.SettleTaskThread(ctx, now, first, TaskThreadResult{Outcome: TaskThreadUnknown, Err: errors.New("context deadline exceeded")}); err != nil || gaveUp {
		t.Fatalf("settle unknown gaveUp=%v err=%v", gaveUp, err)
	}
	if x := threadState(t, s, a.ID); x.State != TaskThreadCreating || x.ClaimKey != "" || x.Attempts != 1 || x.NextAttemptAt.Sub(now.Add(TaskThreadLookupDelay)).Abs() > time.Millisecond || !strings.Contains(x.LastError, "unknown") {
		t.Fatalf("after unknown %+v", x)
	}
	second, _, err := s.ClaimTaskThread(ctx, now)
	if err != nil || second.TaskID != b.ID || second.Lookup {
		t.Fatalf("second claim %+v err=%v", second, err)
	}
	expired := now.Add(TaskThreadClaimLease + time.Second)
	got, err := s.RecoverTaskThreadClaims(ctx, expired)
	if err != nil || len(got) != 1 || got[0].TaskID != b.ID || got[0].State != TaskThreadCreating {
		t.Fatalf("recovered %+v err=%v", got, err)
	}
	if x := threadState(t, s, b.ID); x.ClaimKey != "" || x.NextAttemptAt.After(expired) {
		t.Fatalf("after recovery %+v", x)
	}

	// Before the delay the first row is not due; nothing is created blind.
	if x, found, _ := s.ClaimTaskThread(ctx, now.Add(TaskThreadLookupDelay-time.Second)); found {
		t.Fatalf("claimed %+v before the settle delay", x)
	}
	// After it (and the recovered row at once) both come back as lookups.
	at := now.Add(TaskThreadLookupDelay + 2*time.Second)
	lookups := map[int64]TaskThreadItem{}
	for range 2 {
		x, found, err := s.ClaimTaskThread(ctx, at)
		if err != nil || !found || x.Type != TaskThreadCreate || !x.Lookup {
			t.Fatalf("lookup claim %+v found=%v err=%v", x, found, err)
		}
		lookups[x.TaskID] = x
	}
	// A rate-limited lookup did not run: the row comes back as a lookup.
	if gaveUp, err := s.SettleTaskThread(ctx, at, lookups[a.ID], TaskThreadResult{Outcome: TaskThreadRetry, RetryAfter: time.Second, Err: errors.New("thread lookup: discord bot status 429")}); err != nil || gaveUp {
		t.Fatalf("settle lookup retry gaveUp=%v err=%v", gaveUp, err)
	}
	if x := threadState(t, s, a.ID); x.State != TaskThreadCreating || x.ClaimKey != "" {
		t.Fatalf("after lookup retry %+v", x)
	}
	at = at.Add(2 * time.Second)
	again, found, err := s.ClaimTaskThread(ctx, at)
	if err != nil || !found || again.TaskID != a.ID || !again.Lookup {
		t.Fatalf("lookup after retry %+v found=%v err=%v", again, found, err)
	}
	lookups[a.ID] = again
	if gaveUp, err := s.SettleTaskThread(ctx, at, lookups[a.ID], TaskThreadResult{Outcome: TaskThreadAmbiguous, Err: errors.New("2 threads match the lost create")}); err != nil || !gaveUp {
		t.Fatalf("settle ambiguous gaveUp=%v err=%v", gaveUp, err)
	}
	if x := threadState(t, s, a.ID); x.State != TaskThreadNeedsOperator || x.ThreadID != nil || x.ClaimKey != "" {
		t.Fatalf("after ambiguous %+v", x)
	}
	if _, err := s.SettleTaskThread(ctx, at, lookups[b.ID], TaskThreadResult{Outcome: TaskThreadSent, ThreadID: "9100"}); err != nil {
		t.Fatal(err)
	}
	if x := threadState(t, s, b.ID); x.State != TaskThreadActive || x.ThreadID == nil || *x.ThreadID != "9100" {
		t.Fatalf("after adopt %+v", x)
	}
	// The needs_operator row is never created again and gets no line.
	send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: testPR(302), TaskID: a.ID})
	for {
		x, found, err := s.ClaimTaskThread(ctx, now.Add(24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			break
		}
		if x.TaskID == a.ID {
			t.Fatalf("claimed %+v after needs_operator", x)
		}
		if _, err := s.SettleTaskThread(ctx, now.Add(24*time.Hour), x, TaskThreadResult{Outcome: TaskThreadSent}); err != nil {
			t.Fatal(err)
		}
	}
}

// A create whose outcome stays unknown stops needs_operator after the
// attempt cap, and only a create can be ambiguous.
func TestTaskThreadUnknownCreateGivesUp(t *testing.T) {
	s := schemaStore(t, "task_threads_unknown_cap")
	ctx := t.Context()
	projects := []string{testProjectName}
	now := time.Now().UTC()
	if _, err := s.QueueTaskThreads(ctx, now, projects); err != nil {
		t.Fatal(err)
	}
	task := startedThreadTask(t, s, testProjectName, "always unknown")
	if _, err := s.QueueTaskThreads(ctx, now, projects); err != nil {
		t.Fatal(err)
	}
	x, found, err := s.ClaimTaskThread(ctx, now)
	if err != nil || !found {
		t.Fatalf("claim found=%v err=%v", found, err)
	}
	if _, err := s.SettleTaskThread(ctx, now, TaskThreadItem{TaskID: x.TaskID, Destination: x.Destination, Key: "e:1", Type: TaskLineMerged}, TaskThreadResult{Outcome: TaskThreadAmbiguous}); err == nil {
		t.Fatal("a line settled ambiguous")
	}
	x.Attempts = taskThreadMaxAttempts - 1
	if gaveUp, err := s.SettleTaskThread(ctx, now, x, TaskThreadResult{Outcome: TaskThreadUnknown, Err: errors.New("discord bot status 502")}); err != nil || !gaveUp {
		t.Fatalf("settle gaveUp=%v err=%v", gaveUp, err)
	}
	if got := threadState(t, s, task.ID); got.State != TaskThreadNeedsOperator || got.Attempts != taskThreadMaxAttempts {
		t.Fatalf("after the cap %+v", got)
	}
}

// A PR event that itself starts the task is the thread's first line.
func TestTaskThreadStartThatIsAPREvent(t *testing.T) {
	s := schemaStore(t, "task_threads_prstart")
	ctx := t.Context()
	projects := []string{testProjectName}
	now := time.Now().UTC()
	if _, err := s.QueueTaskThreads(ctx, now, projects); err != nil {
		t.Fatal(err)
	}
	p := testProjectName
	x, err := s.CreateTask(ctx, Task{Lane: relaneLane(t, "thread"), Title: "started by its PR", Kind: "implement", Priority: 5, CreatedBy: "tester", Project: &p})
	if err != nil {
		t.Fatal(err)
	}
	pr := testPR(303)
	if res := send(t, s, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: pr, TaskID: x.ID}); !res.Applied || res.State != StateReviewing {
		t.Fatalf("pr event %+v", res)
	}
	if n, err := s.QueueTaskThreads(ctx, now, projects); err != nil || n != 1 {
		t.Fatalf("queued %d err=%v", n, err)
	}
	create, _, err := s.ClaimTaskThread(ctx, now)
	if err != nil || create.Type != TaskThreadCreate {
		t.Fatalf("create %+v err=%v", create, err)
	}
	if _, err = s.SettleTaskThread(ctx, now, create, TaskThreadResult{Outcome: TaskThreadSent, ThreadID: "9100"}); err != nil {
		t.Fatal(err)
	}
	line, found, err := s.ClaimTaskThread(ctx, now)
	if err != nil || !found || line.Type != TaskLinePROpened || line.PR != pr || line.Key != "e:"+strconv.FormatInt(threadState(t, s, x.ID).StartEventID, 10) {
		t.Fatalf("first line %+v found=%v err=%v", line, found, err)
	}
}
