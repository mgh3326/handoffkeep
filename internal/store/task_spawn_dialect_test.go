package store

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Dialect-neutral cases of the task spawn records (hk 5074, porter A5): they run
// on PostgreSQL in TestTasksAreaPostgres and on MySQL in TestMySQLTasksArea and
// use only public store methods.

func init() {
	registerTaskAreaCase("spawn/latest-wins-and-stale-ignored", spawnCaseLatestWins)
	registerTaskAreaCase("spawn/refusals", spawnCaseRefusals)
	registerTaskAreaCase("spawn/terminal-notice-once-per-key", spawnCaseTerminalNotice)
	registerTaskAreaCase("spawn/start-condition-and-states", spawnCaseStartConditionAndStates)
	registerTaskAreaCase("spawn/concurrent-writes", spawnCaseConcurrentWrites)
}

var a5Seq atomic.Int64

// a5Name is a name that is unique within the process and passes validName.
func a5Name(prefix string) string { return fmt.Sprintf("%s-%d", prefix, a5Seq.Add(1)) }

// a5Project adds a project to the vocabulary and returns its name.
func a5Project(t *testing.T, s *Store) string {
	t.Helper()
	name := a5Name("a5proj")
	if _, err := s.AddTaskProject(t.Context(), name, "a5-test"); err != nil {
		t.Fatal(err)
	}
	return name
}

