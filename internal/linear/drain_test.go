package linear

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/store"
)

func isolatedLinearStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	baseURL := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if baseURL == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for Linear drain tests")
	}
	admin, err := pgx.Connect(t.Context(), baseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	schema := fmt.Sprintf("linear_drain_%d", time.Now().UnixNano())
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema+",public")
	parsed.RawQuery = query.Encode()
	scopedURL := parsed.String()
	st, err := store.Open(t.Context(), scopedURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st, scopedURL
}

func createDrainTask(t *testing.T, st *store.Store, refs store.TaskRefs) store.Task {
	t.Helper()
	task, err := st.CreateTask(t.Context(), store.Task{
		Lane:      "builder-lane",
		Title:     "Mirror connector contract",
		Kind:      "implement",
		Refs:      refs,
		CreatedBy: "fixture-client",
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func seedOutbox(t *testing.T, scopedURL string, operations ...struct {
	op      string
	payload string
}) store.Task {
	t.Helper()
	st, err := store.Open(t.Context(), scopedURL)
	if err != nil {
		t.Fatal(err)
	}
	task := createDrainTask(t, st, store.TaskRefs{})
	st.Close()
	connection, err := pgx.Connect(t.Context(), scopedURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	if _, err := connection.Exec(t.Context(), `INSERT INTO linear_issues(task_id,issue_id,identifier,created_at,updated_at) VALUES($1,'issue-1','ENG-101',now(),now())`, task.ID); err != nil {
		t.Fatal(err)
	}
	for index, operation := range operations {
		if _, err := connection.Exec(t.Context(), `INSERT INTO linear_outbox(task_id,seq,op,payload,next_attempt_at,created_at,updated_at) VALUES($1,$2,$3,$4::jsonb,now(),now(),now())`, task.ID, index+1, operation.op, operation.payload); err != nil {
			t.Fatal(err)
		}
	}
	return task
}

func startTestDrain(t *testing.T, st *store.Store, client *Client, retry time.Duration) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	drain := &Drain{
		Store:        st,
		Client:       client,
		PollInterval: 5 * time.Millisecond,
		Jitter:       func(time.Duration) time.Duration { return retry },
	}
	go func() {
		defer close(done)
		drain.Run(ctx)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("drain did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func waitOutbox(t *testing.T, st *store.Store, taskID int64, predicate func([]store.LinearOutbox) bool) []store.LinearOutbox {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := st.ListLinearOutbox(t.Context(), taskID)
		if err != nil {
			t.Fatal(err)
		}
		if predicate(rows) {
			return rows
		}
		time.Sleep(5 * time.Millisecond)
	}
	rows, err := st.ListLinearOutbox(t.Context(), taskID)
	t.Fatalf("outbox condition timed out: rows=%+v err=%v", rows, err)
	return nil
}

func TestLinearMarkersHaveExactBoundaries(t *testing.T) {
	if strings.Contains(markerForTask(10), markerForTask(1)) {
		t.Fatalf("task marker %q aliases %q", markerForTask(10), markerForTask(1))
	}
	first := markerForOutbox(store.LinearOutbox{TaskID: 1, Seq: 1})
	tenth := markerForOutbox(store.LinearOutbox{TaskID: 1, Seq: 10})
	if strings.Contains(tenth, first) {
		t.Fatalf("outbox marker %q aliases %q", tenth, first)
	}
}

func TestLinearIssueCreateAmbiguousRestartAdoptsMarker(t *testing.T) {
	st, _ := isolatedLinearStore(t)
	st.EnableLinearSync()
	fake := newFakeLinear(t)
	fake.failures["HKIssueCreate"] = []string{"drop"}
	task := createDrainTask(t, st, store.TaskRefs{Linear: &store.TaskLinear{Sync: true, Tier: "T3", Grade: "S+", Brief: "brief/connector"}})
	stopFirst := startTestDrain(t, st, fake.client(t), 400*time.Millisecond)
	waitOutbox(t, st, task.ID, func(rows []store.LinearOutbox) bool {
		return len(rows) == 1 && rows[0].State == "pending" && rows[0].Attempts == 1
	})
	stopFirst()
	stopSecond := startTestDrain(t, st, fake.client(t), 5*time.Millisecond)
	rows := waitOutbox(t, st, task.ID, func(rows []store.LinearOutbox) bool {
		return len(rows) == 1 && rows[0].State == "skipped" && rows[0].Attempts == 2
	})
	stopSecond()
	if fake.count("HKIssueCreate") != 1 || fake.effect("HKIssueCreate") != 1 {
		t.Fatalf("issueCreate calls=%d effects=%d rows=%+v", fake.count("HKIssueCreate"), fake.effect("HKIssueCreate"), rows)
	}
	if _, found, err := st.GetLinearIssue(t.Context(), task.ID); err != nil || !found {
		t.Fatalf("adopted issue found=%t err=%v", found, err)
	}
}

func TestLinearTerminalAmbiguousRestartsSuppressDuplicates(t *testing.T) {
	st, scopedURL := isolatedLinearStore(t)
	fake := newFakeLinear(t)
	fake.issueExists = true
	fake.failures["HKCommentCreate"] = []string{"drop"}
	fake.failures["HKIssueArchive"] = []string{"drop"}
	task := seedOutbox(t, scopedURL,
		struct {
			op      string
			payload string
		}{store.LinearOpTerminalComment, `{"comment":"Terminal summary"}`},
		struct {
			op      string
			payload string
		}{store.LinearOpTerminalArchive, `{}`},
	)

	stopFirst := startTestDrain(t, st, fake.client(t), 300*time.Millisecond)
	waitOutbox(t, st, task.ID, func(rows []store.LinearOutbox) bool {
		return len(rows) == 2 && rows[0].State == "pending" && rows[0].Attempts == 1
	})
	stopFirst()
	stopSecond := startTestDrain(t, st, fake.client(t), 300*time.Millisecond)
	waitOutbox(t, st, task.ID, func(rows []store.LinearOutbox) bool {
		return len(rows) == 2 && rows[0].State == "skipped" && rows[1].State == "pending" && rows[1].Attempts == 1
	})
	stopSecond()
	stopThird := startTestDrain(t, st, fake.client(t), 5*time.Millisecond)
	rows := waitOutbox(t, st, task.ID, func(rows []store.LinearOutbox) bool {
		return len(rows) == 2 && rows[0].State == "skipped" && rows[1].State == "skipped"
	})
	stopThird()
	if fake.count("HKCommentCreate") != 1 || fake.effect("HKCommentCreate") != 1 {
		t.Fatalf("comment calls=%d effects=%d rows=%+v", fake.count("HKCommentCreate"), fake.effect("HKCommentCreate"), rows)
	}
	if fake.count("HKIssueArchive") != 1 || fake.effect("HKIssueArchive") != 1 {
		t.Fatalf("archive calls=%d effects=%d rows=%+v", fake.count("HKIssueArchive"), fake.effect("HKIssueArchive"), rows)
	}
}

func TestLinearTerminalPartialFailureDoesNotDuplicateOtherOperation(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		operation string
		failedRow int
	}{
		{name: "comment_response_lost", operation: "HKCommentCreate", failedRow: 0},
		{name: "archive_response_lost", operation: "HKIssueArchive", failedRow: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			st, scopedURL := isolatedLinearStore(t)
			fake := newFakeLinear(t)
			fake.issueExists = true
			fake.failures[scenario.operation] = []string{"drop"}
			task := seedOutbox(t, scopedURL,
				struct {
					op      string
					payload string
				}{store.LinearOpTerminalComment, `{"comment":"Terminal summary"}`},
				struct {
					op      string
					payload string
				}{store.LinearOpTerminalArchive, `{}`},
			)

			stopFirst := startTestDrain(t, st, fake.client(t), 300*time.Millisecond)
			waitOutbox(t, st, task.ID, func(rows []store.LinearOutbox) bool {
				return len(rows) == 2 && rows[scenario.failedRow].State == "pending" && rows[scenario.failedRow].Attempts == 1
			})
			stopFirst()
			stopSecond := startTestDrain(t, st, fake.client(t), 5*time.Millisecond)
			rows := waitOutbox(t, st, task.ID, func(rows []store.LinearOutbox) bool {
				return len(rows) == 2 &&
					(rows[0].State == "sent" || rows[0].State == "skipped") &&
					(rows[1].State == "sent" || rows[1].State == "skipped")
			})
			stopSecond()
			if fake.count("HKCommentCreate") != 1 || fake.effect("HKCommentCreate") != 1 {
				t.Fatalf("comment calls=%d effects=%d rows=%+v", fake.count("HKCommentCreate"), fake.effect("HKCommentCreate"), rows)
			}
			if fake.count("HKIssueArchive") != 1 || fake.effect("HKIssueArchive") != 1 {
				t.Fatalf("archive calls=%d effects=%d rows=%+v", fake.count("HKIssueArchive"), fake.effect("HKIssueArchive"), rows)
			}
		})
	}
}

func TestLinearIssueStateRestartIsIdempotent(t *testing.T) {
	st, scopedURL := isolatedLinearStore(t)
	fake := newFakeLinear(t)
	fake.issueExists = true
	task := seedOutbox(t, scopedURL, struct {
		op      string
		payload string
	}{store.LinearOpIssueState, `{"state":"In Progress"}`})
	stopFirst := startTestDrain(t, st, fake.client(t), 5*time.Millisecond)
	waitOutbox(t, st, task.ID, func(rows []store.LinearOutbox) bool {
		return len(rows) == 1 && rows[0].State == "sent"
	})
	stopFirst()
	stopSecond := startTestDrain(t, st, fake.client(t), 5*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	stopSecond()
	if fake.count("HKIssueUpdate") != 1 || fake.effect("HKIssueUpdate") != 1 {
		t.Fatalf("issueUpdate calls=%d effects=%d", fake.count("HKIssueUpdate"), fake.effect("HKIssueUpdate"))
	}
}

func TestLinearDrainAdvisoryLockSingleOwnerAndRelease(t *testing.T) {
	st, scopedURL := isolatedLinearStore(t)
	st.EnableLinearSync()
	fake := newFakeLinear(t)
	stopFirst := startTestDrain(t, st, fake.client(t), 5*time.Millisecond)
	stopSecond := startTestDrain(t, st, fake.client(t), 5*time.Millisecond)
	connection, err := pgx.Connect(t.Context(), scopedURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	deadline := time.Now().Add(2 * time.Second)
	for {
		var holders int
		if err := connection.QueryRow(t.Context(), `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND granted AND classid=0 AND objid=$1`, store.LinearDrainAdvisoryLock).Scan(&holders); err != nil {
			t.Fatal(err)
		}
		if holders == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("advisory lock holders=%d", holders)
		}
		time.Sleep(5 * time.Millisecond)
	}
	task := createDrainTask(t, st, store.TaskRefs{Linear: &store.TaskLinear{Sync: true}})
	waitOutbox(t, st, task.ID, func(rows []store.LinearOutbox) bool {
		return len(rows) == 1 && (rows[0].State == "sent" || rows[0].State == "skipped")
	})
	if fake.count("HKIssueCreate") != 1 {
		t.Fatalf("issueCreate calls=%d", fake.count("HKIssueCreate"))
	}
	stopFirst()
	stopSecond()
	var acquired bool
	if err := connection.QueryRow(t.Context(), `SELECT pg_try_advisory_lock($1)`, store.LinearDrainAdvisoryLock).Scan(&acquired); err != nil || !acquired {
		t.Fatalf("reacquire=%t err=%v", acquired, err)
	}
	if _, err := connection.Exec(t.Context(), `SELECT pg_advisory_unlock($1)`, store.LinearDrainAdvisoryLock); err != nil {
		t.Fatal(err)
	}
}

func TestLinearFailureClassificationAndMissingLabels(t *testing.T) {
	t.Run("permanent", func(t *testing.T) {
		st, _ := isolatedLinearStore(t)
		st.EnableLinearSync()
		fake := newFakeLinear(t)
		fake.alwaysFailure["HKIssueSearch"] = "401"
		task := createDrainTask(t, st, store.TaskRefs{Linear: &store.TaskLinear{Sync: true}})
		stop := startTestDrain(t, st, fake.client(t), 5*time.Millisecond)
		rows := waitOutbox(t, st, task.ID, func(rows []store.LinearOutbox) bool {
			return len(rows) == 1 && rows[0].State == "failed"
		})
		stop()
		if rows[0].Attempts != 1 || rows[0].LastError == "" {
			t.Fatalf("permanent row=%+v", rows[0])
		}
	})
	t.Run("transient", func(t *testing.T) {
		st, _ := isolatedLinearStore(t)
		st.EnableLinearSync()
		fake := newFakeLinear(t)
		fake.alwaysFailure["HKIssueSearch"] = "503"
		task := createDrainTask(t, st, store.TaskRefs{Linear: &store.TaskLinear{Sync: true}})
		stop := startTestDrain(t, st, fake.client(t), 20*time.Millisecond)
		rows := waitOutbox(t, st, task.ID, func(rows []store.LinearOutbox) bool {
			return len(rows) == 1 && rows[0].State == "pending" && rows[0].Attempts >= 2
		})
		stop()
		if rows[0].LastError == "" {
			t.Fatalf("transient row=%+v", rows[0])
		}
	})
	t.Run("missing_labels", func(t *testing.T) {
		st, _ := isolatedLinearStore(t)
		st.EnableLinearSync()
		fake := newFakeLinear(t)
		task := createDrainTask(t, st, store.TaskRefs{Linear: &store.TaskLinear{
			Sync: true, Tier: "T3", Grade: "S+", Labels: []string{"connector", "missing-label"},
		}})
		stop := startTestDrain(t, st, fake.client(t), 5*time.Millisecond)
		rows := waitOutbox(t, st, task.ID, func(rows []store.LinearOutbox) bool {
			return len(rows) == 1 && rows[0].State == "sent"
		})
		stop()
		if !strings.Contains(rows[0].LastError, "missing-label") {
			t.Fatalf("missing label was not reported: %+v", rows[0])
		}
		variables := fake.variables("HKIssueCreate")
		input, _ := variables["input"].(map[string]any)
		for _, value := range input["labelIds"].([]any) {
			if value == "missing-label" {
				t.Fatal("missing label was sent as an ID")
			}
		}
	})
}
