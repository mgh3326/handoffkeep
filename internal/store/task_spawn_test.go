package store

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/guard"
)

func spawnTestInput(state string, at time.Time) TaskSpawnInput {
	return TaskSpawnInput{RequestID: "req-1", Job: "job-1", Role: "worker", State: state, Attempt: 1, Host: "m1b", UpdatedAt: at}
}

func TestTaskSpawnLatestWinsAndIgnoresStaleReplays(t *testing.T) {
	s := schemaStore(t, "task_spawn_order")
	ctx := t.Context()
	lane := relaneLane(t, "spawn-order")
	x := relaneTask(t, s, lane, "spawn order")
	base := time.Date(2026, 11, 13, 1, 0, 0, 0, time.UTC)
	put := func(in TaskSpawnInput) (TaskSpawn, bool) {
		t.Helper()
		got, applied, err := s.PutTaskSpawn(ctx, x.ID, in, "svc-panewire")
		if err != nil {
			t.Fatal(err)
		}
		return got, applied
	}
	if got, applied := put(spawnTestInput(SpawnWaiting, base)); !applied || got.State != SpawnWaiting {
		t.Fatalf("first write applied=%t state=%s", applied, got.State)
	}
	if got, applied := put(spawnTestInput(SpawnLaunching, base.Add(time.Minute))); !applied || got.State != SpawnLaunching {
		t.Fatalf("newer write applied=%t state=%s", applied, got.State)
	}
	// An older write and an equal one are ignored; the stored record answers.
	for _, at := range []time.Time{base.Add(30 * time.Second), base.Add(time.Minute)} {
		if got, applied := put(spawnTestInput(SpawnPending, at)); applied || got.State != SpawnLaunching {
			t.Fatalf("stale write at %s applied=%t state=%s", at, applied, got.State)
		}
	}
	y, found, err := s.GetTask(ctx, x.ID)
	if err != nil || !found || y.Spawn == nil || y.Spawn.State != SpawnLaunching {
		t.Fatalf("GetTask spawn=%+v found=%t err=%v", y.Spawn, found, err)
	}
	xs, err := s.ListTasksFiltered(ctx, TaskQuery{Lane: lane, Limit: 10})
	if err != nil || len(xs) != 1 || xs[0].Spawn == nil || xs[0].Spawn.State != SpawnLaunching {
		t.Fatalf("list spawn=%+v err=%v", xs, err)
	}
}

func TestTaskSpawnRefusesBadInput(t *testing.T) {
	s := schemaStore(t, "task_spawn_refuse")
	ctx := t.Context()
	x := relaneTask(t, s, relaneLane(t, "spawn-refuse"), "spawn refuse")
	at := time.Date(2026, 11, 13, 1, 0, 0, 0, time.UTC)
	unknown := spawnTestInput("queued", at)
	if _, _, err := s.PutTaskSpawn(ctx, x.ID, unknown, "svc-panewire"); !errors.Is(err, ErrSpawnInvalid) || !strings.Contains(err.Error(), "state is not a spawn state") {
		t.Fatalf("unknown state err=%v", err)
	}
	if _, _, err := s.PutTaskSpawn(ctx, x.ID+1000000, spawnTestInput(SpawnWaiting, at), "svc-panewire"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing task err=%v", err)
	}
	self := spawnTestInput(SpawnWaiting, at)
	self.StartCondition = &TaskSpawnStartInput{TaskID: x.ID, Until: "merged"}
	if _, _, err := s.PutTaskSpawn(ctx, x.ID, self, "svc-panewire"); !errors.Is(err, ErrSpawnInvalid) {
		t.Fatalf("self start condition err=%v", err)
	}
	badUntil := spawnTestInput(SpawnWaiting, at)
	badUntil.StartCondition = &TaskSpawnStartInput{TaskID: x.ID + 1, Until: "released"}
	if _, _, err := s.PutTaskSpawn(ctx, x.ID, badUntil, "svc-panewire"); !errors.Is(err, ErrSpawnInvalid) {
		t.Fatalf("until err=%v", err)
	}
	// Nothing was stored by a refused write.
	y, _, err := s.GetTask(ctx, x.ID)
	if err != nil || y.Spawn != nil {
		t.Fatalf("spawn after refusals=%+v err=%v", y.Spawn, err)
	}
}

