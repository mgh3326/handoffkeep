package store

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Dialect-neutral cases of the Discord task-thread poster's store (hk 5074,
// porter A5). They run on PostgreSQL in TestTasksAreaPostgres and on MySQL in
// TestMySQLTasksArea and use only public store methods.

func init() {
	registerTaskAreaCase("threads/queue-epoch-and-projects", threadsCaseQueue)
	registerTaskAreaCase("threads/claim-settle-recover", threadsCaseClaimSettleRecover)
	registerTaskAreaCase("threads/unknown-create-is-looked-up", threadsCaseUnknownCreate)
	registerTaskAreaCase("threads/attempt-cap-and-ambiguous", threadsCaseAttemptCap)
	registerTaskAreaCase("threads/start-that-is-a-pr-event", threadsCasePRStart)
	registerTaskAreaCase("threads/posted-keys", threadsCasePostedKeys)
	registerTaskAreaCase("threads/concurrent-claims-are-distinct", threadsCaseConcurrentClaims)
}

func a5StartedTask(t *testing.T, s *Store, project, title string) Task {
	t.Helper()
	x := a5Task(t, s, project, title)
	x, err := s.TransitionTask(t.Context(), x.ID, StateDeveloping, "a5-test", "start", nil, "started by the thread test")
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func a5ThreadState(t *testing.T, s *Store, id int64) TaskThreadState {
	t.Helper()
	x, found, err := s.GetTaskThread(t.Context(), id)
	if err != nil || !found {
		t.Fatalf("task %d delivery: found=%v err=%v", id, found, err)
	}
	return x
}

func threadsCaseQueue(t *testing.T, s *Store) {
	ctx := t.Context()
	in, other := a5Project(t, s), a5Project(t, s)
	projects := []string{in}
	before := a5StartedTask(t, s, in, "started before the poster")
	now := time.Now().UTC()
	if n, err := s.QueueTaskThreads(ctx, now, projects); err != nil || n != 0 {
		t.Fatalf("first pass queued %d err=%v", n, err)
	}
	listed := a5StartedTask(t, s, in, "in the list")
	outside := a5StartedTask(t, s, other, "outside the list")
	if n, err := s.QueueTaskThreads(ctx, now, projects); err != nil || n != 1 {
		t.Fatalf("second pass queued %d err=%v", n, err)
	}
	x := a5ThreadState(t, s, listed.ID)
	if x.State != TaskThreadPending || x.ThreadID != nil || x.LastEventID != x.StartEventID-1 || !strings.HasPrefix(x.ThreadName, "#") || len(x.Posted) != 0 || x.ClaimKey != "" {
		t.Fatalf("queued row %+v", x)
	}
	if x.Destination != TaskThreadDestinationDiscord || x.NextAttemptAt.Sub(now).Abs() > time.Millisecond {
		t.Fatalf("queued row destination/next attempt %+v (now %s)", x, now)
	}
	for _, id := range []int64{before.ID, outside.ID} {
		if _, found, err := s.GetTaskThread(ctx, id); err != nil || found {
			t.Fatalf("task %d queued: found=%v err=%v", id, found, err)
		}
	}
	// A pass with nothing new queues nothing, and an empty project list owes none.
	if n, err := s.QueueTaskThreads(ctx, now, projects); err != nil || n != 0 {
		t.Fatalf("idle pass queued %d err=%v", n, err)
	}
	empty := a5StartedTask(t, s, in, "nobody asked")
	if n, err := s.QueueTaskThreads(ctx, now, nil); err != nil || n != 0 {
		t.Fatalf("empty project list queued %d err=%v", n, err)
	}
	if _, found, _ := s.GetTaskThread(ctx, empty.ID); found {
		t.Fatal("a task got a thread with no project listed")
	}
	// The overlap rescans the ids behind the cursor: the task an empty pass
	// skipped is owed once its project is listed, and only once.
	if n, err := s.QueueTaskThreads(ctx, now, projects); err != nil || n != 1 {
		t.Fatalf("overlap pass queued %d err=%v", n, err)
	}
	// A second start of the same task owes nothing more.
	if _, err := s.TransitionTask(ctx, listed.ID, StateUnstarted, "a5-test", "park", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTask(ctx, listed.ID, StateDeveloping, "a5-test", "again", nil, "restarted"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.QueueTaskThreads(ctx, now, projects); err != nil || n != 0 {
		t.Fatalf("restart queued %d err=%v", n, err)
	}
	if got := a5ThreadState(t, s, listed.ID); got.StartEventID != x.StartEventID {
		t.Fatalf("restart moved the start: %+v", got)
	}
}

func threadsCaseClaimSettleRecover(t *testing.T, s *Store) {
	ctx := t.Context()
	project := a5Project(t, s)
	projects := []string{project}
	now := time.Now().UTC()
	if _, err := s.QueueTaskThreads(ctx, now, projects); err != nil {
		t.Fatal(err)
	}
	task := a5StartedTask(t, s, project, "claims")
	if _, err := s.QueueTaskThreads(ctx, now, projects); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.ClaimTaskThread(ctx, now.Add(-time.Hour)); err != nil || found {
		t.Fatalf("claim before the row is due found=%v err=%v", found, err)
	}
	create, found, err := s.ClaimTaskThread(ctx, now)
	if err != nil || !found || create.Type != TaskThreadCreate || create.TaskID != task.ID || create.Project != project || create.Kind != "implement" || create.Key != TaskThreadCreate || create.Lookup || create.Grade != "" || create.Tier != "" {
		t.Fatalf("claim create %+v found=%v err=%v", create, found, err)
	}
	if x := a5ThreadState(t, s, task.ID); x.State != TaskThreadCreating || x.ClaimKey != TaskThreadCreate {
		t.Fatalf("claimed row %+v", x)
	}
	if _, found, _ := s.ClaimTaskThread(ctx, now); found {
		t.Fatal("a claimed create was claimed again")
	}
	// A 500: back to pending with backoff and one attempt.
	if gaveUp, err := s.SettleTaskThread(ctx, now, create, TaskThreadResult{Outcome: TaskThreadRetry, Err: errors.New("discord webhook status 500")}); err != nil || gaveUp {
		t.Fatalf("settle retry gaveUp=%v err=%v", gaveUp, err)
	}
	if x := a5ThreadState(t, s, task.ID); x.State != TaskThreadPending || x.Attempts != 1 || !x.NextAttemptAt.After(now) || x.ClaimKey != "" || !strings.Contains(x.LastError, "500") {
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
	x := a5ThreadState(t, s, task.ID)
	if x.State != TaskThreadActive || x.ThreadID == nil || *x.ThreadID != "9001" || x.Attempts != 0 || x.LastEventID != x.StartEventID-1 || x.LastError != "" {
		t.Fatalf("after create %+v", x)
	}
	// Settling a claim that is no longer held fails and changes nothing.
	if _, err = s.SettleTaskThread(ctx, later, create, TaskThreadResult{Outcome: TaskThreadSent, ThreadID: "9002"}); err == nil {
		t.Fatal("a stale claim settled")
	}
	if got := a5ThreadState(t, s, task.ID); got.ThreadID == nil || *got.ThreadID != "9001" {
		t.Fatalf("a stale settle changed the row: %+v", got)
	}
	if _, err = s.SettleTaskThread(ctx, later, TaskThreadItem{TaskID: task.ID, Destination: TaskThreadDestinationDiscord, Key: TaskThreadCreate, Type: TaskThreadCreate}, TaskThreadResult{Outcome: TaskThreadSent}); err == nil {
		t.Fatal("a create settled without a thread id")
	}

	// A PR line: claimed, then its worker dies mid-send.
	pr := a5PR(301)
	if _, err := s.ApplyTaskSourceEvent(ctx, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: pr, TaskID: task.ID}); err != nil {
		needPorted(t, s, "PR source events (changes, B5)", err)
		t.Fatal(err)
	}
	line, found, err := s.ClaimTaskThread(ctx, later)
	if err != nil || !found || line.Type != TaskLinePROpened || line.PR != pr || line.ThreadID != "9001" || !strings.HasPrefix(line.Key, "e:") || line.ThreadName != x.ThreadName {
		t.Fatalf("claim line %+v found=%v err=%v", line, found, err)
	}
	// Within the lease nothing happens; after it the line counts as posted, so
	// it is never sent twice.
	if got, err := s.RecoverTaskThreadClaims(ctx, later.Add(time.Minute)); err != nil || len(got) != 0 {
		t.Fatalf("recovered inside the lease: %+v err=%v", got, err)
	}
	got, err := s.RecoverTaskThreadClaims(ctx, later.Add(TaskThreadClaimLease+time.Second))
	if err != nil || len(got) != 1 || got[0].Key != line.Key || got[0].TaskID != task.ID || got[0].State != TaskThreadActive {
		t.Fatalf("recovered %+v err=%v", got, err)
	}
	x = a5ThreadState(t, s, task.ID)
	if x.ClaimKey != "" || "e:"+strconv.FormatInt(x.LastEventID, 10) != line.Key || !strings.Contains(x.LastError, "unknown") {
		t.Fatalf("after line recovery %+v", x)
	}
	if extra, found, _ := s.ClaimTaskThread(ctx, later.Add(time.Hour)); found {
		t.Fatalf("recovered line claimed again: %+v", extra)
	}
	if got, err := s.RecoverTaskThreadClaims(ctx, later.Add(time.Hour)); err != nil || len(got) != 0 {
		t.Fatalf("recovered twice: %+v err=%v", got, err)
	}

	// A line that is sent moves the cursor; a refused one stops the row.
	if _, err := s.ApplyTaskSourceEvent(ctx, TaskSourceEvent{Source: SourceMergewakeMerge, SourceRef: pr, TaskID: task.ID}); err != nil {
		t.Fatal(err)
	}
	line, found, err = s.ClaimTaskThread(ctx, later.Add(time.Hour))
	if err != nil || !found || line.Type != TaskLineMerged || line.PR != pr {
		t.Fatalf("merged line %+v found=%v err=%v", line, found, err)
	}
	if gaveUp, err := s.SettleTaskThread(ctx, later.Add(time.Hour), line, TaskThreadResult{Outcome: TaskThreadSent}); err != nil || gaveUp {
		t.Fatalf("settle sent line gaveUp=%v err=%v", gaveUp, err)
	}
	if got := a5ThreadState(t, s, task.ID); got.State != TaskThreadActive || "e:"+strconv.FormatInt(got.LastEventID, 10) != line.Key || got.ClaimKey != "" || got.Attempts != 0 {
		t.Fatalf("after sent line %+v", got)
	}
	if _, err := s.ApplyTaskSourceEvent(ctx, TaskSourceEvent{Source: SourceMergewakeRevert, SourceRef: a5PR(303), TaskID: task.ID, RevertedPR: pr}); err != nil {
		t.Fatal(err)
	}
	line, found, err = s.ClaimTaskThread(ctx, later.Add(time.Hour))
	if err != nil || !found || line.Type != TaskLineReverted || line.PR != a5PR(303) {
		t.Fatalf("reverted line %+v found=%v err=%v", line, found, err)
	}
	if gaveUp, err := s.SettleTaskThread(ctx, later.Add(time.Hour), line, TaskThreadResult{Outcome: TaskThreadRejected, Err: errors.New("discord webhook status 404")}); err != nil || !gaveUp {
		t.Fatalf("settle rejected gaveUp=%v err=%v", gaveUp, err)
	}
	if got := a5ThreadState(t, s, task.ID); got.State != TaskThreadFailed || got.ClaimKey != "" || !strings.Contains(got.LastError, "404") {
		t.Fatalf("after rejection %+v", got)
	}
}

// A create whose outcome is unknown is never created blind: it waits
// TaskThreadLookupDelay, then comes back as a lookup.
func threadsCaseUnknownCreate(t *testing.T, s *Store) {
	ctx := t.Context()
	project := a5Project(t, s)
	projects := []string{project}
	now := time.Now().UTC()
	if _, err := s.QueueTaskThreads(ctx, now, projects); err != nil {
		t.Fatal(err)
	}
	a := a5StartedTask(t, s, project, "timeout on create")
	b := a5StartedTask(t, s, project, "worker died on create")
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
	if x := a5ThreadState(t, s, a.ID); x.State != TaskThreadCreating || x.ClaimKey != "" || x.Attempts != 1 || x.NextAttemptAt.Sub(now.Add(TaskThreadLookupDelay)).Abs() > time.Millisecond || !strings.Contains(x.LastError, "unknown") {
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
	if x := a5ThreadState(t, s, b.ID); x.ClaimKey != "" || x.NextAttemptAt.After(expired.Add(time.Microsecond)) || !strings.Contains(x.LastError, "create outcome unknown") {
		t.Fatalf("after recovery %+v", x)
	}

	// Before the delay the first row is not due; nothing is created blind.
	if x, found, _ := s.ClaimTaskThread(ctx, now.Add(TaskThreadLookupDelay-time.Second)); found {
		t.Fatalf("claimed %+v before the settle delay", x)
	}
	// After it, both come back as lookups. The earlier claim of b above was
	// recovered, so it is the one claimed here too (it is due at once).
	at := now.Add(TaskThreadLookupDelay + 2*time.Second)
	for _, id := range []int64{a.ID, b.ID} {
		if x := a5ThreadState(t, s, id); x.ClaimKey != "" {
			t.Fatalf("task %d still claimed: %+v", id, x)
		}
	}
	lookups := map[int64]TaskThreadItem{}
	for range 2 {
		x, found, err := s.ClaimTaskThread(ctx, at)
		if err != nil || !found || x.Type != TaskThreadCreate || !x.Lookup {
			t.Fatalf("lookup claim %+v found=%v err=%v", x, found, err)
		}
		lookups[x.TaskID] = x
	}
	if len(lookups) != 2 {
		t.Fatalf("lookups %+v", lookups)
	}
	// A rate-limited lookup did not run: the row comes back as a lookup.
	if gaveUp, err := s.SettleTaskThread(ctx, at, lookups[a.ID], TaskThreadResult{Outcome: TaskThreadRetry, RetryAfter: time.Second, Err: errors.New("thread lookup: discord bot status 429")}); err != nil || gaveUp {
		t.Fatalf("settle lookup retry gaveUp=%v err=%v", gaveUp, err)
	}
	if x := a5ThreadState(t, s, a.ID); x.State != TaskThreadCreating || x.ClaimKey != "" || x.Attempts != lookups[a.ID].Attempts {
		t.Fatalf("after lookup retry %+v", x)
	}
	at = at.Add(2 * time.Second)
	again, found, err := s.ClaimTaskThread(ctx, at)
	if err != nil || !found || again.TaskID != a.ID || !again.Lookup {
		t.Fatalf("lookup after retry %+v found=%v err=%v", again, found, err)
	}
	// A lookup that cannot tell stops the row needs_operator; one that finds the
	// thread adopts it.
	if gaveUp, err := s.SettleTaskThread(ctx, at, again, TaskThreadResult{Outcome: TaskThreadAmbiguous, Err: errors.New("2 threads match the lost create")}); err != nil || !gaveUp {
		t.Fatalf("settle ambiguous gaveUp=%v err=%v", gaveUp, err)
	}
	if x := a5ThreadState(t, s, a.ID); x.State != TaskThreadNeedsOperator || x.ThreadID != nil || x.ClaimKey != "" {
		t.Fatalf("after ambiguous %+v", x)
	}
	if _, err := s.SettleTaskThread(ctx, at, lookups[b.ID], TaskThreadResult{Outcome: TaskThreadSent, ThreadID: "9100"}); err != nil {
		t.Fatal(err)
	}
	if x := a5ThreadState(t, s, b.ID); x.State != TaskThreadActive || x.ThreadID == nil || *x.ThreadID != "9100" {
		t.Fatalf("after adopt %+v", x)
	}
	// The needs_operator row is never created again and gets no line.
	if _, err := s.ApplyTaskSourceEvent(ctx, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: a5PR(302), TaskID: a.ID}); err != nil {
		needPorted(t, s, "PR source events (changes, B5)", err)
		t.Fatal(err)
	}
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
		if _, err := s.SettleTaskThread(ctx, now.Add(24*time.Hour), x, TaskThreadResult{Outcome: TaskThreadRejected}); err != nil {
			t.Fatal(err)
		}
	}
}

// A create whose outcome stays unknown stops needs_operator after the attempt
// cap, and only a create can be ambiguous.
func threadsCaseAttemptCap(t *testing.T, s *Store) {
	ctx := t.Context()
	project := a5Project(t, s)
	projects := []string{project}
	now := time.Now().UTC()
	if _, err := s.QueueTaskThreads(ctx, now, projects); err != nil {
		t.Fatal(err)
	}
	task := a5StartedTask(t, s, project, "never answers")
	if _, err := s.QueueTaskThreads(ctx, now, projects); err != nil {
		t.Fatal(err)
	}
	at := now
	for i := 1; i <= taskThreadMaxAttempts; i++ {
		at = at.Add(TaskThreadLookupDelay + time.Second)
		x, found, err := s.ClaimTaskThread(ctx, at)
		if err != nil || !found || x.TaskID != task.ID {
			t.Fatalf("claim %d: %+v found=%v err=%v", i, x, found, err)
		}
		gaveUp, err := s.SettleTaskThread(ctx, at, x, TaskThreadResult{Outcome: TaskThreadUnknown, Err: errors.New("timeout")})
		if err != nil || gaveUp != (i == taskThreadMaxAttempts) {
			t.Fatalf("settle %d gaveUp=%v err=%v", i, gaveUp, err)
		}
	}
	if got := a5ThreadState(t, s, task.ID); got.State != TaskThreadNeedsOperator || got.Attempts != taskThreadMaxAttempts || got.ClaimKey != "" || !strings.Contains(got.LastError, "create outcome unknown") {
		t.Fatalf("after the cap %+v", got)
	}
	if _, found, err := s.ClaimTaskThread(ctx, at.Add(24*time.Hour)); err != nil || found {
		t.Fatalf("a stopped row was claimed: found=%v err=%v", found, err)
	}

	line := a5StartedTask(t, s, project, "line is not a create")
	if _, err := s.QueueTaskThreads(ctx, at, projects); err != nil {
		t.Fatal(err)
	}
	x, found, err := s.ClaimTaskThread(ctx, at)
	if err != nil || !found || x.TaskID != line.ID {
		t.Fatalf("claim %+v found=%v err=%v", x, found, err)
	}
	if _, err := s.SettleTaskThread(ctx, at, x, TaskThreadResult{Outcome: TaskThreadSent, ThreadID: "9300"}); err != nil {
		t.Fatal(err)
	}
	notCreate := TaskThreadItem{TaskID: line.ID, Destination: TaskThreadDestinationDiscord, Key: "e:1", Type: TaskLineMerged}
	if _, err := s.SettleTaskThread(ctx, at, notCreate, TaskThreadResult{Outcome: TaskThreadAmbiguous}); err == nil {
		t.Fatal("a line settled ambiguous")
	}
	if _, err := s.SettleTaskThread(ctx, at, notCreate, TaskThreadResult{Outcome: TaskThreadOutcome(99)}); err == nil {
		t.Fatal("an unknown outcome settled")
	}
}

// Lines of the change, deploy and decision sources are keyed g:, d:, do: and
// da: and are listed in posted, a text array on PostgreSQL and a JSON array on
// MySQL. Those sources belong to other areas, so the claim is forced by a
// dialect-specific fixture and the poster's own paths are exercised on it.
func threadsCasePostedKeys(t *testing.T, s *Store) {
	ctx := t.Context()
	force := a5ForceClaim
	project := a5Project(t, s)
	projects := []string{project}
	now := time.Now().UTC()
	if _, err := s.QueueTaskThreads(ctx, now, projects); err != nil {
		t.Fatal(err)
	}
	task := a5StartedTask(t, s, project, "posted keys")
	if _, err := s.QueueTaskThreads(ctx, now, projects); err != nil {
		t.Fatal(err)
	}
	create, found, err := s.ClaimTaskThread(ctx, now)
	if err != nil || !found {
		t.Fatalf("claim create found=%v err=%v", found, err)
	}
	if _, err := s.SettleTaskThread(ctx, now, create, TaskThreadResult{Outcome: TaskThreadSent, ThreadID: "9400"}); err != nil {
		t.Fatal(err)
	}
	line := func(key string) TaskThreadItem {
		return TaskThreadItem{TaskID: task.ID, Destination: TaskThreadDestinationDiscord, Key: key, Type: TaskLineReview}
	}

	// Sent: the key joins posted.
	force(t, s, task.ID, "g:7", now)
	if gaveUp, err := s.SettleTaskThread(ctx, now, line("g:7"), TaskThreadResult{Outcome: TaskThreadSent}); err != nil || gaveUp {
		t.Fatalf("settle g:7 gaveUp=%v err=%v", gaveUp, err)
	}
	// Unknown: a line counts as posted, so it is never posted twice.
	force(t, s, task.ID, "d:3", now)
	if _, err := s.SettleTaskThread(ctx, now, line("d:3"), TaskThreadResult{Outcome: TaskThreadUnknown, Err: errors.New("timeout")}); err != nil {
		t.Fatal(err)
	}
	// A claim whose worker died: recovery posts it.
	force(t, s, task.ID, "do:11", now)
	got, err := s.RecoverTaskThreadClaims(ctx, now.Add(TaskThreadClaimLease+time.Second))
	if err != nil || len(got) != 1 || got[0].Key != "do:11" || got[0].State != TaskThreadActive {
		t.Fatalf("recovered %+v err=%v", got, err)
	}
	x := a5ThreadState(t, s, task.ID)
	if !slices.Equal(x.Posted, []string{"g:7", "d:3", "do:11"}) || x.ClaimKey != "" || x.LastEventID != x.StartEventID-1 || x.State != TaskThreadActive {
		t.Fatalf("posted %+v", x)
	}
	// A retried line is not posted.
	force(t, s, task.ID, "da:2", now)
	if _, err := s.SettleTaskThread(ctx, now, line("da:2"), TaskThreadResult{Outcome: TaskThreadRetry, Err: errors.New("status 429")}); err != nil {
		t.Fatal(err)
	}
	if x = a5ThreadState(t, s, task.ID); !slices.Equal(x.Posted, []string{"g:7", "d:3", "do:11"}) || x.Attempts != 1 || x.State != TaskThreadActive {
		t.Fatalf("after retried line %+v", x)
	}
}

// Claims run in parallel without sharing a row: every owed create is claimed
// by exactly one caller.
func threadsCaseConcurrentClaims(t *testing.T, s *Store) {
	ctx := t.Context()
	project := a5Project(t, s)
	projects := []string{project}
	now := time.Now().UTC()
	if _, err := s.QueueTaskThreads(ctx, now, projects); err != nil {
		t.Fatal(err)
	}
	const owed = 4
	want := map[int64]bool{}
	for i := range owed {
		want[a5StartedTask(t, s, project, "parallel "+strconv.Itoa(i)).ID] = true
	}
	if n, err := s.QueueTaskThreads(ctx, now, projects); err != nil || n != owed {
		t.Fatalf("queued %d err=%v", n, err)
	}
	const callers = 12
	var mu sync.Mutex
	claimed := map[int64]int{}
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			x, found, err := s.ClaimTaskThread(ctx, now)
			if err == nil && found {
				mu.Lock()
				claimed[x.TaskID]++
				mu.Unlock()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(claimed) != owed {
		t.Fatalf("claimed %v, want each of the %d owed creates", claimed, owed)
	}
	for id, n := range claimed {
		if !want[id] || n != 1 {
			t.Fatalf("task %d claimed %d times (%v)", id, n, claimed)
		}
	}
}

// A task whose start is its PR's event owes a thread, and that same event is
// the thread's first line.
func threadsCasePRStart(t *testing.T, s *Store) {
	ctx := t.Context()
	project := a5Project(t, s)
	projects := []string{project}
	now := time.Now().UTC()
	if _, err := s.QueueTaskThreads(ctx, now, projects); err != nil {
		t.Fatal(err)
	}
	x := a5Task(t, s, project, "started by its PR")
	pr := a5PR(304)
	res, err := s.ApplyTaskSourceEvent(ctx, TaskSourceEvent{Source: SourceMergewakePR, SourceRef: pr, TaskID: x.ID})
	needPorted(t, s, "PR source events (changes, B5)", err)
	if err != nil || !res.Applied || res.State != StateReviewing {
		t.Fatalf("pr event %+v err=%v", res, err)
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
	if err != nil || !found || line.Type != TaskLinePROpened || line.PR != pr || line.Key != "e:"+strconv.FormatInt(a5ThreadState(t, s, x.ID).StartEventID, 10) {
		t.Fatalf("first line %+v found=%v err=%v", line, found, err)
	}
}
