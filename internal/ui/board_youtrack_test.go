package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
	"github.com/mgh3326/handoffkeep/internal/youtrack"
)

type uiYouTrack struct {
	mu      sync.Mutex
	updates int
	fail    bool
}

func (f *uiYouTrack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/issues/") {
		if f.fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"unavailable"}`))
			return
		}
		f.updates++
		_, _ = w.Write([]byte(`{"id":"9-1"}`))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"id":"9-1","customFields":[]}`))
}

func uiYouTrackHandler(t *testing.T, fake *uiYouTrack, st *store.Store) *Handler {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	client, err := youtrack.New(youtrack.Config{BaseURL: srv.URL, Token: "fixture-token", Sleep: func(_ context.Context, _ time.Duration) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return &Handler{store: st, csrfKey: []byte("test-key"), youTrack: youtrack.NewService(st, client, youtrack.Options{})}
}

func uiYouTrackStore(t *testing.T) *store.Store {
	t.Helper()
	_, url := testdb.Schema(t, "ui_youtrack")
	st, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

func TestBoardReturnsCachedYouTrackPriorityAndLane(t *testing.T) {
	fake := &uiYouTrack{}
	st := uiYouTrackStore(t)
	h := uiYouTrackHandler(t, fake, st)
	ctx := context.Background()
	project := "experiment"
	linked, err := st.CreateTask(ctx, store.Task{Lane: "handoffkeep", Title: "linked", Kind: "implement", CreatedBy: "test", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	unlinked, err := st.CreateTask(ctx, store.Task{Lane: "handoffkeep", Title: "unlinked", Kind: "implement", CreatedBy: "test", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertYouTrackMirror(ctx, store.YouTrackMirrorInput{TaskID: linked.ID, IssueID: "9-1", DisplayRef: "HK-1", Tenant: "tenant", Role: "mirror", SyncedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutYouTrackFieldCache(ctx, store.YouTrackFieldCache{TaskID: linked.ID, Priority: 7, Lane: "rstack", Source: store.YouTrackSourceYouTrack, FetchedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.boardTasks(rec, httptest.NewRequest(http.MethodGet, "/ui/api/board/tasks?lane=handoffkeep&limit=20", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response boardTasksResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	for _, task := range response.Tasks {
		switch task.ID {
		case linked.ID:
			if task.Priority != 7 || task.Lane != "rstack" || task.PrioritySource != "youtrack" || task.LaneSource != "youtrack" || task.Stale {
				t.Fatalf("linked board task=%+v", task)
			}
		case unlinked.ID:
			if task.Priority != 0 || task.Lane != "handoffkeep" || task.PrioritySource != "" || task.LaneSource != "" || task.Stale {
				t.Fatalf("unlinked board task=%+v", task)
			}
		}
	}
}

func TestPriorityIsPGFirstAndQueuesExport(t *testing.T) {
	fake := &uiYouTrack{}
	st := uiYouTrackStore(t)
	h := uiYouTrackHandler(t, fake, st)
	ctx := context.Background()
	project := "experiment"
	task, err := st.CreateTask(ctx, store.Task{Lane: "handoffkeep", Title: "priority", Kind: "implement", CreatedBy: "test", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertYouTrackMirror(ctx, store.YouTrackMirrorInput{TaskID: task.ID, IssueID: "9-1", DisplayRef: "HK-1", Tenant: "tenant", Role: "mirror", SyncedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	person := cfaccess.Identity{Email: "test@example.com"}
	call := func(body string) (int, map[string]any) {
		r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("http://example.com/ui/api/tasks/%d/priority", task.ID), strings.NewReader(body))
		r.Header.Set("Origin", "http://example.com")
		token, err := h.newCSRFToken(person.Email)
		if err != nil {
			t.Fatal(err)
		}
		r.AddCookie(&http.Cookie{Name: csrfCookie, Value: token})
		r.Header.Set("X-CSRF-Token", token)
		w := httptest.NewRecorder()
		h.serveAPI(w, r, person)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}

	status, out := call(`{"priority":5,"event_key":"one"}`)
	if status != http.StatusOK {
		t.Fatalf("status=%d out=%v", status, out)
	}
	got, _, _ := st.GetTask(ctx, task.ID)
	if got.Priority != 5 {
		t.Fatalf("priority=%d", got.Priority)
	}
	cache, ok, _ := st.GetYouTrackFieldCache(ctx, task.ID)
	if !ok || cache.Priority != 5 || cache.Source != store.YouTrackSourceHK {
		t.Fatalf("cache=%+v ok=%v", cache, ok)
	}
	if due, _ := st.DueYouTrackOutbox(ctx, 10); len(due) != 1 || due[0].Op != store.YouTrackOpPriority {
		t.Fatalf("outbox=%+v", due)
	}

	// A YouTrack outage does not block the next write; the outbox retries.
	fake.fail = true
	status, out = call(`{"priority":9,"event_key":"two"}`)
	if status != http.StatusOK {
		t.Fatalf("outage: status=%d out=%v", status, out)
	}
	got, _, _ = st.GetTask(ctx, task.ID)
	if got.Priority != 9 {
		t.Fatalf("PG priority=%d", got.Priority)
	}
	due, _ := st.DueYouTrackOutbox(ctx, 10)
	if len(due) != 1 || due[0].Value != "9" {
		t.Fatalf("outbox=%+v", due)
	}
}