func TestTaskSpawnTerminalCommentWrittenOncePerState(t *testing.T) {
	s := schemaStore(t, "task_spawn_notice")
	ctx := t.Context()
	x := relaneTask(t, s, relaneLane(t, "spawn-notice"), "spawn notice")
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
	put(spawnTestInput(SpawnWaiting, at))
	if n := comments(); n != 0 {
		t.Fatalf("non-terminal write wrote %d comments", n)
	}
	landed := spawnTestInput(SpawnLanded, at.Add(time.Minute))
	put(landed)
	if n := comments(); n != 1 {
		t.Fatalf("landed wrote %d comments, want 1", n)
	}
	// A newer replay of the same landed state writes nothing more.
	put(spawnTestInput(SpawnLanded, at.Add(2*time.Minute)))
	if n := comments(); n != 1 {
		t.Fatalf("replayed landed wrote %d comments, want 1", n)
	}
	// The same request refused is a new terminal state: one more comment.
	put(spawnTestInput(SpawnRefused, at.Add(3*time.Minute)))
	if n := comments(); n != 2 {
		t.Fatalf("refused wrote %d comments, want 2", n)
	}
	// A new attempt that lands is a new key: one more comment.
	retry := spawnTestInput(SpawnLanded, at.Add(4*time.Minute))
	retry.RequestID, retry.Attempt = "req-2", 2
	put(retry)
	if n := comments(); n != 3 {
		t.Fatalf("second attempt landed wrote %d comments, want 3", n)
	}
}

