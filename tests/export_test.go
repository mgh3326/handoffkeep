package tests

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/store"
)

type exportResponse struct {
	SnapshotID string `json:"snapshot_id"`
	DBTime     string `json:"db_time"`
	Source     struct {
		VCSRevision string `json:"vcs_revision"`
	} `json:"source"`
	Scope struct {
		Lane       string `json:"lane"`
		State      string `json:"state"`
		ParentLane string `json:"parent_lane"`
		Limit      int    `json:"limit"`
	} `json:"scope"`
	Watermark struct {
		TaskEventMaxID  int64 `json:"task_event_max_id"`
		RelayEventMaxID int64 `json:"relay_event_max_id"`
	} `json:"watermark"`
	Counts struct {
		Total   int            `json:"total"`
		ByState map[string]int `json:"by_state"`
		ByLane  map[string]int `json:"by_lane"`
	} `json:"counts"`
	RowsReturned int               `json:"rows_returned"`
	Complete     bool              `json:"complete"`
	Truncated    bool              `json:"truncated"`
	IDsSHA256    string            `json:"ids_sha256"`
	RowsSHA256   string            `json:"rows_sha256"`
	Tasks        []json.RawMessage `json:"tasks"`
}

var canonicalStates = []string{"backlog", "claimed", "in_progress", "verifying", "join", "hold", "needs_decision", "merged", "dropped"}

func exportGet(t *testing.T, h *httptest.Server, token, query string) *http.Response {
	t.Helper()
	return request(t, h.Client(), http.MethodGet, h.URL+"/v1/tasks/export"+query, token, nil)
}

