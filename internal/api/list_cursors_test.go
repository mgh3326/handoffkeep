package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// TestListCursorsRejectBadAfterID: every paged list answers 400
// invalid_context for a cursor that is not a non-negative integer, before it
// touches the store.
func TestListCursorsRejectBadAfterID(t *testing.T) {
	h := Server{Tokens: Tokens{"op": "op-token"}}.Handler()
	for _, path := range []string{
		"/v1/placements?after_id=",
		"/v1/bench/reps?after_id=",
		"/v1/documents?after_id=",
		"/v1/changes?task=1&after_id=",
	} {
		for _, value := range []string{"x", "-1", "1.5"} {
			req := httptest.NewRequest(http.MethodGet, path+value, nil)
			req.Header.Set("Authorization", "Bearer op-token")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"invalid_context"`) {
				t.Fatalf("%s%s: %d %s", path, value, w.Code, w.Body.String())
			}
		}
	}
}

func listCursorsServer(t *testing.T, prefix string) (*httptest.Server, *store.Store) {
	t.Helper()
	_, dbURL := testdb.Schema(t, prefix)
	st, err := store.Open(t.Context(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Server{
		Service: Service{Store: st},
		Tokens:  Tokens{"op": "op-token", "peer": "peer-token"},
	}.Handler())
	t.Cleanup(srv.Close)
	t.Cleanup(st.Close)
	return srv, st
}

// apiPage walks one list through the API with after_id and returns the ids
// the pages delivered plus each page's next_after_id (nil on the last page).
// url already carries its filters.
func apiPage(t *testing.T, url, token, field string, limit int) ([]int64, []any) {
	t.Helper()
	after, ids, nexts := int64(0), []int64{}, []any{}
	for {
		status, body := jobsRequest(t, http.MethodGet, queryJoin(url, fmt.Sprintf("after_id=%d&limit=%d", after, limit)), token, "")
		if status != http.StatusOK {
			t.Fatalf("%s after %d: %d %v", url, after, status, body)
		}
		rows, _ := body[field].([]any)
		for _, row := range rows {
			ids = append(ids, int64(row.(map[string]any)["id"].(float64)))
		}
		next := body["next_after_id"]
		nexts = append(nexts, next)
		if next == nil {
			return ids, nexts
		}
		after = int64(next.(float64))
		if len(ids) > 1000 {
			t.Fatalf("%s paging did not end", url)
		}
	}
}

// queryJoin adds q to url, using ? or & as the URL needs.
func queryJoin(url, q string) string {
	if strings.Contains(url, "?") {
		return url + "&" + q
	}
	return url + "?" + q
}

func eachOnce(t *testing.T, ids []int64, n int) {
	t.Helper()
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("id %d returned twice: %v", id, ids)
		}
		seen[id] = true
	}
	if len(ids) != n {
		t.Fatalf("paged %d rows, seeded %d: %v", len(ids), n, ids)
	}
}

// TestListCursorsPageEveryRow walks placements, documents and bench reps with
// a small limit: each row arrives once, and a request without after_id keeps
// the answer it had before the cursor existed.
func TestListCursorsPageEveryRow(t *testing.T) {
	h, st := listCursorsServer(t, "list_cursors_api")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)

	for i := range 5 {
		d := store.PlacementDecision{JobID: fmt.Sprintf("api-cursor-%d", i), Attempt: 1, DecidedOn: "host-a", DecidedAt: at,
			Source: "auto", Outcome: "refused", Ranking: "score", StateSource: "hub", Candidates: []store.PlacementCandidate{}}
		if _, _, err := st.RecordPlacement(t.Context(), d); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.PutDocument(t.Context(), store.Document{Key: fmt.Sprintf("api-cursor/%d", i), Kind: "note", Body: "x", CreatedBy: "tester"}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpsertBenchReps(t.Context(), []store.BenchRep{{OriginID: int64(i + 1), Profile: "api-cursor", RecordedAt: at, CreatedBy: "api-cursor"}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, list := range []struct{ url, field string }{
		{h.URL + "/v1/placements", "placements"},
		{h.URL + "/v1/documents?prefix=api-cursor/", "documents"},
		{h.URL + "/v1/bench/reps", "reps"},
	} {
		ids, nexts := apiPage(t, list.url, "op-token", list.field, 2)
		eachOnce(t, ids, 5)
		if nexts[len(nexts)-1] != nil {
			t.Fatalf("%s last page still offered a cursor: %v", list.field, nexts)
		}
		// Without after_id the body carries no cursor field.
		status, body := jobsRequest(t, http.MethodGet, queryJoin(list.url, "limit=2"), "op-token", "")
		if status != http.StatusOK {
			t.Fatalf("%s: %d %v", list.field, status, body)
		}
		if _, present := body["next_after_id"]; present {
			t.Fatalf("%s without after_id returned next_after_id", list.field)
		}
	}
}

// TestChangesCursorsPageEveryRow walks the changes linked to one task.
func TestChangesCursorsPageEveryRow(t *testing.T) {
	h, st := changesServer(t)
	x := relaneAPITask(t, st, relaneLane(t, "cursor"), "cursor target")
	repo := changeRepoName("cursor")
	for i := 1; i <= 5; i++ {
		changeEvent(t, h, st, map[string]any{"source": "mergewake.pr", "source_ref": fmt.Sprintf("https://github.com/%s/pull/%d", repo, i), "task_id": x.ID})
	}
	ids, nexts := apiPage(t, fmt.Sprintf("%s/v1/changes?task=%d", h.URL, x.ID), "peer-token", "changes", 2)
	eachOnce(t, ids, 5)
	if nexts[len(nexts)-1] != nil {
		t.Fatalf("last changes page still offered a cursor: %v", nexts)
	}
	status, body := changesCall(t, h, http.MethodGet, fmt.Sprintf("/v1/changes?task=%d", x.ID), "peer-token", nil)
	if status != http.StatusOK {
		t.Fatalf("list without after_id: %d %v", status, body)
	}
	if _, present := body["next_after_id"]; present {
		t.Fatalf("changes without after_id returned next_after_id")
	}
}
