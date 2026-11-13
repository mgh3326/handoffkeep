package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
	"github.com/mgh3326/handoffkeep/internal/youtrack"
)

// ytServer is a fake YouTrack that serves a task issue list over HTTP, so the
// routes run the real client against it (hk 4430).
type ytServer struct {
	mu     sync.Mutex
	issues []youtrack.Issue
	// changes are the state changes /api/activities serves, in the wire shape.
	changes []map[string]any
	fail    bool
}

func (f *ytServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodGet || (r.URL.Path != "/api/issues" && r.URL.Path != "/api/activities") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if f.fail {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"unavailable"}`))
		return
	}
	if r.URL.Path == "/api/activities" {
		skip, _ := strconv.Atoi(r.URL.Query().Get("$skip"))
		page := []map[string]any{}
		if skip < len(f.changes) {
			page = f.changes[skip:]
		}
		_ = json.NewEncoder(w).Encode(page)
		return
	}
	skip, _ := strconv.Atoi(r.URL.Query().Get("$skip"))
	page := []youtrack.Issue{}
	if skip < len(f.issues) {
		page = f.issues[skip:]
	}
	_ = json.NewEncoder(w).Encode(page)
}

func (f *ytServer) set(issues ...youtrack.Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues = issues
}

// setChanges serves state changes to developing and so on, as the activity log
// records them: field name, new value, target issue and a millisecond time.
func (f *ytServer) setChanges(changes ...youtrack.StateChange) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changes = nil
	for _, c := range changes {
		f.changes = append(f.changes, map[string]any{
			"timestamp": c.At.UnixMilli(),
			"field":     map[string]any{"name": "hk 상태"},
			"added":     []map[string]any{{"name": youtrack.StateLabel(c.State)}},
			"target":    map[string]any{"id": c.IssueID},
		})
	}
}

func (f *ytServer) addChange(change map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changes = append(f.changes, change)
}

func (f *ytServer) setFail(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fail
}

type ytRouteFixture struct {
	h      *Handler
	st     *store.Store
	fake   *ytServer
	tenant string
	dbURL  string
}

func newYTRouteFixture(t *testing.T) ytRouteFixture {
	t.Helper()
	_, dbURL := testdb.Schema(t, "ui_yt_routes")
	st, err := store.Open(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	fake := &ytServer{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	client, err := youtrack.New(youtrack.Config{BaseURL: srv.URL, Token: "fixture-token", MaxRetries: -1,
		Sleep: func(context.Context, time.Duration) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{store: st, csrfKey: []byte("test-key"), ytTasks: newYTTaskSource(client, st, client.Tenant(), time.Nanosecond)}
	return ytRouteFixture{h: h, st: st, fake: fake, tenant: client.Tenant(), dbURL: dbURL}
}

func (f ytRouteFixture) task(t *testing.T, title string) store.Task {
	t.Helper()
	project := "experiment"
	task, err := f.st.CreateTask(context.Background(), store.Task{Lane: "handoffkeep", Title: title, Kind: "implement", CreatedBy: "test", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func (f ytRouteFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), f.dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func (f ytRouteFixture) queryRow(t *testing.T, sql string, dest ...any) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), f.dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if err := conn.QueryRow(context.Background(), sql).Scan(dest...); err != nil {
		t.Fatal(err)
	}
}

var operator = cfaccess.Identity{Email: "operator@example.com"}

func TestYouTrackBoardReadsIssuesAndJoinsHK(t *testing.T) {
	f := newYTRouteFixture(t)
	byField := f.task(t, "hk title A")
	byMirror := f.task(t, "hk title B")
	f.task(t, "no issue")
	if _, err := f.st.UpsertYouTrackMirror(context.Background(), store.YouTrackMirrorInput{TaskID: byMirror.ID, IssueID: "2-9", Tenant: f.tenant, Role: "mirror"}); err != nil {
		t.Fatal(err)
	}
	f.fake.set(
		ytIssue("2-1", byField.ID, "yt title A", "developing", "rstack", 7, youtrack.IssueField{Name: "등급", Value: json.RawMessage(`{"name":"A"}`)}),
		ytIssue("2-9", 0, "yt title B", "reviewing", "handoffkeep", 1),
	)
	get := func(path string) (int, boardTasksResponse, string) {
		rec := httptest.NewRecorder()
		f.h.boardTasks(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var out boardTasksResponse
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
		}
		return rec.Code, out, rec.Body.String()
	}
	code, page, body := get("/ui/api/board/tasks?limit=20")
	if code != http.StatusOK || page.Source != "youtrack" || len(page.Tasks) != 2 {
		t.Fatalf("status=%d body=%s", code, body)
	}
	a, b := page.Tasks[0], page.Tasks[1]
	if a.ID != byField.ID || a.Title != "yt title A" || a.State != "developing" || a.Lane != "rstack" || a.Priority != 7 ||
		a.Grade != "A" || a.Issue != "HK-2-1" || a.CreatedBy != "test" {
		t.Fatalf("task A=%+v", a)
	}
	if b.ID != byMirror.ID || b.Title != "yt title B" || b.State != "reviewing" {
		t.Fatalf("task B=%+v", b)
	}
	if code, page, _ := get("/ui/api/board/tasks?lane=rstack"); code != http.StatusOK || len(page.Tasks) != 1 || page.Tasks[0].ID != byField.ID {
		t.Fatalf("lane filter: status=%d tasks=%+v", code, page.Tasks)
	}
	if code, page, _ := get("/ui/api/board/tasks?limit=1"); code != http.StatusOK || len(page.Tasks) != 1 || !page.Truncated || page.NextAfterID != byField.ID {
		t.Fatalf("paging: status=%d page=%+v", code, page)
	}
	f.fake.setFail(true)
	if code, _, body := get("/ui/api/board/tasks"); code != http.StatusBadGateway || !strings.Contains(body, `"youtrack_unavailable"`) {
		t.Fatalf("YouTrack down: status=%d body=%q", code, body)
	}
}

func TestYouTrackWorkStatusReadsIssues(t *testing.T) {
	f := newYTRouteFixture(t)
	task := f.task(t, "hk title")
	oldTask := f.task(t, "old hk title")
	oldIssue := ytIssue("2-2", oldTask.ID, "old yt title", "developing", "handoffkeep", 0)
	oldIssue.Created = time.Now().UTC().Add(-60 * 24 * time.Hour).UnixMilli()
	oldIssue.Updated = time.Now().UTC().Add(-time.Hour).UnixMilli()
	f.fake.set(ytIssue("2-1", task.ID, "yt title", "developing", "handoffkeep", 0), oldIssue)
	enteredAt := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
	f.fake.setChanges(youtrack.StateChange{IssueID: "2-1", At: enteredAt, State: "developing"})
	// Scalar values on unrelated fields are valid activity data. They must not
	// prevent the work status page from using the state activity above.
	f.fake.addChange(map[string]any{
		"timestamp": enteredAt.Add(time.Minute).UnixMilli(),
		"field":     map[string]any{"name": "hk PR"},
		"added":     []any{"pull/42"},
		"target":    map[string]any{"id": "2-1"},
	})
	rec := httptest.NewRecorder()
	f.h.workStatus(rec, httptest.NewRequest(http.MethodGet, "/ui/api/work-status", nil), operator)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var status store.WorkStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	// hk still holds the task in backlog; YouTrack's state is the one shown.
	if status.Summary.InWork != 2 || status.Summary.Backlog != 0 || len(status.InWork) != 2 {
		t.Fatalf("work status=%+v", status)
	}
	// The age is YouTrack's activity time, not hk's: hk has no event for it.
	var entered, old *store.WorkStatusInWorkTask
	for i := range status.InWork {
		switch status.InWork[i].ID {
		case task.ID:
			entered = &status.InWork[i]
		case oldTask.ID:
			old = &status.InWork[i]
		}
	}
	if entered == nil || entered.Title != "yt title" || entered.Since == nil || !entered.Since.Equal(enteredAt) {
		t.Fatalf("entered task=%+v want since %v", entered, enteredAt)
	}
	wantWindowStart := status.GeneratedAt.Add(-stateHistoryLookback)
	if old == nil || old.Since == nil || old.Since.Before(wantWindowStart.Add(-time.Second)) || old.Since.After(wantWindowStart.Add(time.Second)) ||
		old.AgeSeconds < int64(stateHistoryLookback.Seconds())-1 || old.AgeSeconds > int64(stateHistoryLookback.Seconds()) {
		t.Fatalf("old task=%+v want since within 1s of %v and age within 1s of %d", old, wantWindowStart, int64(stateHistoryLookback.Seconds()))
	}
	f.fake.setFail(true)
	rec = httptest.NewRecorder()
	f.h.workStatus(rec, httptest.NewRequest(http.MethodGet, "/ui/api/work-status", nil), operator)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("YouTrack down: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestYouTrackHomeTaskCounts(t *testing.T) {
	f := newYTRouteFixture(t)
	open := f.task(t, "hk open")
	closed := f.task(t, "hk closed")
	// hk still says backlog for this one; YouTrack says developing, and it
	// has had no activity for a month: a stall by YouTrack's state.
	// task_events is append-only, so the task is inserted a month old with
	// no events: its activity is its creation time.
	var stalled store.Task
	f.queryRow(t, `INSERT INTO tasks(lane,title,kind,state,created_by,created_at,updated_at,last_activity_at)
		VALUES('handoffkeep','hk stalled','implement','backlog','test',now()-interval '30 days',now()-interval '30 days',now()-interval '30 days') RETURNING id`, &stalled.ID)
	f.exec(t, `UPDATE tasks SET awaiting_decision=true WHERE id = ANY($1)`, []int64{open.ID, closed.ID})
	f.fake.set(
		ytIssue("2-1", open.ID, "yt open", "backlog", "rstack", 4),
		ytIssue("2-2", closed.ID, "yt closed", "completed", "handoffkeep", 0),
		ytIssue("2-3", stalled.ID, "yt stalled", "developing", "handoffkeep", 0),
	)
	rec := httptest.NewRecorder()
	f.h.attention(rec, httptest.NewRequest(http.MethodGet, "/ui/api/attention", nil), operator)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response attentionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Tasks.Count != 2 || response.Tasks.AwaitingDecision != 1 || response.Tasks.Stalled != 1 || len(response.Tasks.Items) != 2 {
		t.Fatalf("tasks=%+v", response.Tasks)
	}
	items := map[int64]attentionTaskView{}
	for _, x := range response.Tasks.Items {
		items[x.ID] = x
	}
	if x := items[open.ID]; x.Title != "yt open" || x.Lane != "rstack" || x.Priority != 4 || x.Reason != store.AttentionReasonAwaitingDecision {
		t.Fatalf("item=%+v", x)
	}
	if x := items[stalled.ID]; x.Title != "yt stalled" || x.State != "developing" || x.Reason != store.AttentionReasonStalled {
		t.Fatalf("stalled item=%+v", x)
	}
	f.fake.setFail(true)
	rec = httptest.NewRecorder()
	f.h.attention(rec, httptest.NewRequest(http.MethodGet, "/ui/api/attention", nil), operator)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("YouTrack down: status=%d body=%s", rec.Code, rec.Body.String())
	}
}