// a5Task files a backlog task in project.
func a5Task(t *testing.T, s *Store, project, title string) Task {
	t.Helper()
	x, err := s.CreateTask(t.Context(), Task{Lane: a5Name("a5lane"), Title: title, Kind: "implement", Priority: 5, CreatedBy: "a5-test", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func a5PR(n int) string { return fmt.Sprintf("https://github.com/example/app/pull/%d", n) }

// a5Merge opens a PR for a started task and merges it, so the task ends merged.
func a5Merge(t *testing.T, s *Store, id int64, pr int) {
	t.Helper()
	ctx := t.Context()
	if _, err := s.TransitionTask(ctx, id, StateDeveloping, "a5-test", "start", nil, "started by the a5 test"); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []TaskSourceEvent{
		{Source: SourceMergewakePR, SourceRef: a5PR(pr), TaskID: id},
		{Source: SourceMergewakeMerge, SourceRef: a5PR(pr), TaskID: id},
	} {
		if _, err := s.ApplyTaskSourceEvent(ctx, ev); err != nil {
			t.Fatalf("event %+v: %v", ev, err)
		}
	}
}

func a5SpawnInput(state string, at time.Time) TaskSpawnInput {
	return TaskSpawnInput{RequestID: "req-1", Job: "job-1", Role: "worker", State: state, Attempt: 1, Host: "m1b", UpdatedAt: at}
}

func spawnCaseLatestWins(t *testing.T, s *Store) {
	ctx := t.Context()
	x := a5Task(t, s, a5Project(t, s), "spawn order")
	base := time.Date(2026, 11, 13, 1, 0, 0, 0, time.UTC)
	put := func(in TaskSpawnInput) (TaskSpawn, bool) {
		t.Helper()
		got, applied, err := s.PutTaskSpawn(ctx, x.ID, in, "svc-panewire")
		if err != nil {
			t.Fatal(err)
		}
		return got, applied
	}
	first := a5SpawnInput(SpawnWaiting, base)
	check, expires := base.Add(5*time.Minute), base.Add(time.Hour)
	first.NextCheckAt, first.ExpiresAt, first.Reason = &check, &expires, "queued behind a merge"
	got, applied := put(first)
	if !applied || got.State != SpawnWaiting || got.Reason != first.Reason || got.Host != "m1b" || got.Role != "worker" || got.Attempt != 1 {
		t.Fatalf("first write applied=%t record=%+v", applied, got)
	}
	if got.NextCheckAt == nil || !got.NextCheckAt.Equal(check) || got.ExpiresAt == nil || !got.ExpiresAt.Equal(expires) || !got.UpdatedAt.Equal(base) {
		t.Fatalf("first write times: %+v", got)
	}
	if got, applied := put(a5SpawnInput(SpawnLaunching, base.Add(time.Minute))); !applied || got.State != SpawnLaunching || got.NextCheckAt != nil || got.ExpiresAt != nil {
		t.Fatalf("newer write applied=%t record=%+v", applied, got)
	}
	// An older write and an equal one are ignored; the stored record answers.
	for _, at := range []time.Time{base.Add(30 * time.Second), base.Add(time.Minute)} {
		if got, applied := put(a5SpawnInput(SpawnPending, at)); applied || got.State != SpawnLaunching {
			t.Fatalf("stale write at %s applied=%t state=%s", at, applied, got.State)
		}
	}
	y, found, err := s.GetTask(ctx, x.ID)
	if err != nil || !found || y.Spawn == nil || y.Spawn.State != SpawnLaunching {
		t.Fatalf("GetTask spawn=%+v found=%t err=%v", y.Spawn, found, err)
	}
}

func spawnCaseRefusals(t *testing.T, s *Store) {
	ctx := t.Context()
	project := a5Project(t, s)
	x := a5Task(t, s, project, "spawn refusals")
	at := time.Date(2026, 11, 13, 1, 0, 0, 0, time.UTC)
	if _, _, err := s.PutTaskSpawn(ctx, x.ID+1000000, a5SpawnInput(SpawnWaiting, at), "svc-panewire"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("unknown task err=%v", err)
	}
	if _, _, err := s.PutTaskSpawn(ctx, 0, a5SpawnInput(SpawnWaiting, at), "svc-panewire"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("task 0 err=%v", err)
	}
	if _, _, err := s.PutTaskSpawn(ctx, x.ID, a5SpawnInput(SpawnWaiting, at), ""); !errors.Is(err, ErrTaskCommentInvalid) {
		t.Fatalf("empty author err=%v", err)
	}
	if _, _, err := s.PutTaskSpawn(ctx, x.ID, a5SpawnInput("queued", at), "svc-panewire"); !errors.Is(err, ErrSpawnInvalid) {
		t.Fatalf("unknown state err=%v", err)
	}
	self := a5SpawnInput(SpawnWaiting, at)
	self.StartCondition = &TaskSpawnStartInput{TaskID: x.ID, Until: "merged"}
	if _, _, err := s.PutTaskSpawn(ctx, x.ID, self, "svc-panewire"); !errors.Is(err, ErrSpawnInvalid) {
		t.Fatalf("self condition err=%v", err)
	}
	missing := a5SpawnInput(SpawnWaiting, at)
	missing.StartCondition = &TaskSpawnStartInput{TaskID: x.ID + 1000000, Until: "merged"}
	if _, _, err := s.PutTaskSpawn(ctx, x.ID, missing, "svc-panewire"); !errors.Is(err, ErrSpawnInvalid) {
		t.Fatalf("missing condition task err=%v", err)
	}
	// Nothing above wrote a record: the first valid write still applies.
	if _, applied, err := s.PutTaskSpawn(ctx, x.ID, a5SpawnInput(SpawnWaiting, at), "svc-panewire"); err != nil || !applied {
		t.Fatalf("first valid write applied=%t err=%v", applied, err)
	}
	for _, bad := range []struct {
		ids   []int64
		until string
	}{{nil, ""}, {make([]int64, TaskStatesMax+1), ""}, {[]int64{x.ID}, "released"}, {[]int64{0}, ""}} {
		if _, err := s.TaskStates(ctx, bad.ids, bad.until); !errors.Is(err, ErrSpawnInvalid) {
			t.Fatalf("TaskStates(%d ids, %q) err=%v", len(bad.ids), bad.until, err)
		}
	}
}

func spawnCaseTerminalNotice(t *testing.T, s *Store) {
	ctx := t.Context()
	x := a5Task(t, s, a5Project(t, s), "spawn notice")
	at := time.Date(2026, 11, 13, 1, 0, 0, 0, time.UTC)
	put := func(in TaskSpawnInput) {
		t.Helper()
		if _, _, err := s.PutTaskSpawn(ctx, x.ID, in, "svc-panewire"); err != nil {
			t.Fatal(err)
		}
	}
	comments := func() int {
		t.Helper()
		xs, err := s.ListTaskComments(ctx, x.ID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		return len(xs)
	}
	put(a5SpawnInput(SpawnWaiting, at))
	if n := comments(); n != 0 {
		t.Fatalf("non-terminal write wrote %d comments", n)
	}
	put(a5SpawnInput(SpawnLanded, at.Add(time.Minute)))
	if n := comments(); n != 1 {
		t.Fatalf("landed wrote %d comments, want 1", n)
	}
	xs, err := s.ListTaskComments(ctx, x.ID, 0, 100)
	if err != nil || len(xs) != 1 || xs[0].Author != "svc-panewire" || xs[0].Body != "spawn landed request=req-1 attempt=1 job=job-1" {
		t.Fatalf("notice comment %+v err=%v", xs, err)
	}
	// A newer replay of the same landed state writes nothing more.
	put(a5SpawnInput(SpawnLanded, at.Add(2*time.Minute)))
	if n := comments(); n != 1 {
		t.Fatalf("replayed landed wrote %d comments, want 1", n)
	}
	// A stale replay of a terminal state applies nothing, so it comments nothing.
	put(a5SpawnInput(SpawnRefused, at.Add(time.Second)))
	if n := comments(); n != 1 {
		t.Fatalf("stale refused wrote %d comments, want 1", n)
	}
	// The same request refused is a new terminal state: one more comment.
	refused := a5SpawnInput(SpawnRefused, at.Add(3*time.Minute))
	refused.Reason = "no capacity"
	put(refused)
	if n := comments(); n != 2 {
		t.Fatalf("refused wrote %d comments, want 2", n)
	}
	// A new attempt that lands is a new key: one more comment.
	retry := a5SpawnInput(SpawnLanded, at.Add(4*time.Minute))
	retry.RequestID, retry.Attempt = "req-2", 2
	put(retry)
	if n := comments(); n != 3 {
		t.Fatalf("second attempt landed wrote %d comments, want 3", n)
	}
	// A secret-like reason is refused before anything is written.
	leak := a5SpawnInput(SpawnGivenUp, at.Add(5*time.Minute))
	leak.Reason = "token ghp_" + strings.Repeat("a", 36)
	if _, _, err := s.PutTaskSpawn(ctx, x.ID, leak, "svc-panewire"); err == nil {
		t.Fatal("a secret-like reason was accepted")
	}
	if n := comments(); n != 3 {
		t.Fatalf("refused write left %d comments, want 3", n)
	}
}

func spawnCaseStartConditionAndStates(t *testing.T, s *Store) {
	ctx := t.Context()
	project := a5Project(t, s)
	blocker := a5Task(t, s, project, "blocker")
	blocked := a5Task(t, s, project, "blocked")
	queued := a5Task(t, s, project, "queued")
	if _, err := s.AddTaskRelation(ctx, blocker.ID, RelationBlocks, blocked.ID, "a5-test"); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 13, 1, 0, 0, 0, time.UTC)
	in := a5SpawnInput(SpawnWaiting, at)
	in.StartCondition = &TaskSpawnStartInput{TaskID: blocker.ID, Until: "merged"}
	got, _, err := s.PutTaskSpawn(ctx, queued.ID, in, "svc-panewire")
	if err != nil {
		t.Fatal(err)
	}
	if got.StartCondition == nil || got.StartCondition.TaskID != blocker.ID || got.StartCondition.Until != "merged" || got.StartCondition.Met || got.StartCondition.TaskState != blocker.State {
		t.Fatalf("start condition=%+v", got.StartCondition)
	}

	ids := []int64{blocked.ID, queued.ID, blocked.ID, blocked.ID + 1000000}
	xs, err := s.TaskStates(ctx, ids, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 2 || xs[0].ID != blocked.ID || xs[1].ID != queued.ID {
		t.Fatalf("entries=%+v", xs)
	}
	if len(xs[0].Blockers) != 1 || xs[0].Blockers[0] != blocker.ID || xs[0].ConditionMet != nil || xs[0].Category != TaskCategory(blocked.State) {
		t.Fatalf("blocked entry=%+v", xs[0])
	}
	if xs[1].ConditionMet == nil || *xs[1].ConditionMet || len(xs[1].Blockers) != 1 || xs[1].Blockers[0] != blocker.ID {
		t.Fatalf("queued entry=%+v", xs[1])
	}
	snap, err := s.TaskStatesSnapshot(ctx, ids, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Tasks) != 2 || len(snap.Missing) != 1 || snap.Missing[0] != blocked.ID+1000000 || snap.AsOf.IsZero() || time.Since(snap.AsOf).Abs() > time.Hour {
		t.Fatalf("snapshot=%+v", snap)
	}

	a5Merge(t, s, blocker.ID, 5001)
	later := a5SpawnInput(SpawnPending, at.Add(time.Minute))
	later.StartCondition = &TaskSpawnStartInput{TaskID: blocker.ID, Until: "merged"}
	if got, _, err = s.PutTaskSpawn(ctx, queued.ID, later, "svc-panewire"); err != nil {
		t.Fatal(err)
	}
	if got.StartCondition == nil || got.StartCondition.TaskState != StateMerged || !got.StartCondition.Met {
		t.Fatalf("after merge start condition=%+v", got.StartCondition)
	}
	xs, err = s.TaskStates(ctx, ids, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(xs[0].Blockers) != 0 || xs[1].ConditionMet == nil || !*xs[1].ConditionMet || len(xs[1].Blockers) != 0 {
		t.Fatalf("after merge entries=%+v", xs)
	}
	// until=deployed is judged against the deployed level: merged is not enough.
	xs, err = s.TaskStates(ctx, []int64{queued.ID}, "deployed")
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 1 || xs[0].ConditionMet == nil || *xs[0].ConditionMet || len(xs[0].Blockers) != 1 {
		t.Fatalf("deployed level entries=%+v", xs)
	}
}

func spawnCaseConcurrentWrites(t *testing.T, s *Store) {
	ctx := t.Context()
	project := a5Project(t, s)
	base := time.Date(2026, 11, 13, 1, 0, 0, 0, time.UTC)

	// The first write of a task, raced: one write applies, the rest see it.
	first := a5Task(t, s, project, "first write race")
	const writers = 8
	var wg sync.WaitGroup
	results := make(chan bool, writers)
	errs := make(chan error, writers)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, applied, err := s.PutTaskSpawn(ctx, first.ID, a5SpawnInput(SpawnPending, base), "svc-panewire")
			errs <- err
			results <- applied
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	applied := 0
	for ok := range results {
		if ok {
			applied++
		}
	}
	if applied != 1 {
		t.Fatalf("%d of %d equal-time first writes applied, want exactly 1", applied, writers)
	}

	// Distinct times, raced, each landing the same request: the newest record
	// wins and the terminal notice is written once.
	landing := a5Task(t, s, project, "landing race")
	errs = make(chan error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := s.PutTaskSpawn(ctx, landing.ID, a5SpawnInput(SpawnLanded, base.Add(time.Duration(i+1)*time.Minute)), "svc-panewire")
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
	stored, _, err := s.PutTaskSpawn(ctx, landing.ID, a5SpawnInput(SpawnLanded, base), "svc-panewire")
	if err != nil {
		t.Fatal(err)
	}
	if want := base.Add(writers * time.Minute); !stored.UpdatedAt.Equal(want) {
		t.Fatalf("stored updated_at %s, want the newest write %s", stored.UpdatedAt, want)
	}
	xs, err := s.ListTaskComments(ctx, landing.ID, 0, 100)
	if err != nil || len(xs) != 1 {
		t.Fatalf("landing notices %d err=%v, want 1", len(xs), err)
	}
}
