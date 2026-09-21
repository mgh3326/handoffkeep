package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/store"
)

func commentURL(base string, id int64) string {
	return fmt.Sprintf("%s/v1/tasks/%d/comments", base, id)
}

func postComment(t *testing.T, c *http.Client, base, token string, id int64, body any) (int, map[string]any) {
	t.Helper()
	resp := request(t, c, http.MethodPost, commentURL(base, id), token, body)
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func listComments(t *testing.T, s *store.Store, id int64) []store.TaskComment {
	t.Helper()
	xs, err := s.ListTaskComments(t.Context(), id, 0, store.TaskCommentListMax)
	if err != nil {
		t.Fatal(err)
	}
	return xs
}

func TestTaskCommentsAppendAndListInIDOrder(t *testing.T) {
	s := taskTestStore(t)
	task := newTask(t, s, taskLane(t), "comment order", 0)
	h := taskHTTP(s)
	defer h.Close()
	client := remote.Client{URL: h.URL, Token: "node-token", HTTP: h.Client()}
	var ids []int64
	for _, body := range []string{"first", "second\nwith *markdown*", "third"} {
		x, err := client.CreateTaskComment(t.Context(), task.ID, body)
		if err != nil {
			t.Fatal(err)
		}
		if x.TaskID != task.ID || x.Body != body || x.Author != "node" || x.CreatedAt.IsZero() {
			t.Fatalf("comment=%+v", x)
		}
		ids = append(ids, x.ID)
	}
	if !sort.SliceIsSorted(ids, func(i, j int) bool { return ids[i] < ids[j] }) || ids[0] == ids[1] || ids[1] == ids[2] {
		t.Fatalf("ids not strictly increasing: %v", ids)
	}
	all, err := client.ListTaskComments(t.Context(), task.ID, 0, 100)
	if err != nil || len(all) != 3 || all[0].Body != "first" || all[2].Body != "third" {
		t.Fatalf("all=%+v err=%v", all, err)
	}
	after, err := client.ListTaskComments(t.Context(), task.ID, ids[0], 1)
	if err != nil || len(after) != 1 || after[0].ID != ids[1] {
		t.Fatalf("after=%+v err=%v", after, err)
	}
	empty := newTask(t, s, taskLane(t), "no comments", 0)
	none, err := client.ListTaskComments(t.Context(), empty.ID, 0, 100)
	if err != nil || none == nil || len(none) != 0 {
		t.Fatalf("empty list=%#v err=%v", none, err)
	}
}

// taskSnapshot is everything a comment must not change about its task.
type taskSnapshot struct {
	Task        store.Task
	Events      int
	LaneEvents  int
	OpenLane    bool
	OpenTask    bool
	LinearQueue int
}

func snapshotTask(t *testing.T, s *store.Store, db *pgx.Conn, id int64, lane string, laneQuestion int64) taskSnapshot {
	t.Helper()
	x, found, err := s.GetTask(t.Context(), id)
	if err != nil || !found {
		t.Fatalf("task found=%v err=%v", found, err)
	}
	snap := taskSnapshot{Events: len(x.Events)}
	x.Events = nil
	snap.Task = x
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM relay_events WHERE owner_lane=$1`, lane).Scan(&snap.LaneEvents); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(t.Context(), `SELECT count(*) FROM linear_outbox WHERE task_id=$1`, id).Scan(&snap.LinearQueue); err != nil {
		t.Fatal(err)
	}
	openLane, err := s.ListOpenLaneDecisions(t.Context(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range openLane {
		if event.ID == laneQuestion {
			snap.OpenLane = true
		}
	}
	openTasks, err := s.ListOpenTaskDecisions(t.Context(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, decision := range openTasks {
		if decision.Task.ID == id {
			snap.OpenTask = true
		}
	}
	return snap
}

// A comment is data, never approval or dispatch. Decision-shaped text on a
// needs_decision task and on a lane with an open question must leave task
// state, priority, refs, events, lane events, and both decision inboxes as
// they were.
func TestTaskCommentNeverMovesTaskOrDecisions(t *testing.T) {
	s := taskTestStore(t)
	lane := taskLane(t)
	task := newTask(t, s, lane, "comment is data", 7)
	if _, err := s.ClaimTask(t.Context(), task.ID, "builder"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTask(t.Context(), task.ID, "needs_decision", "builder", "pick one", nil); err != nil {
		t.Fatal(err)
	}
	question, _, err := s.AppendRelayEvent(t.Context(), store.RelayEvent{Kind: "lane.event", OwnerLane: lane, EventID: "comment-q-" + lane, Text: "[decision-needed] ship it?"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _, _ = s.AppendRelayEvent(ctx, store.RelayEvent{Kind: "lane.event", OwnerLane: lane, EventID: "comment-q-cleanup-" + lane, Text: "[decision-answered] #" + strconv.FormatInt(question.ID, 10) + ": cleanup"})
		_, _ = s.TransitionTask(ctx, task.ID, "dropped", "cleanup", "", nil)
	})
	db, err := pgx.Connect(t.Context(), os.Getenv("HANDOFFKEEP_TEST_DB_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(t.Context())
	before := snapshotTask(t, s, db, task.ID, lane, question.ID)
	if !before.OpenLane || !before.OpenTask || before.Task.State != "needs_decision" {
		t.Fatalf("fixture not open: %+v", before)
	}
	h := taskHTTP(s)
	defer h.Close()
	bodies := []string{
		"[decision] #" + strconv.FormatInt(task.ID, 10) + ": A (from operator)",
		"[decision-answered] #" + strconv.FormatInt(question.ID, 10) + ": yes",
		"approved, go ahead and merge",
	}
	for _, body := range bodies {
		if status, out := postComment(t, h.Client(), h.URL, "node-token", task.ID, map[string]string{"body": body}); status != http.StatusCreated {
			t.Fatalf("status=%d out=%v", status, out)
		}
	}
	after := snapshotTask(t, s, db, task.ID, lane, question.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("comment changed task or decisions\nbefore=%+v\nafter=%+v", before, after)
	}
	if got := listComments(t, s, task.ID); len(got) != len(bodies) {
		t.Fatalf("comments=%d want=%d", len(got), len(bodies))
	}
}

// The author is the bearer-token client. A body author is refused outright,
// and identity-looking headers are ignored.
func TestTaskCommentAuthorComesOnlyFromToken(t *testing.T) {
	s := taskTestStore(t)
	task := newTask(t, s, taskLane(t), "author source", 0)
	h := taskHTTP(s)
	defer h.Close()
	for _, body := range []map[string]string{
		{"body": "forged", "author": "operator"},
		{"body": "forged", "created_by": "operator"},
	} {
		status, out := postComment(t, h.Client(), h.URL, "node-token", task.ID, body)
		if status != http.StatusBadRequest || out["error"] != "author_not_accepted" {
			t.Fatalf("forged body %v: status=%d out=%v", body, status, out)
		}
	}
	if got := listComments(t, s, task.ID); len(got) != 0 {
		t.Fatalf("forged comment stored: %+v", got)
	}
	b, _ := json.Marshal(map[string]string{"body": "header forgery"})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, commentURL(h.URL, task.ID), strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer node2-token")
	for _, name := range []string{"X-HK-Author", "X-HK-Client", "X-Forwarded-User", "Cf-Access-Authenticated-User-Email", "From"} {
		req.Header.Set(name, "operator")
	}
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	got := listComments(t, s, task.ID)
	if len(got) != 1 || got[0].Author != "node2" {
		t.Fatalf("comments=%+v", got)
	}
}

// Append-only is enforced by the database, not only by the absence of routes.
func TestTaskCommentsDatabaseAppendOnly(t *testing.T) {
	s := taskTestStore(t)
	task := newTask(t, s, taskLane(t), "immutable comments", 0)
	x, err := s.CreateTaskComment(t.Context(), task.ID, "node", "original")
	if err != nil {
		t.Fatal(err)
	}
	db, err := pgx.Connect(t.Context(), os.Getenv("HANDOFFKEEP_TEST_DB_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(t.Context())
	for _, sql := range []string{
		"UPDATE task_comments SET body='mutated' WHERE id=$1",
		"UPDATE task_comments SET author='operator' WHERE id=$1",
		"DELETE FROM task_comments WHERE id=$1",
	} {
		if _, err := db.Exec(t.Context(), sql, x.ID); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("append-only mutation %q err=%v", sql, err)
		}
	}
	if _, err := db.Exec(t.Context(), "TRUNCATE task_comments"); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("truncate err=%v", err)
	}
	got := listComments(t, s, task.ID)
	if len(got) != 1 || got[0].Body != "original" || got[0].Author != "node" {
		t.Fatalf("comment changed: %+v", got)
	}
	h := taskHTTP(s)
	defer h.Close()
	for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, path := range []string{commentURL(h.URL, task.ID), commentURL(h.URL, task.ID) + "/" + strconv.FormatInt(x.ID, 10)} {
			resp := request(t, h.Client(), method, path, "node-token", map[string]string{"body": "mutated"})
			resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s %s status=%d", method, path, resp.StatusCode)
			}
		}
	}
	if got := listComments(t, s, task.ID); len(got) != 1 || got[0].Body != "original" {
		t.Fatalf("comment changed over HTTP: %+v", got)
	}
}

func TestTaskCommentResponsesAreDistinct(t *testing.T) {
	s := taskTestStore(t)
	task := newTask(t, s, taskLane(t), "comment errors", 0)
	h := taskHTTP(s)
	defer h.Close()
	missing := int64(1 << 60)
	cases := []struct {
		name   string
		token  string
		id     int64
		body   any
		status int
		code   string
	}{
		{"no token", "", task.ID, map[string]string{"body": "x"}, http.StatusUnauthorized, "unauthorized"},
		{"wrong token", "nope", task.ID, map[string]string{"body": "x"}, http.StatusUnauthorized, "unauthorized"},
		{"empty", "node-token", task.ID, map[string]string{"body": ""}, http.StatusBadRequest, "comment_empty"},
		{"blank", "node-token", task.ID, map[string]string{"body": " \n\t "}, http.StatusBadRequest, "comment_empty"},
		{"too long", "node-token", task.ID, map[string]string{"body": strings.Repeat("a", store.TaskCommentMaxBytes+1)}, http.StatusRequestEntityTooLarge, "comment_too_long"},
		{"request too large", "node-token", task.ID, map[string]string{"body": strings.Repeat("\x01", 70000)}, http.StatusRequestEntityTooLarge, "comment_too_long"},
		{"missing task", "node-token", missing, map[string]string{"body": "x"}, http.StatusNotFound, "not_found"},
		{"author field", "node-token", task.ID, map[string]string{"body": "x", "author": "y"}, http.StatusBadRequest, "author_not_accepted"},
	}
	for _, tc := range cases {
		status, out := postComment(t, h.Client(), h.URL, tc.token, tc.id, tc.body)
		if status != tc.status || out["error"] != tc.code {
			t.Errorf("%s: status=%d out=%v want %d %s", tc.name, status, out, tc.status, tc.code)
		}
	}
	for _, tc := range []struct {
		name, token string
		id          int64
		status      int
		code        string
	}{
		{"list no token", "", task.ID, http.StatusUnauthorized, "unauthorized"},
		{"list missing task", "node-token", missing, http.StatusNotFound, "not_found"},
	} {
		resp := request(t, h.Client(), http.MethodGet, commentURL(h.URL, tc.id), tc.token, nil)
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if resp.StatusCode != tc.status || out["error"] != tc.code {
			t.Errorf("%s: status=%d out=%v", tc.name, resp.StatusCode, out)
		}
	}
	if got := listComments(t, s, task.ID); len(got) != 0 {
		t.Fatalf("rejected comments stored: %+v", got)
	}
	x, err := s.CreateTaskComment(t.Context(), task.ID, "node", strings.Repeat("b", store.TaskCommentMaxBytes))
	if err != nil || len(x.Body) != store.TaskCommentMaxBytes {
		t.Fatalf("limit-sized comment err=%v", err)
	}
}

// Concurrent writers must commit in id order so an after_id cursor never
// skips a comment that becomes visible after a larger id.
func TestTaskCommentCursorNeverSkipsConcurrentWrites(t *testing.T) {
	s := taskTestStore(t)
	task := newTask(t, s, taskLane(t), "comment cursor", 0)
	const writers, perWriter = 8, 12
	seen := map[int64]bool{}
	done := make(chan struct{})
	readerErr := make(chan error, 1)
	go func() {
		var cursor int64
		for {
			select {
			case <-done:
				readerErr <- nil
				return
			default:
			}
			xs, err := s.ListTaskComments(context.Background(), task.ID, cursor, store.TaskCommentListMax)
			if err != nil {
				readerErr <- err
				return
			}
			for _, x := range xs {
				seen[x.ID] = true
				cursor = x.ID
			}
		}
	}()
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if _, err := s.CreateTaskComment(context.Background(), task.ID, "node", fmt.Sprintf("w%d-%d", w, i)); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(done)
	if err := <-readerErr; err != nil {
		t.Fatal(err)
	}
	all := listComments(t, s, task.ID)
	if len(all) != writers*perWriter {
		t.Fatalf("comments=%d", len(all))
	}
	missed := 0
	for i, x := range all {
		if i > 0 && x.CreatedAt.Before(all[i-1].CreatedAt) {
			t.Fatalf("created_at order differs from id order at id %d", x.ID)
		}
		// Only ids below the reader's final cursor can have been skipped.
		if !seen[x.ID] && x.ID < maxSeen(seen) {
			missed++
		}
	}
	if missed != 0 {
		t.Fatalf("cursor reader skipped %d comments", missed)
	}
}

func maxSeen(seen map[int64]bool) int64 {
	var out int64
	for id := range seen {
		if id > out {
			out = id
		}
	}
	return out
}
