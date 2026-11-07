package api

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

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
	"github.com/mgh3326/handoffkeep/internal/youtrack"
)

// youtrackAPI is a tiny YouTrack stand-in for the read and relane paths.
type youtrackAPI struct {
	mu       sync.Mutex
	updates  []map[string]any
	issues   map[string]youtrack.Issue
	fail     bool
	priority int
	lane     string
}

func (f *youtrackAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	write := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	path := r.URL.Path
	switch {
	case path == "/api/projects":
		write(http.StatusOK, []youtrack.ProjectRef{{ID: "0-1", ShortName: "HK", Name: "HK"}})
	case strings.HasPrefix(path, "/api/admin/projects/"):
		write(http.StatusOK, []map[string]any{{
			"field":  map[string]any{"id": "f-lane", "name": "레인", "$type": "ProjectCustomField"},
			"bundle": map[string]any{"id": "b-lane", "$type": "EnumBundle", "values": []map[string]any{{"id": "v1", "name": "handoffkeep"}, {"id": "v2", "name": "rstack"}, {"id": "v3", "name": "ui-youtrack"}}},
		}})
	case strings.HasPrefix(path, "/api/issues/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(path, "/api/issues/")
		if f.fail {
			write(http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
			return
		}
		issue, ok := f.issues[id]
		if !ok {
			issue = youtrack.Issue{ID: id, IDReadable: "HK-1", CustomFields: []youtrack.IssueField{
				{Name: "hk 우선순위", Value: json.RawMessage(fmt.Sprint(f.priority))},
				{Name: "레인", Value: json.RawMessage(`{"name":"` + f.lane + `"}`)},
			}}
		}
		write(http.StatusOK, issue)
	case strings.HasPrefix(path, "/api/issues/") && r.Method == http.MethodPost:
		if f.fail {
			write(http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.updates = append(f.updates, body)
		write(http.StatusOK, map[string]any{"id": strings.TrimPrefix(path, "/api/issues/")})
	default:
		write(http.StatusNotFound, map[string]string{"error": "not_found"})
	}
}

func apiYouTrackServer(t *testing.T, fake *youtrackAPI) (*httptest.Server, *store.Store) {
	t.Helper()
	ctx := context.Background()
	_, dbURL := testdb.Schema(t, "youtrack_api")
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	client, err := youtrack.New(youtrack.Config{BaseURL: srv.URL, Token: "fixture-token", Sleep: func(_ context.Context, _ time.Duration) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	service := youtrack.NewService(st, client, youtrack.Options{})
	h := httptest.NewServer(Server{Service: Service{Store: st, YouTrack: service}, Tokens: Tokens{"tester": "tok"}}.Handler())
	t.Cleanup(h.Close)
	return h, st
}

func apiYouTrackTask(t *testing.T, st *store.Store, lane, title string) store.Task {
	t.Helper()
	project := apiTestProject
	x, err := st.CreateTask(context.Background(), store.Task{Lane: lane, Title: title, Kind: "implement", CreatedBy: "test", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func apiGetTask(t *testing.T, h *httptest.Server, id int64) store.Task {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/v1/tasks/%d", h.URL, id), nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var out store.Task
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTaskReadReturnsCachedYouTrackPriorityAndLane(t *testing.T) {
	fake := &youtrackAPI{}
	h, st := apiYouTrackServer(t, fake)
	ctx := context.Background()
	linked := apiYouTrackTask(t, st, "handoffkeep", "linked")
	unlinked := apiYouTrackTask(t, st, "handoffkeep", "unlinked")
	if _, err := st.LinkYouTrackIssue(ctx, linked.ID, "9-1", "HK-1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := st.PutYouTrackFieldCache(ctx, store.YouTrackFieldCache{TaskID: linked.ID, Priority: 7, Lane: "rstack", FetchedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	got := apiGetTask(t, h, linked.ID)
	if got.Priority != 7 || got.Lane != "rstack" || got.PrioritySource != "youtrack" || got.LaneSource != "youtrack" || got.Stale {
		t.Fatalf("linked task=%+v", got)
	}
	got = apiGetTask(t, h, unlinked.ID)
	if got.Priority != 0 || got.Lane != "handoffkeep" || got.PrioritySource != "" || got.LaneSource != "" || got.Stale {
		t.Fatalf("unlinked task=%+v", got)
	}

	// The list route carries the same overlay.
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/v1/tasks?lane=handoffkeep&limit=20", h.URL), nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var listing struct {
		Tasks []store.Task `json:"tasks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	for _, task := range listing.Tasks {
		if task.ID == linked.ID && (task.Priority != 7 || task.Lane != "rstack" || task.PrioritySource != "youtrack") {
			t.Fatalf("list linked task=%+v", task)
		}
		if task.ID == unlinked.ID && (task.PrioritySource != "" || task.LaneSource != "") {
			t.Fatalf("list unlinked task=%+v", task)
		}
	}
}

func TestTaskReadMarksStaleCacheAndFetchError(t *testing.T) {
	fake := &youtrackAPI{fail: true}
	h, st := apiYouTrackServer(t, fake)
	ctx := context.Background()
	stale := apiYouTrackTask(t, st, "handoffkeep", "stale")
	if _, err := st.LinkYouTrackIssue(ctx, stale.ID, "9-2", "HK-2", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := st.PutYouTrackFieldCache(ctx, store.YouTrackFieldCache{TaskID: stale.ID, Priority: 3, Lane: "ui-youtrack", FetchedAt: time.Now().UTC(), Stale: true}); err != nil {
		t.Fatal(err)
	}
	got := apiGetTask(t, h, stale.ID)
	if !got.Stale || got.Priority != 3 || got.Lane != "ui-youtrack" || got.PrioritySource != "youtrack" {
		t.Fatalf("stale task=%+v", got)
	}

	// A linked task with no cache seeds from the hk columns and marks stale
	// when YouTrack cannot be read.
	uncached := apiYouTrackTask(t, st, "handoffkeep", "uncached")
	if _, err := st.LinkYouTrackIssue(ctx, uncached.ID, "9-3", "HK-3", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	got = apiGetTask(t, h, uncached.ID)
	if !got.Stale || got.PrioritySource != "youtrack" || got.Priority != 0 || got.Lane != "handoffkeep" {
		t.Fatalf("fetch-error task=%+v", got)
	}
}

func TestRelaneWritesYouTrackFirstAndUpdatesCache(t *testing.T) {
	fake := &youtrackAPI{}
	h, st := apiYouTrackServer(t, fake)
	ctx := context.Background()
	task := apiYouTrackTask(t, st, "handoffkeep", "relane target")
	if _, err := st.LinkYouTrackIssue(ctx, task.ID, "9-4", "HK-4", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	status, out := relanePost(t, h, "tok", map[string]any{"ids": []int64{task.ID}, "to": "rstack", "note": "test"})
	if status != http.StatusOK || out.Moved != 1 || out.Results[0].Error != "" {
		t.Fatalf("status=%d out=%+v", status, out)
	}
	fake.mu.Lock()
	updates := len(fake.updates)
	fake.mu.Unlock()
	if updates != 1 {
		t.Fatalf("youtrack updates=%d", updates)
	}
	got, _, _ := st.GetTask(ctx, task.ID)
	if got.Lane != "rstack" {
		t.Fatalf("lane=%s", got.Lane)
	}
	cache, ok, _ := st.GetYouTrackFieldCache(ctx, task.ID)
	if !ok || cache.Lane != "rstack" {
		t.Fatalf("cache=%+v ok=%v", cache, ok)
	}
}

func TestRelaneYouTrackFailureChangesNothing(t *testing.T) {
	fake := &youtrackAPI{fail: true}
	h, st := apiYouTrackServer(t, fake)
	ctx := context.Background()
	task := apiYouTrackTask(t, st, "handoffkeep", "relane failure")
	if _, err := st.LinkYouTrackIssue(ctx, task.ID, "9-5", "HK-5", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	status, out := relanePost(t, h, "tok", map[string]any{"ids": []int64{task.ID}, "to": "rstack", "note": "test"})
	if status != http.StatusOK || out.Failed != 1 || out.Results[0].Error != "youtrack_unavailable" {
		t.Fatalf("status=%d out=%+v", status, out)
	}
	got, _, _ := st.GetTask(ctx, task.ID)
	if got.Lane != "handoffkeep" {
		t.Fatalf("hk lane changed on a YouTrack failure: %s", got.Lane)
	}
	if _, ok, _ := st.GetYouTrackFieldCache(ctx, task.ID); ok {
		t.Fatal("cache was written on a YouTrack failure")
	}
}
