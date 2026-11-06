package store

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// The tests in this file drive two real store calls into the interleaving
// that closes a lock cycle. A third connection (the blocker) holds one lock
// so the first call stops at a known statement; the second call starts only
// once the first is seen blocked by the blocker, and the blocker is released
// only once the second is blocked by one of them too (or has finished). There are
// no sleeps for ordering: every step waits on observed lock state. Each
// actor runs on its own pool whose application_name identifies it, so the
// polling never mistakes another package's waiting backend for an actor.

// lockActor runs one store call in the background.
type lockActor struct {
	app  string
	done chan struct{}
	err  error
}

func startLockActor(app string, f func() error) *lockActor {
	a := &lockActor{app: app, done: make(chan struct{})}
	go func() {
		defer close(a.done)
		a.err = f()
	}()
	return a
}

// waitBlockedBy returns once the actor's backend is blocked by a backend of
// one of the named applications (true) or the actor has finished (false).
// Naming the blockers matters on the shared CI database: the task-mutation
// and migration advisory locks are database-wide, so an actor may first
// queue behind another package's test, which is not the wait the cycle needs.
func waitBlockedBy(t *testing.T, ctx context.Context, probe *pgxpool.Pool, a *lockActor, by ...string) bool {
	t.Helper()
	for {
		select {
		case <-a.done:
			return false
		case <-ctx.Done():
			t.Fatalf("%s: neither blocked nor done: %v", a.app, ctx.Err())
		default:
		}
		var blocked bool
		if err := probe.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity w, pg_stat_activity b WHERE w.application_name=$1 AND b.application_name=ANY($2) AND b.pid=ANY(pg_blocking_pids(w.pid)))`, a.app, by).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return true
		}
		time.Sleep(5 * time.Millisecond) // poll interval, not an ordering sleep
	}
}

// finishLockActors waits for every actor and fails with the full PgError on
// a deadlock, so a CI log names the locks of the cycle.
func finishLockActors(t *testing.T, ctx context.Context, actors ...*lockActor) {
	t.Helper()
	for _, a := range actors {
		select {
		case <-a.done:
		case <-ctx.Done():
			t.Fatalf("%s did not finish: %v", a.app, ctx.Err())
		}
	}
	for _, a := range actors {
		var pgErr *pgconn.PgError
		if errors.As(a.err, &pgErr) && pgErr.Code == "40P01" {
			t.Errorf("%s: deadlock (SQLSTATE %s): %s\nDETAIL: %s\nHINT: %s\nWHERE: %s", a.app, pgErr.Code, pgErr.Message, pgErr.Detail, pgErr.Hint, pgErr.Where)
		} else if a.err != nil {
			t.Errorf("%s: %v", a.app, a.err)
		}
	}
}

// lockOrderSchema returns a throwaway schema URL and a function that names
// an actor's application_name inside it.
func lockOrderSchema(t *testing.T, prefix string) (string, func(string) string) {
	t.Helper()
	schema, dbURL := testdb.Schema(t, prefix)
	return dbURL, func(role string) string { return schema + "_" + role }
}

func withApplicationName(t *testing.T, dbURL, app string) string {
	t.Helper()
	parsed, err := url.Parse(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	q.Set("application_name", app)
	parsed.RawQuery = q.Encode()
	return parsed.String()
}

func openLockActorStore(t *testing.T, dbURL, app string) *Store {
	t.Helper()
	s, err := Open(t.Context(), withApplicationName(t, dbURL, app))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func lockBlocker(t *testing.T, ctx context.Context, dbURL, app string) pgx.Tx {
	t.Helper()
	conn, err := pgx.Connect(ctx, withApplicationName(t, dbURL, app))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func lockOrderTask(t *testing.T, s *Store, refs TaskRefs) Task {
	t.Helper()
	x, err := s.CreateTask(t.Context(), Task{Lane: "lock-order", Title: "lock order", Priority: 1, Kind: "implement", Refs: refs, CreatedBy: "lock-order-test", Project: projectPtr(testProjectName)})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

// TestLockOrderStartupMigrationVersusTaskWrite is the cycle CI hit in
// tests/disposition_test.go: a store opened while another transaction writes
// a task. ApplyDisposition inserts its task_events row (ROW EXCLUSIVE on
// task_events) before it updates the task (ROW EXCLUSIVE on tasks); the
// startup migration takes SHARE on tasks (CREATE INDEX IF NOT EXISTS) before
// SHARE on task_events. A store opened on an up-to-date schema must take
// neither table lock.
func TestLockOrderStartupMigrationVersusTaskWrite(t *testing.T) {
	dbURL, app := lockOrderSchema(t, "lockmig")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	s := openLockActorStore(t, dbURL, app("apply"))

	merged := time.Now().UTC().Add(-time.Hour)
	item, created, err := s.CreateDisposition(ctx, DispositionInput{Lane: "lock-order", Project: testProjectName, Title: "lock order disposition", OriginPR: "https://github.com/example/lock-order/pull/1", MergeSHA: strings.Repeat("a", 40), MergedAt: &merged, Install: DispositionInstall{State: "unknown"}, Recommended: "E", CreatedBy: "lock-order-test"})
	if err != nil || !created {
		t.Fatalf("create disposition created=%v err=%v", created, err)
	}
	gen, err := dispositionGen(ctx, s.pool, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AnswerDisposition(ctx, DispositionAnswerInput{ID: item.ID, Gen: gen, Key: "E", OperatorEmail: "admin@example.com", EventID: "lock-order"}); err != nil {
		t.Fatal(err)
	}
	other := lockOrderTask(t, s, TaskRefs{})

	// The blocker takes the next task_events id for another task, so
	// ApplyDisposition's own insert (the next id) waits on the blocker's
	// transaction after its statement has locked task_events.
	blocker := lockBlocker(t, ctx, dbURL, app("blocker"))
	var next int64
	if err = blocker.QueryRow(ctx, `SELECT nextval(pg_get_serial_sequence('task_events','id'))`).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if _, err = blocker.Exec(ctx, `INSERT INTO task_events(id,task_id,"from","to","by",at) VALUES($1,$2,'backlog','backlog','lock-order-test',now())`, next+1, other.ID); err != nil {
		t.Fatal(err)
	}

	apply := startLockActor(app("apply"), func() error {
		_, err := s.ApplyDisposition(ctx, item.ID, "lock-order-test", "")
		return err
	})
	if !waitBlockedBy(t, ctx, s.pool, apply, app("blocker")) {
		t.Fatalf("ApplyDisposition finished before the blocker released: %v", apply.err)
	}
	openURL := withApplicationName(t, dbURL, app("open"))
	open := startLockActor(app("open"), func() error {
		st, err := Open(ctx, openURL)
		if err == nil {
			st.Close()
		}
		return err
	})
	waitBlockedBy(t, ctx, s.pool, open, app("apply"), app("blocker"))
	if err = blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	finishLockActors(t, ctx, apply, open)
}

// TestLockOrderJobUpdateVersusTaskWrite: UpdateJob with the work board on
// locks the job's task row (recordLinearBoardJobTx) and, for a new role,
// then takes the task-mutation lock (applyJobRoleTx). Every task writer takes
// that lock before the row, so the job update must take it first too.
func TestLockOrderJobUpdateVersusTaskWrite(t *testing.T) {
	dbURL, app := lockOrderSchema(t, "lockjob")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	jobs := openLockActorStore(t, dbURL, app("job"))
	jobs.EnableLinearBoard()
	writer := openLockActorStore(t, dbURL, app("relane"))

	task := lockOrderTask(t, jobs, TaskRefs{})
	const jobID = "lock-order-job"
	if _, _, err := jobs.ClaimJob(ctx, Job{JobID: jobID, TaskID: &task.ID, Host: "lock-host", CreatedBy: "lock-order-test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.ClaimTask(ctx, task.ID, "lead", jobID, ""); err != nil {
		t.Fatal(err)
	}

	blocker := lockBlocker(t, ctx, dbURL, app("blocker"))
	if _, err := blocker.Exec(ctx, `SELECT id FROM tasks WHERE id=$1 FOR UPDATE`, task.ID); err != nil {
		t.Fatal(err)
	}
	role := "worker"
	update := startLockActor(app("job"), func() error {
		_, err := jobs.UpdateJob(ctx, jobID, JobUpdate{Role: &role})
		return err
	})
	if !waitBlockedBy(t, ctx, jobs.pool, update, app("blocker")) {
		t.Fatalf("UpdateJob finished before the blocker released: %v", update.err)
	}
	relane := startLockActor(app("relane"), func() error {
		_, _, err := writer.RelaneTask(ctx, task.ID, "lock-order-2", "lock-order-test", "lock order", true)
		return err
	})
	waitBlockedBy(t, ctx, jobs.pool, relane, app("job"), app("blocker"))
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	finishLockActors(t, ctx, update, relane)
}

// TestLockOrderRelayBoardVersusAutoClose: a relay event with the work board
// on locks every task it matches in id order (a parent before its child),
// while dropping a child under the task-mutation lock locks the child and
// then auto-close locks its parent. The relay must take the task-mutation
// lock before any task row.
func TestLockOrderRelayBoardVersusAutoClose(t *testing.T) {
	dbURL, app := lockOrderSchema(t, "lockrelay")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	relay := openLockActorStore(t, dbURL, app("relay"))
	relay.EnableLinearBoard()
	closer := openLockActorStore(t, dbURL, app("close"))

	const pr = "https://github.com/example/lock-order/pull/7"
	parent := lockOrderTask(t, relay, TaskRefs{PR: pr})
	child := lockOrderTask(t, relay, TaskRefs{PR: pr})
	if _, _, err := relay.SetTaskParent(ctx, child.ID, &parent.ID, "lock-order-test"); err != nil {
		t.Fatal(err)
	}

	blocker := lockBlocker(t, ctx, dbURL, app("blocker"))
	if _, err := blocker.Exec(ctx, `SELECT id FROM tasks WHERE id=$1 FOR UPDATE`, parent.ID); err != nil {
		t.Fatal(err)
	}
	appendRelay := startLockActor(app("relay"), func() error {
		_, _, err := relay.AppendRelayEvent(ctx, RelayEvent{Kind: "lane.event", OwnerLane: "lock-order", EventID: "lock-order-ready", Text: "[lock-order] READY | " + pr, PR: pr})
		return err
	})
	if !waitBlockedBy(t, ctx, relay.pool, appendRelay, app("blocker")) {
		t.Fatalf("AppendRelayEvent finished before the blocker released: %v", appendRelay.err)
	}
	drop := startLockActor(app("close"), func() error {
		_, err := closer.TransitionTask(ctx, child.ID, StateDropped, "lock-order-test", "lock order", nil, "")
		return err
	})
	waitBlockedBy(t, ctx, relay.pool, drop, app("relay"), app("blocker"))
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	finishLockActors(t, ctx, appendRelay, drop)
}

// TestMigrateVersionsMatchFreshSchema keeps migrateCurrent honest: a fresh
// schema records exactly the versions it waits for, so a version added to
// Store.migrate without updating migrateVersions fails here instead of
// making every later start re-run DDL (or skip it).
func TestMigrateVersionsMatchFreshSchema(t *testing.T) {
	_, dbURL := testdb.Schema(t, "lockver")
	s, err := Open(t.Context(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var got []int32
	if err = s.pool.QueryRow(t.Context(), `SELECT array_agg(version ORDER BY version) FROM schema_version`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	want := migrateVersions()
	if len(got) != len(want) {
		t.Fatalf("recorded versions %v, migrateVersions %v", got, want)
	}
	for i := range want {
		if int(got[i]) != want[i] {
			t.Fatalf("recorded versions %v, migrateVersions %v", got, want)
		}
	}
	tx, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if current, err := migrateCurrent(t.Context(), tx, nil); err != nil || !current {
		t.Fatalf("fresh schema current=%v err=%v", current, err)
	}
	if _, err = tx.Exec(t.Context(), `DELETE FROM schema_version WHERE version=$1`, SchemaVersion); err != nil {
		t.Fatal(err)
	}
	if current, err := migrateCurrent(t.Context(), tx, nil); err != nil || current {
		t.Fatalf("schema without v%d current=%v err=%v", SchemaVersion, current, err)
	}
}

func TestRetryDeadlockIsBounded(t *testing.T) {
	deadlock := &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}
	calls := 0
	err := retryDeadlock(t.Context(), 3, func(context.Context) error { calls++; return deadlock })
	if !errors.Is(err, deadlock) || calls != 3 {
		t.Fatalf("always deadlocking: calls=%d err=%v", calls, err)
	}
	calls = 0
	err = retryDeadlock(t.Context(), 3, func(context.Context) error {
		calls++
		if calls == 1 {
			return deadlock
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("deadlock then success: calls=%d err=%v", calls, err)
	}
	other := &pgconn.PgError{Code: "40001"}
	calls = 0
	err = retryDeadlock(t.Context(), 3, func(context.Context) error { calls++; return other })
	if !errors.Is(err, other) || calls != 1 {
		t.Fatalf("other error retried: calls=%d err=%v", calls, err)
	}
}
