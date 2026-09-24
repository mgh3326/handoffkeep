package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func relaneAPIServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	dbURL := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if dbURL == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for PostgreSQL relane API tests")
	}
	st, err := store.Open(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(Server{Service: Service{Store: st}, Tokens: Tokens{"tester": "tok"}}.Handler())
	t.Cleanup(h.Close)
	t.Cleanup(st.Close)
	return h, st
}

func relaneLane(t *testing.T, prefix string) string {
	t.Helper()
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func relaneAPITask(t *testing.T, st *store.Store, lane, title string) store.Task {
	t.Helper()
	x, err := st.CreateTask(context.Background(), store.Task{Lane: lane, Title: title, Kind: "implement", CreatedBy: "relane-api-test"})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

type relaneResponse struct {
	Results []struct {
		ID      int64       `json:"id"`
		OK      bool        `json:"ok"`
		Changed bool        `json:"changed"`
		Task    *store.Task `json:"task"`
		Error   string      `json:"error"`
	} `json:"results"`
	Moved     int `json:"moved"`
	Unchanged int `json:"unchanged"`
	Failed    int `json:"failed"`
}

func relanePost(t *testing.T, h *httptest.Server, token string, body any) (int, relaneResponse) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, h.URL+"/v1/tasks/relane", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out relaneResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// A single relane writes the move with the token identity as the event actor:
// the client can never choose "by".
func TestTasksRelaneAPI(t *testing.T) {
	h, st := relaneAPIServer(t)
	from, to := relaneLane(t, "api-rel-from"), relaneLane(t, "api-rel-to")
	x := relaneAPITask(t, st, from, "single move")
	relaneAPITask(t, st, to, "anchor")

	code, out := relanePost(t, h, "tok", map[string]any{"ids": []int64{x.ID}, "to": to, "note": "triage"})
	if code != 200 || len(out.Results) != 1 {
		t.Fatalf("code=%d out=%+v", code, out)
	}
	item := out.Results[0]
	if !item.OK || !item.Changed || item.Task == nil || item.Task.Lane != to || item.Task.State != "backlog" {
		t.Fatalf("item=%+v", item)
	}
	if out.Moved != 1 || out.Failed != 0 || out.Unchanged != 0 {
		t.Fatalf("tallies=%+v", out)
	}
	got, found, err := st.GetTask(context.Background(), x.ID)
	if err != nil || !found || len(got.Events) != 1 {
		t.Fatalf("got=%+v", got)
	}
	e := got.Events[0]
	if e.Kind != store.TaskEventRelane || e.From != from || e.To != to || e.By != "tester" || e.Note != "triage" {
		t.Fatalf("event=%+v", e)
	}
}

// Each batch item commits independently: a not-found id and a terminal task
// fail in place while the healthy item still moves.
func TestTasksRelaneAPIBatchPartialFailure(t *testing.T) {
	h, st := relaneAPIServer(t)
	ctx := context.Background()
	from, to := relaneLane(t, "api-batch-from"), relaneLane(t, "api-batch-to")
	ok1 := relaneAPITask(t, st, from, "moves")
	ok2 := relaneAPITask(t, st, from, "already there")
	relaneAPITask(t, st, to, "anchor")
	// A terminal row: backlog→claimed→in_progress→verifying→merged.
	term := relaneAPITask(t, st, from, "closed")
	for _, step := range []struct{ to string }{{"claimed"}, {"in_progress"}, {"verifying"}, {"merged"}} {
		var err error
		if step.to == "claimed" {
			_, err = st.ClaimTask(ctx, term.ID, "c")
		} else {
			_, err = st.TransitionTask(ctx, term.ID, step.to, "c", "", nil)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, changed, err := st.RelaneTask(ctx, ok2.ID, to, "seed", "pre-move", false); err != nil || !changed {
		t.Fatal(err)
	}
	ghost := ok1.ID + 900000

	code, out := relanePost(t, h, "tok", map[string]any{"ids": []int64{ok1.ID, ghost, term.ID, ok2.ID}, "to": to, "note": "batch"})
	if code != 200 || len(out.Results) != 4 {
		t.Fatalf("code=%d out=%+v", code, out)
	}
	res := map[int64]string{}
	changed := map[int64]bool{}
	for _, item := range out.Results {
		if item.OK {
			res[item.ID] = "ok"
			changed[item.ID] = item.Changed
		} else {
			res[item.ID] = item.Error
		}
	}
	if res[ok1.ID] != "ok" || !changed[ok1.ID] {
		t.Fatalf("ok1 result=%q", res[ok1.ID])
	}
	if res[ghost] != "not_found" {
		t.Fatalf("ghost result=%q", res[ghost])
	}
	if res[term.ID] != "task_terminal" {
		t.Fatalf("terminal result=%q", res[term.ID])
	}
	if res[ok2.ID] != "ok" || changed[ok2.ID] {
		t.Fatalf("same-lane result=%q changed=%v", res[ok2.ID], changed[ok2.ID])
	}
	if out.Moved != 1 || out.Unchanged != 1 || out.Failed != 2 {
		t.Fatalf("tallies=%+v", out)
	}
	// The failure did not roll back the healthy item.
	got, _, _ := st.GetTask(ctx, ok1.ID)
	if got.Lane != to {
		t.Fatalf("ok1 lane=%q", got.Lane)
	}
	got, _, _ = st.GetTask(ctx, term.ID)
	if got.Lane != from || got.State != "merged" {
		t.Fatalf("terminal mutated: %+v", got)
	}
}

// Unknown lanes are refused per item unless the caller opts in.
func TestTasksRelaneAPIUnknownLane(t *testing.T) {
	h, st := relaneAPIServer(t)
	from := relaneLane(t, "api-unk-from")
	x := relaneAPITask(t, st, from, "typo probe")
	typo := relaneLane(t, "api-direktor")

	code, out := relanePost(t, h, "tok", map[string]any{"ids": []int64{x.ID}, "to": typo, "note": "typo"})
	if code != 200 || len(out.Results) != 1 || out.Results[0].Error != "unknown_lane" {
		t.Fatalf("code=%d out=%+v", code, out)
	}
	code, out = relanePost(t, h, "tok", map[string]any{"ids": []int64{x.ID}, "to": typo, "note": "intentional", "allow_new_lane": true})
	if code != 200 || len(out.Results) != 1 || !out.Results[0].OK {
		t.Fatalf("code=%d out=%+v", code, out)
	}
}

// Request-shape failures are 400s before any item runs; authentication is the
// existing bearer check, and a caller-supplied "by" is an unknown field.
func TestTasksRelaneAPIRejectsBadRequests(t *testing.T) {
	h, st := relaneAPIServer(t)
	x := relaneAPITask(t, st, relaneLane(t, "api-bad"), "shape probe")

	for _, body := range []map[string]any{
		{"ids": []int64{}, "to": "x", "note": "n"},
		{"ids": []int64{x.ID}, "note": "n"},
		{"ids": []int64{x.ID}, "to": "x"},
		{"ids": []int64{x.ID}, "to": "x", "note": "n", "by": "mallory"},
		{"ids": []int64{0}, "to": "x", "note": "n"},
	} {
		if code, _ := relanePost(t, h, "tok", body); code != 400 {
			t.Fatalf("body=%v code=%d, want 400", body, code)
		}
	}
	if code, _ := relanePost(t, h, "", map[string]any{"ids": []int64{x.ID}, "to": "x", "note": "n"}); code != 401 {
		t.Fatalf("unauthenticated code=%d, want 401", code)
	}
	if code, _ := relanePost(t, h, "wrong", map[string]any{"ids": []int64{x.ID}, "to": "x", "note": "n"}); code != 401 {
		t.Fatalf("bad token code=%d, want 401", code)
	}
	// Over the batch cap is a 400, not 500 partial work.
	tooMany := make([]int64, taskRelaneBatchMax+1)
	for i := range tooMany {
		tooMany[i] = int64(i + 1)
	}
	if code, _ := relanePost(t, h, "tok", map[string]any{"ids": tooMany, "to": "x", "note": "n"}); code != 400 {
		t.Fatalf("oversized batch code=%d, want 400", code)
	}
}