func TestTaskSpawnStartConditionShowsBlockerState(t *testing.T) {
	s := schemaStore(t, "task_spawn_start")
	ctx := t.Context()
	lane := relaneLane(t, "spawn-start")
	blocker := relaneTask(t, s, lane, "blocker")
	queued := relaneTask(t, s, lane, "queued")
	at := time.Date(2026, 11, 13, 1, 0, 0, 0, time.UTC)
	in := spawnTestInput(SpawnWaiting, at)
	in.StartCondition = &TaskSpawnStartInput{TaskID: blocker.ID, Until: "merged"}
	got, _, err := s.PutTaskSpawn(ctx, queued.ID, in, "svc-panewire")
	if err != nil {
		t.Fatal(err)
	}
	if got.StartCondition == nil || got.StartCondition.TaskID != blocker.ID || got.StartCondition.Until != "merged" || got.StartCondition.Met || got.StartCondition.TaskState != blocker.State {
		t.Fatalf("start condition=%+v", got.StartCondition)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET state=$2 WHERE id=$1`, blocker.ID, StateMerged); err != nil {
		t.Fatal(err)
	}
	later := spawnTestInput(SpawnPending, at.Add(time.Minute))
	later.StartCondition = &TaskSpawnStartInput{TaskID: blocker.ID, Until: "merged"}
	got, _, err = s.PutTaskSpawn(ctx, queued.ID, later, "svc-panewire")
	if err != nil {
		t.Fatal(err)
	}
	if got.StartCondition == nil || got.StartCondition.TaskState != StateMerged || !got.StartCondition.Met {
		t.Fatalf("after merge start condition=%+v", got.StartCondition)
	}
}

func TestTaskStatesBlockersAndCondition(t *testing.T) {
	s := schemaStore(t, "task_spawn_states")
	ctx := t.Context()
	lane := relaneLane(t, "spawn-states")
	blocker := relaneTask(t, s, lane, "blocker")
	blocked := relaneTask(t, s, lane, "blocked")
	waiting := relaneTask(t, s, lane, "waiting")
	if _, err := s.AddTaskRelation(ctx, blocker.ID, RelationBlocks, blocked.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 13, 1, 0, 0, 0, time.UTC)
	in := spawnTestInput(SpawnWaiting, at)
	in.StartCondition = &TaskSpawnStartInput{TaskID: blocker.ID, Until: "merged"}
	if _, _, err := s.PutTaskSpawn(ctx, waiting.ID, in, "svc-panewire"); err != nil {
		t.Fatal(err)
	}
	ids := []int64{blocked.ID, waiting.ID, blocked.ID, blocked.ID + 1000000}
	xs, err := s.TaskStates(ctx, ids, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 2 || xs[0].ID != blocked.ID || xs[1].ID != waiting.ID {
		t.Fatalf("entries=%+v", xs)
	}
	if len(xs[0].Blockers) != 1 || xs[0].Blockers[0] != blocker.ID || xs[0].ConditionMet != nil {
		t.Fatalf("blocked entry=%+v", xs[0])
	}
	if xs[0].Category != TaskCategory(blocked.State) {
		t.Fatalf("category=%q", xs[0].Category)
	}
	if xs[1].ConditionMet == nil || *xs[1].ConditionMet || len(xs[1].Blockers) != 1 || xs[1].Blockers[0] != blocker.ID {
		t.Fatalf("waiting entry=%+v", xs[1])
	}
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET state=$2 WHERE id=$1`, blocker.ID, StateMerged); err != nil {
		t.Fatal(err)
	}
	xs, err = s.TaskStates(ctx, ids, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(xs[0].Blockers) != 0 || xs[1].ConditionMet == nil || !*xs[1].ConditionMet || len(xs[1].Blockers) != 0 {
		t.Fatalf("after merge entries=%+v", xs)
	}
	// until=deployed is judged against the deployed level: merged is not enough.
	xs, err = s.TaskStates(ctx, []int64{waiting.ID}, "deployed")
	if err != nil {
		t.Fatal(err)
	}
	if len(xs) != 1 || xs[0].ConditionMet == nil || *xs[0].ConditionMet || len(xs[0].Blockers) != 1 {
		t.Fatalf("deployed level entries=%+v", xs)
	}
	for _, bad := range []struct {
		ids   []int64
		until string
	}{{nil, ""}, {make([]int64, TaskStatesMax+1), ""}, {[]int64{blocked.ID}, "released"}} {
		if _, err := s.TaskStates(ctx, bad.ids, bad.until); !errors.Is(err, ErrSpawnInvalid) {
			t.Fatalf("TaskStates(%d ids, %q) err=%v", len(bad.ids), bad.until, err)
		}
	}
}

func TestWorkStatusQueuesOnlyRequestsInFlight(t *testing.T) {
	s := schemaStore(t, "task_spawn_queue")
	ctx := t.Context()
	x := relaneTask(t, s, relaneLane(t, "spawn-queue"), "spawn queue")
	at := time.Date(2026, 11, 13, 1, 0, 0, 0, time.UTC)
	queued := func() *WorkStatusQueuedTask {
		t.Helper()
		ws, err := s.ReadWorkStatus(ctx, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		for i := range ws.Queued {
			if ws.Queued[i].ID == x.ID {
				return &ws.Queued[i]
			}
		}
		return nil
	}
	if q := queued(); q != nil {
		t.Fatalf("task with no spawn request is queued: %+v", q)
	}
	if _, _, err := s.PutTaskSpawn(ctx, x.ID, spawnTestInput(SpawnPending, at), "svc-panewire"); err != nil {
		t.Fatal(err)
	}
	if q := queued(); q == nil || q.Spawn.State != SpawnPending {
		t.Fatalf("pending request not queued: %+v", q)
	}
	if _, _, err := s.PutTaskSpawn(ctx, x.ID, spawnTestInput(SpawnLanded, at.Add(time.Minute)), "svc-panewire"); err != nil {
		t.Fatal(err)
	}
	if q := queued(); q != nil {
		t.Fatalf("landed request still queued: %+v", q)
	}
}

// checkTaskSpawnInput is pure: no database needed. It refuses values the
// table or the ordering cannot hold: an attempt past int32, an updated_at far
// ahead of the clock (it would win every later comparison), and secret-like
// text in any field a notice comment carries.
func TestCheckTaskSpawnInputRefusesUnboundedValues(t *testing.T) {
	now := time.Now().UTC()
	ok := spawnTestInput(SpawnWaiting, now)
	if err := checkTaskSpawnInput(ok); err != nil {
		t.Fatalf("valid input refused: %v", err)
	}
	tooBig := ok
	tooBig.Attempt = math.MaxInt32 + 1
	if err := checkTaskSpawnInput(tooBig); !errors.Is(err, ErrSpawnInvalid) {
		t.Fatalf("attempt past int32 err=%v", err)
	}
	far := ok
	far.UpdatedAt = now.Add(48 * time.Hour)
	if err := checkTaskSpawnInput(far); !errors.Is(err, ErrSpawnInvalid) {
		t.Fatalf("far-future updated_at err=%v", err)
	}
	near := ok
	near.UpdatedAt = now.Add(10 * time.Minute)
	if err := checkTaskSpawnInput(near); err != nil {
		t.Fatalf("near-future updated_at refused: %v", err)
	}
	secret := ok
	secret.Job = "ghp_" + strings.Repeat("a", 36)
	if p, ok := guard.SecretPattern(checkTaskSpawnInput(secret)); !ok || p != "github_token" {
		t.Fatalf("secret in job not refused as a secret: pattern=%q ok=%t", p, ok)
	}
}
