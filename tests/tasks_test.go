package tests

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/store"
)

func taskTestStore(t *testing.T) *store.Store {
	t.Helper()
	url := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if url == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for PostgreSQL task tests")
	}
	s, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func taskLane(t *testing.T) string { return fmt.Sprintf("task-%d", time.Now().UnixNano()) }

func newTask(t *testing.T, s *store.Store, lane, title string, priority int) store.Task {
	t.Helper()
	x, err := s.CreateTask(t.Context(), store.Task{Lane: lane, Title: title, Kind: "implement", Priority: priority, CreatedBy: "test-node"})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func taskHTTP(s *store.Store) *httptest.Server {
	return httptest.NewServer(api.Server{Service: api.Service{Store: s}, Tokens: api.Tokens{"node": "node-token", "node2": "node2-token"}}.Handler())
}

func TestTaskConcurrentClaimHasExactlyOneWinner(t *testing.T) {
	s := taskTestStore(t)
	task := newTask(t, s, taskLane(t), "claim once", 0)
	h := taskHTTP(s)
	defer h.Close()
	clients := []remote.Client{{URL: h.URL, Token: "node-token", HTTP: h.Client()}, {URL: h.URL, Token: "node2-token", HTTP: h.Client()}}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, conflicts := 0, 0
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := clients[i%len(clients)].ClaimTask(t.Context(), task.ID, "captain-"+string(rune('a'+i%26)))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
			} else if err.Error() == "task_conflict" {
				conflicts++
			} else {
				t.Errorf("claim: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 || conflicts != 31 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
	got, found, err := s.GetTask(t.Context(), task.ID)
	if err != nil || !found || got.State != "claimed" || len(got.Events) != 1 {
		t.Fatalf("task=%+v found=%v err=%v", got, found, err)
	}
}

func TestTaskIllegalTransitionReturnsConflict(t *testing.T) {
	s := taskTestStore(t)
	task := newTask(t, s, taskLane(t), "must be claimed first", 0)
	h := taskHTTP(s)
	defer h.Close()
	resp := request(t, h.Client(), http.MethodPost, h.URL+"/v1/tasks/"+fmt.Sprint(task.ID)+"/transition", "node-token", map[string]any{"to": "merged"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

func TestTaskEveryTransitionWritesOneEvent(t *testing.T) {
	s := taskTestStore(t)
	task := newTask(t, s, taskLane(t), "event audit", 0)
	if _, err := s.ClaimTask(t.Context(), task.ID, "captain-a"); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"in_progress", "verifying", "in_progress", "verifying", "merged"} {
		if _, err := s.TransitionTask(t.Context(), task.ID, to, "node", "", nil); err != nil {
			t.Fatalf("to %s: %v", to, err)
		}
	}
	got, found, err := s.GetTask(t.Context(), task.ID)
	if err != nil || !found || got.State != "merged" || len(got.Events) != 6 {
		t.Fatalf("task=%+v found=%v err=%v", got, found, err)
	}
	for i, event := range got.Events {
		if event.TaskID != task.ID || event.From == "" || event.To == "" || event.By == "" {
			t.Fatalf("event[%d]=%+v", i, event)
		}
	}
}

func TestTaskNextClaimsPriorityThenCreationOrder(t *testing.T) {
	s := taskTestStore(t)
	lane := taskLane(t)
	low := newTask(t, s, lane, "low", 1)
	highFirst := newTask(t, s, lane, "high first", 9)
	highSecond := newTask(t, s, lane, "high second", 9)
	for _, want := range []int64{highFirst.ID, highSecond.ID, low.ID} {
		got, err := s.NextTask(t.Context(), lane, "captain-a")
		if err != nil || got.ID != want || got.State != "claimed" {
			t.Fatalf("got=%+v err=%v want=%d", got, err, want)
		}
	}
	if _, err := s.NextTask(t.Context(), lane, "captain-a"); !errors.Is(err, store.ErrQueueEmpty) {
		t.Fatalf("empty next err=%v", err)
	}
}

func TestTaskNextEmptyIsExplicitQueueEmptyAndCLIExitThree(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	lane := taskLane(t)
	resp := request(t, h.Client(), http.MethodPost, h.URL+"/v1/tasks/next", "node-token", map[string]string{"lane": lane, "claimed_by": "captain"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body["error"] != "queue_empty" {
		t.Fatalf("body=%v err=%v", body, err)
	}
	client := remote.Client{URL: h.URL, Token: "node-token", HTTP: h.Client()}
	if _, err := client.NextTask(t.Context(), lane, "captain"); err == nil || err.Error() != "queue_empty" {
		t.Fatalf("remote error=%v", err)
	}
}

func TestTaskTransitionGraphAndRefsSnapshots(t *testing.T) {
	s := taskTestStore(t)
	// join -> hold is forbidden; needs_decision -> claimed is the canonical
	// decision-resume path (backlog remains an allowed alternative).
	join := newTask(t, s, taskLane(t), "joined", 0)
	if _, err := s.ClaimTask(t.Context(), join.ID, "captain"); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"in_progress", "join"} {
		if _, err := s.TransitionTask(t.Context(), join.ID, to, "node", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.TransitionTask(t.Context(), join.ID, "hold", "node", "", nil); !errors.Is(err, store.ErrTaskConflict) {
		t.Fatalf("join->hold error=%v", err)
	}
	decision := newTask(t, s, taskLane(t), "decision", 0)
	if _, err := s.ClaimTask(t.Context(), decision.ID, "captain"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTask(t.Context(), decision.ID, "needs_decision", "node", "choose", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTask(t.Context(), decision.ID, "claimed", "node", "resolved", nil); err != nil {
		t.Fatalf("needs_decision->claimed: %v", err)
	}

	refs := store.TaskRefs{PR: "4", HeadSHA: "abc123", ReportPath: "report.md", JobID: "job-4"}
	task, err := s.CreateTask(t.Context(), store.Task{Lane: taskLane(t), Title: "preserve refs", Kind: "implement", Refs: refs, CreatedBy: "node"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimTask(t.Context(), task.ID, "captain"); err != nil {
		t.Fatal(err)
	}
	got, err := s.TransitionTask(t.Context(), task.ID, "in_progress", "node", "started", &store.TaskRefs{PR: "5"})
	if err != nil {
		t.Fatal(err)
	}
	want := store.TaskRefs{PR: "5", HeadSHA: "abc123", ReportPath: "report.md", JobID: "job-4"}
	if !reflect.DeepEqual(got.Refs, want) {
		t.Fatalf("refs=%+v want=%+v", got.Refs, want)
	}
	shown, found, err := s.GetTask(t.Context(), task.ID)
	if err != nil || !found || len(shown.Events) != 2 || shown.Events[1].Refs == nil || !reflect.DeepEqual(*shown.Events[1].Refs, want) {
		t.Fatalf("show=%+v found=%v err=%v", shown, found, err)
	}
}

func TestTaskRejectsSecretNoteQuestionAndRefsWithoutStorage(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	secret := "sk-abcdefghijklmnopqrstuvwxyz"
	task := newTask(t, s, taskLane(t), "secret test", 0)
	if _, err := s.ClaimTask(t.Context(), task.ID, "captain"); err != nil {
		t.Fatal(err)
	}
	for _, body := range []map[string]any{
		{"to": "in_progress", "note": secret},
		{"to": "in_progress", "refs": map[string]string{"pr": secret}},
		{"to": "needs_decision", "note": secret},
	} {
		resp := request(t, h.Client(), http.MethodPost, h.URL+"/v1/tasks/"+fmt.Sprint(task.ID)+"/transition", "node-token", body)
		if resp.StatusCode != http.StatusBadRequest {
			resp.Body.Close()
			t.Fatalf("body=%v status=%d", body, resp.StatusCode)
		}
		resp.Body.Close()
	}
	got, found, err := s.GetTask(t.Context(), task.ID)
	if err != nil || !found || got.State != "claimed" || len(got.Events) != 1 {
		t.Fatalf("task=%+v found=%v err=%v", got, found, err)
	}
}

func TestTaskEventsDatabaseAppendOnly(t *testing.T) {
	s := taskTestStore(t)
	task := newTask(t, s, taskLane(t), "append only", 0)
	if _, err := s.ClaimTask(t.Context(), task.ID, "captain"); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.GetTask(t.Context(), task.ID)
	if err != nil || !found || len(got.Events) != 1 {
		t.Fatalf("task=%+v found=%v err=%v", got, found, err)
	}
	db, err := pgx.Connect(t.Context(), os.Getenv("HANDOFFKEEP_TEST_DB_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(t.Context())
	for _, sql := range []string{
		"UPDATE task_events SET note='mutated' WHERE id=$1",
		"DELETE FROM task_events WHERE id=$1",
	} {
		if _, err := db.Exec(t.Context(), sql, got.Events[0].ID); err == nil {
			t.Fatalf("append-only mutation succeeded: %s", sql)
		}
	}
}