func exportDecode(t *testing.T, resp *http.Response) exportResponse {
	t.Helper()
	defer resp.Body.Close()
	var doc exportResponse
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func exportRecount(t *testing.T, where string, args ...any) (int, map[string]int, map[string]int) {
	t.Helper()
	db, err := pgx.Connect(t.Context(), os.Getenv("HANDOFFKEEP_TEST_DB_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(t.Context())
	var total int
	if err := db.QueryRow(t.Context(), `SELECT COUNT(*) FROM tasks WHERE `+where, args...).Scan(&total); err != nil {
		t.Fatal(err)
	}
	byState, byLane := map[string]int{}, map[string]int{}
	rows, err := db.Query(t.Context(), `SELECT state,lane FROM tasks WHERE `+where, args...)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var state, lane string
		if err := rows.Scan(&state, &lane); err != nil {
			t.Fatal(err)
		}
		byState[state]++
		byLane[lane]++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return total, byState, byLane
}

func exportConsistent(t *testing.T, doc exportResponse) {
	t.Helper()
	sumState, sumLane := 0, 0
	for state, count := range doc.Counts.ByState {
		sumState += count
		if count < 0 {
			t.Fatalf("negative count: %+v", doc.Counts)
		}
		known := false
		for _, canonical := range canonicalStates {
			known = known || state == canonical
		}
		if !known {
			t.Fatalf("unknown state %q in counts", state)
		}
	}
	for _, count := range doc.Counts.ByLane {
		sumLane += count
	}
	if sumState != doc.Counts.Total || sumLane != doc.Counts.Total {
		t.Fatalf("counts inconsistent: total=%d by_state=%+v by_lane=%+v", doc.Counts.Total, doc.Counts.ByState, doc.Counts.ByLane)
	}
	for _, state := range canonicalStates {
		if _, ok := doc.Counts.ByState[state]; !ok {
			t.Fatalf("by_state missing canonical state %q: %+v", state, doc.Counts.ByState)
		}
	}
	if doc.Complete && doc.RowsReturned != doc.Counts.Total {
		t.Fatalf("complete but rows_returned=%d total=%d", doc.RowsReturned, doc.Counts.Total)
	}
	if doc.Counts.Total > doc.Scope.Limit && (!doc.Truncated || doc.Complete) {
		t.Fatalf("total>limit must truncate: %+v", doc)
	}
	if doc.Truncated && doc.Complete {
		t.Fatal("truncated and complete both true")
	}
	if doc.RowsReturned != len(doc.Tasks) {
		t.Fatalf("rows_returned=%d len(tasks)=%d", doc.RowsReturned, len(doc.Tasks))
	}
	if doc.SnapshotID == "" || doc.DBTime == "" || doc.Source.VCSRevision == "" {
		t.Fatalf("metadata empty: %+v", doc)
	}
	seen := map[int64]bool{}
	var previous int64
	for i, raw := range doc.Tasks {
		var task store.Task
		if err := json.Unmarshal(raw, &task); err != nil {
			t.Fatal(err)
		}
		if task.Events != nil {
			t.Fatalf("export row %d carries task events", i)
		}
		if seen[task.ID] {
			t.Fatalf("duplicate task id %d", task.ID)
		}
		seen[task.ID] = true
		if i > 0 && task.ID <= previous {
			t.Fatalf("rows not in ascending id order at %d", i)
		}
		previous = task.ID
	}
}

func TestExportRouteAuthAndValidation(t *testing.T) {
	s := taskTestStore(t)
	lane := taskLane(t)
	newTask(t, s, lane, "export probe", 0)
	h := taskHTTP(s)
	defer h.Close()

	resp := exportGet(t, h, "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token status=%d", resp.StatusCode)
	}
	var unauthorized map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&unauthorized); err != nil || unauthorized["error"] != "unauthorized" {
		t.Fatalf("body=%v err=%v", unauthorized, err)
	}
	resp.Body.Close()

	resp = exportGet(t, h, "wrong-token", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = exportGet(t, h, "node-token", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	doc := exportDecode(t, resp)
	exportConsistent(t, doc)
	if doc.Scope.Limit != 1000 || doc.Scope.Lane != "" || doc.Scope.State != "" || doc.Scope.ParentLane != "" {
		t.Fatalf("scope=%+v", doc.Scope)
	}

	resp = exportGet(t, h, "node-token", "?lane="+lane+"&state=backlog&parent_lane=&limit=500")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("filtered status=%d", resp.StatusCode)
	}
	doc = exportDecode(t, resp)
	exportConsistent(t, doc)
	if doc.Scope.Lane != lane || doc.Scope.State != "backlog" || doc.Scope.Limit != 500 {
		t.Fatalf("normalized scope=%+v", doc.Scope)
	}
	if len(doc.Tasks) != 1 {
		t.Fatalf("filtered tasks=%d", len(doc.Tasks))
	}

	for _, query := range []string{
		"?state=bogus", "?lane=bad!lane", "?lane=bad%20lane", "?parent_lane=bad!lane",
		"?limit=0", "?limit=-3", "?limit=abc", "?limit=1.5", "?limit=10001",
	} {
		resp = exportGet(t, h, "node-token", query)
		if resp.StatusCode != http.StatusBadRequest {
			resp.Body.Close()
			t.Fatalf("query=%s status=%d", query, resp.StatusCode)
		}
		var body map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body["error"] != "invalid_export_query" {
			resp.Body.Close()
			t.Fatalf("query=%s body=%v err=%v", query, body, err)
		}
		resp.Body.Close()
	}
	resp = exportGet(t, h, "node-token", "?limit=10000")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("limit=10000 status=%d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestExportCountsAndFilters fixtures tasks in every canonical state across
// three lanes, then compares the export's counts and rows against an
// independent recount over the same tables.
func TestExportCountsAndFilters(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	laneA, laneB, laneC := taskLane(t), taskLane(t), taskLane(t)

	newTask(t, s, laneA, "backlog task", 0)
	claimed := newTask(t, s, laneA, "claimed task", 0)
	if _, err := s.ClaimTask(t.Context(), claimed.ID, "captain"); err != nil {
		t.Fatal(err)
	}
	inProgress := newTask(t, s, laneA, "progress task", 0)
	if _, err := s.ClaimTask(t.Context(), inProgress.ID, "captain"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTask(t.Context(), inProgress.ID, "in_progress", "node", "", nil); err != nil {
		t.Fatal(err)
	}
	verifying := newTask(t, s, laneA, "verify task", 0)
	if _, err := s.ClaimTask(t.Context(), verifying.ID, "captain"); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"in_progress", "verifying"} {
		if _, err := s.TransitionTask(t.Context(), verifying.ID, to, "node", "", nil); err != nil {
			t.Fatal(err)
		}
	}

	join := newTask(t, s, laneB, "join task", 0)
	if _, err := s.ClaimTask(t.Context(), join.ID, "captain"); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"in_progress", "join"} {
		if _, err := s.TransitionTask(t.Context(), join.ID, to, "node", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	hold := newTask(t, s, laneB, "hold task", 0)
	if _, err := s.TransitionTask(t.Context(), hold.ID, "hold", "node", "", nil); err != nil {
		t.Fatal(err)
	}
	decision := newTask(t, s, laneB, "decision task", 0)
	if _, err := s.ClaimTask(t.Context(), decision.ID, "captain"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTask(t.Context(), decision.ID, "needs_decision", "node", "which path?", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(t.Context(), store.Task{Lane: laneB, ParentLane: laneA, Title: "child decision", Kind: "implement", CreatedBy: "node"}); err != nil {
		t.Fatal(err)
	}

	merged := newTask(t, s, laneC, "merged task", 0)
	if _, err := s.ClaimTask(t.Context(), merged.ID, "captain"); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"in_progress", "verifying", "merged"} {
		if _, err := s.TransitionTask(t.Context(), merged.ID, to, "node", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	dropped := newTask(t, s, laneC, "dropped task", 0)
	if _, err := s.TransitionTask(t.Context(), dropped.ID, "dropped", "node", "", nil); err != nil {
		t.Fatal(err)
	}

	// Empty scope: recount the whole table independently.
	total, byState, byLane := exportRecount(t, "1=1")
	resp := exportGet(t, h, "node-token", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	doc := exportDecode(t, resp)
	exportConsistent(t, doc)
	if doc.Counts.Total != total {
		t.Fatalf("total=%d recount=%d", doc.Counts.Total, total)
	}
	for state, count := range byState {
		if doc.Counts.ByState[state] != count {
			t.Fatalf("by_state[%s]=%d recount=%d", state, doc.Counts.ByState[state], count)
		}
	}
	for lane, count := range byLane {
		if doc.Counts.ByLane[lane] != count {
			t.Fatalf("by_lane[%s]=%d recount=%d", lane, doc.Counts.ByLane[lane], count)
		}
	}
	if len(doc.Counts.ByLane) != len(byLane) {
		t.Fatalf("by_lane=%+v recount=%+v", doc.Counts.ByLane, byLane)
	}

	// Filtered scopes.
	for _, tc := range []struct {
		query, where string
		args         []any
	}{
		{"?lane=" + laneA, "lane=$1", []any{laneA}},
		{"?lane=" + laneB, "lane=$1", []any{laneB}},
		{"?state=merged", "state=$1", []any{"merged"}},
		{"?state=dropped", "state=$1", []any{"dropped"}},
		{"?parent_lane=" + laneA, "parent_lane=$1", []any{laneA}},
		{"?lane=" + laneA + "&state=backlog", "lane=$1 AND state=$2", []any{laneA, "backlog"}},
	} {
		wantTotal, wantByState, wantByLane := exportRecount(t, tc.where, tc.args...)
		resp = exportGet(t, h, "node-token", tc.query+"&limit=10000")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("query=%s status=%d", tc.query, resp.StatusCode)
		}
		doc = exportDecode(t, resp)
		exportConsistent(t, doc)
		if doc.Counts.Total != wantTotal || doc.RowsReturned != wantTotal || !doc.Complete {
			t.Fatalf("query=%s total=%d want=%d", tc.query, doc.Counts.Total, wantTotal)
		}
		for state, count := range wantByState {
			if doc.Counts.ByState[state] != count {
				t.Fatalf("query=%s by_state[%s]=%d want=%d", tc.query, state, doc.Counts.ByState[state], count)
			}
		}
		for lane, count := range wantByLane {
			if doc.Counts.ByLane[lane] != count {
				t.Fatalf("query=%s by_lane[%s]=%d want=%d", tc.query, lane, doc.Counts.ByLane[lane], count)
			}
		}
		if len(doc.Counts.ByLane) != len(wantByLane) {
			t.Fatalf("query=%s by_lane=%+v want=%+v", tc.query, doc.Counts.ByLane, wantByLane)
		}
	}
}

func TestExportCapBoundaries(t *testing.T) {
	s := taskTestStore(t)
	lane := taskLane(t)
	for i := 0; i < 5; i++ {
		newTask(t, s, lane, fmt.Sprintf("cap %d", i), 0)
	}
	h := taskHTTP(s)
	defer h.Close()

	resp := exportGet(t, h, "node-token", "?lane="+lane+"&limit=4")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	doc := exportDecode(t, resp)
	exportConsistent(t, doc)
	if doc.Counts.Total != 5 || doc.RowsReturned != 4 || !doc.Truncated || doc.Complete {
		t.Fatalf("limit-1 export=%+v", doc)
	}

	resp = exportGet(t, h, "node-token", "?lane="+lane+"&limit=5")
	doc = exportDecode(t, resp)
	exportConsistent(t, doc)
	if doc.Counts.Total != 5 || doc.RowsReturned != 5 || doc.Truncated || !doc.Complete {
		t.Fatalf("limit==total export=%+v", doc)
	}

	resp = exportGet(t, h, "node-token", "?lane="+lane+"&limit=6")
	doc = exportDecode(t, resp)
	exportConsistent(t, doc)
	if doc.Counts.Total != 5 || doc.RowsReturned != 5 || doc.Truncated || !doc.Complete {
		t.Fatalf("limit+1 export=%+v", doc)
	}

	for _, query := range []string{"?limit=0", "?limit=-1", "?limit=abc", "?limit=10001"} {
		resp = exportGet(t, h, "node-token", query)
		if resp.StatusCode != http.StatusBadRequest {
			resp.Body.Close()
			t.Fatalf("query=%s status=%d", query, resp.StatusCode)
		}
		var body map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body["error"] != "invalid_export_query" {
			resp.Body.Close()
			t.Fatalf("query=%s body=%v", query, body)
		}
		resp.Body.Close()
	}
}

// TestExportDigests recomputes both hashes from the decoded response and
// proves they react to row order, row content, and id sequence changes.
func TestExportDigests(t *testing.T) {
	s := taskTestStore(t)
	lane := taskLane(t)
	for i := 0; i < 3; i++ {
		task, err := s.CreateTask(t.Context(), store.Task{Lane: lane, Title: fmt.Sprintf("digest 태스크 %d <&>", i), Kind: "implement", Priority: i, Refs: store.TaskRefs{PR: strconv.Itoa(100 + i), HeadSHA: fmt.Sprintf("sha%d", i)}, CreatedBy: "node"})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if _, err := s.ClaimTask(t.Context(), task.ID, "captain-k"); err != nil {
				t.Fatal(err)
			}
		}
	}
	h := taskHTTP(s)
	defer h.Close()
	resp := exportGet(t, h, "node-token", "?lane="+lane)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	doc := exportDecode(t, resp)
	exportConsistent(t, doc)
	if len(doc.Tasks) != 3 {
		t.Fatalf("tasks=%d", len(doc.Tasks))
	}

	ids := make([]int64, len(doc.Tasks))
	idsHash, rowsHash := sha256.New(), sha256.New()
	for i, raw := range doc.Tasks {
		var head struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			t.Fatal(err)
		}
		ids[i] = head.ID
		idsHash.Write([]byte(strconv.FormatInt(head.ID, 10) + "\n"))
		rowsHash.Write(raw)
		rowsHash.Write([]byte("\n"))
	}
	if got := hex.EncodeToString(idsHash.Sum(nil)); got != doc.IDsSHA256 {
		t.Fatalf("ids_sha256=%s recomputed=%s", doc.IDsSHA256, got)
	}
	if got := hex.EncodeToString(rowsHash.Sum(nil)); got != doc.RowsSHA256 {
		t.Fatalf("rows_sha256=%s recomputed=%s", doc.RowsSHA256, got)
	}
	t.Logf("digest recount: ids_sha256=%s rows_sha256=%s (recomputed from %d decoded rows)", doc.IDsSHA256, doc.RowsSHA256, len(doc.Tasks))

	reversed := sha256.New()
	for i := len(doc.Tasks) - 1; i >= 0; i-- {
		reversed.Write(doc.Tasks[i])
		reversed.Write([]byte("\n"))
	}
	if hex.EncodeToString(reversed.Sum(nil)) == doc.RowsSHA256 {
		t.Fatal("rows_sha256 is insensitive to row order")
	}
	var mutated store.Task
	if err := json.Unmarshal(doc.Tasks[0], &mutated); err != nil {
		t.Fatal(err)
	}
	mutated.Title += " tampered"
	encoded, err := json.Marshal(mutated)
	if err != nil {
		t.Fatal(err)
	}
	tampered := sha256.New()
	tampered.Write(encoded)
	tampered.Write([]byte("\n"))
	for i := 1; i < len(doc.Tasks); i++ {
		tampered.Write(doc.Tasks[i])
		tampered.Write([]byte("\n"))
	}
	if hex.EncodeToString(tampered.Sum(nil)) == doc.RowsSHA256 {
		t.Fatal("rows_sha256 is insensitive to row content")
	}

	for name, sequence := range map[string][]int64{
		"reordered":  {ids[2], ids[0], ids[1]},
		"duplicated": {ids[0], ids[0], ids[1], ids[2]},
		"missing":    {ids[0], ids[1]},
	} {
		h := sha256.New()
		for _, id := range sequence {
			h.Write([]byte(strconv.FormatInt(id, 10) + "\n"))
		}
		if hex.EncodeToString(h.Sum(nil)) == doc.IDsSHA256 {
			t.Fatalf("ids_sha256 is insensitive to %s ids", name)
		}
	}

	resp = exportGet(t, h, "node-token", "?lane="+taskLane(t))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("empty status=%d", resp.StatusCode)
	}
	doc = exportDecode(t, resp)
	exportConsistent(t, doc)
	const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if doc.IDsSHA256 != emptySHA256 || doc.RowsSHA256 != emptySHA256 || doc.Tasks == nil || len(doc.Tasks) != 0 {
		t.Fatalf("empty digest export=%+v", doc)
	}
}

// TestExportNoStoreAndErrorBodies pins the cache header and proves failure
// paths leak no token, title, refs, note, DB URL, row data, or credentials.
func TestExportNoStoreAndErrorBodies(t *testing.T) {
	s := taskTestStore(t)
	lane := taskLane(t)
	if _, err := s.CreateTask(t.Context(), store.Task{Lane: lane, Title: "sensitive-export-title", Kind: "implement", Refs: store.TaskRefs{PR: "sensitive-pr-458"}, CreatedBy: "node"}); err != nil {
		t.Fatal(err)
	}
	h := taskHTTP(s)
	defer h.Close()

	resp := exportGet(t, h, "node-token", "?lane="+lane)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q", got)
	}
	resp.Body.Close()

	forbidden := []string{"node-token", "node2-token", "sensitive-export-title", "sensitive-pr-458", "postgres://", "handoffkeep_t458_test", "Authorization", "which path?"}
	assertBodyClean := func(resp *http.Response, wantCode string) {
		t.Helper()
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]json.RawMessage
		for _, needle := range forbidden {
			if strings.Contains(string(raw), needle) {
				t.Fatalf("error body leaked %q: %s", needle, raw)
			}
		}
		if strings.Contains(string(raw), `"complete":true`) || strings.Contains(string(raw), `"complete": true`) {
			t.Fatalf("failure body claims complete: %s", raw)
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("error body not json: %s", raw)
		}
		var code string
		if err := json.Unmarshal(body["error"], &code); err != nil || code != wantCode {
			t.Fatalf("error=%s want %s body=%s", code, wantCode, raw)
		}
		if len(body) != 1 {
			t.Fatalf("error body has extra fields: %s", raw)
		}
	}

	assertBodyClean(exportGet(t, h, "node-token", "?state=bogus"), "invalid_export_query")
	assertBodyClean(exportGet(t, h, "node-token", "?limit=abc"), "invalid_export_query")
	assertBodyClean(exportGet(t, h, "", ""), "unauthorized")
	assertBodyClean(exportGet(t, h, "wrong-token", ""), "unauthorized")

	s.Close()
	assertBodyClean(exportGet(t, h, "node-token", "?lane="+lane), "export_unavailable")
}
