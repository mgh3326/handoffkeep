package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/ui"
)

func boardJSON(t *testing.T, client *http.Client, url, assertion string, out any) int {
	t.Helper()
	response := uiRequest(t, client, http.MethodGet, url, assertion, "")
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(out); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
		return response.StatusCode
	}
	_ = responseText(t, response)
	return response.StatusCode
}

func seedBenchRep(t *testing.T, s *store.Store, originID int64, taskRef, role, model string, inputTokens *int64) {
	t.Helper()
	rep := store.BenchRep{
		OriginID:   originID,
		Profile:    "board-test",
		TaskRef:    &taskRef,
		RecordedAt: time.Now().UTC(),
		CreatedBy:  "board-test",
	}
	if role != "" {
		rep.Role = &role
	}
	if model != "" {
		rep.ModelID = &model
	}
	rep.InputTokens = inputTokens
	if _, err := s.UpsertBenchReps(t.Context(), []store.BenchRep{rep}); err != nil {
		t.Fatal(err)
	}
}

func seedPolicyDoc(t *testing.T, s *store.Store, key, body string) {
	t.Helper()
	if _, _, err := s.PutDocument(t.Context(), store.Document{Key: key, Kind: "note", Body: body, CreatedBy: "board-test"}); err != nil {
		t.Fatal(err)
	}
}

func boardPolicyKey(t *testing.T, suffix string) string {
	t.Helper()
	return fmt.Sprintf("policy/test-%d-%s", time.Now().UnixNano(), suffix)
}

func TestUIBoardAuthBoundary(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)

	for _, path := range []string{"/ui/api/board/tasks", "/ui/api/board/tasks/1", "/ui/api/policy/active", "/ui/queue"} {
		response := uiRequest(t, h.Client(), http.MethodGet, h.URL+path, "", "")
		if response.StatusCode != http.StatusUnauthorized || responseText(t, response) != "" {
			t.Fatalf("%s unauthenticated status=%d", path, response.StatusCode)
		}
		response = uiRequest(t, h.Client(), http.MethodGet, h.URL+path, "", "node-token")
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s bearer status=%d", path, response.StatusCode)
		}
		response = uiRequest(t, h.Client(), http.MethodPost, h.URL+path, assertion, "")
		if response.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s status=%d", path, response.StatusCode)
		}
		response.Body.Close()
	}

	// Allowlisted service identities may read the JSON API but never the page.
	service := newP3UITestServer(t, s, fixture, "", "", []string{"glance-fixture"})
	defer service.Close()
	serviceAssertion := p3ServiceAssertion(t, fixture)
	for _, path := range []string{"/ui/api/board/tasks", "/ui/api/policy/active"} {
		response := p3Request(t, service.Client(), http.MethodGet, service.URL+path, serviceAssertion, nil, "")
		if response.StatusCode != http.StatusOK {
			t.Fatalf("service %s status=%d body=%q", path, response.StatusCode, responseText(t, response))
		}
		response.Body.Close()
	}
	response := p3Request(t, service.Client(), http.MethodGet, service.URL+"/ui/queue", serviceAssertion, nil, "")
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("service /ui/queue status=%d", response.StatusCode)
	}
	response.Body.Close()
}

func TestUIBoardPageCSPAndNoSecret(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "http://hub.internal:9000", fleetTestSecret, 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)

	response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/queue", assertion, "")
	body := responseText(t, response)
	if response.Header.Get("Content-Security-Policy") != ui.ConsoleCSP {
		t.Fatalf("queue page csp=%q", response.Header.Get("Content-Security-Policy"))
	}
	if !strings.Contains(body, `id="board-root"`) || !strings.Contains(body, "/ui/static/console/board.js") || !strings.Contains(body, "/ui/static/console/board.css") {
		t.Fatalf("board mount incomplete: %q", body)
	}
	assertNoSecret(t, body, response.Header, fleetTestSecret, "http://hub.internal:9000")

	for _, asset := range []string{"board.js", "board.css", "fleet.js", "fleet.css"} {
		response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/static/console/"+asset, assertion, "")
		if response.StatusCode != http.StatusOK {
			t.Fatalf("console asset %s status=%d", asset, response.StatusCode)
		}
		response.Body.Close()
	}
	for _, path := range []string{"/ui/api/board/tasks", "/ui/api/policy/active"} {
		response = uiRequest(t, h.Client(), http.MethodGet, h.URL+path, assertion, "")
		body = responseText(t, response)
		if response.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s cache-control=%q", path, response.Header.Get("Cache-Control"))
		}
		assertNoSecret(t, body, response.Header, fleetTestSecret, "http://hub.internal:9000")
	}
}

func TestUIBoardTasksPagination(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	lane := uiLane(t, "board-page")
	other := uiLane(t, "board-other")
	ids := []int64{}
	for i := 0; i < 5; i++ {
		ids = append(ids, createUITask(t, s, lane, fmt.Sprintf("page-task-%d", i)).ID)
	}
	createUITask(t, s, other, "other-lane-task")

	type page struct {
		Tasks []struct {
			ID    int64  `json:"id"`
			Lane  string `json:"lane"`
			State string `json:"state"`
		} `json:"tasks"`
		States      []string `json:"states"`
		NextAfterID int64    `json:"next_after_id"`
		Truncated   bool     `json:"truncated"`
	}

	var first page
	if status := boardJSON(t, h.Client(), h.URL+"/ui/api/board/tasks?lane="+lane+"&limit=2", assertion, &first); status != http.StatusOK {
		t.Fatalf("first page status=%d", status)
	}
	if len(first.Tasks) != 2 || !first.Truncated || first.NextAfterID != first.Tasks[1].ID {
		t.Fatalf("first page=%+v", first)
	}
	if len(first.States) == 0 {
		t.Fatal("states list missing")
	}
	seen := map[int64]bool{}
	after := first.NextAfterID
	collected := append([]int64{}, first.Tasks[0].ID, first.Tasks[1].ID)
	for i := 0; i < 10; i++ {
		var next page
		if status := boardJSON(t, h.Client(), h.URL+"/ui/api/board/tasks?lane="+lane+"&limit=2&after_id="+strconv.FormatInt(after, 10), assertion, &next); status != http.StatusOK {
			t.Fatalf("cursor page %d status=%d", i, status)
		}
		for _, task := range next.Tasks {
			collected = append(collected, task.ID)
		}
		if !next.Truncated {
			break
		}
		if next.NextAfterID <= after {
			t.Fatalf("cursor did not advance: %d -> %d", after, next.NextAfterID)
		}
		after = next.NextAfterID
	}
	for _, id := range collected {
		if seen[id] {
			t.Fatalf("duplicate task %d across pages", id)
		}
		seen[id] = true
	}
	if len(seen) != 5 {
		t.Fatalf("collected %d tasks, want 5: %v", len(seen), collected)
	}
	for _, id := range ids {
		if !seen[id] {
			t.Fatalf("task %d missing from paged results", id)
		}
	}

	var filtered page
	if status := boardJSON(t, h.Client(), h.URL+"/ui/api/board/tasks?lane="+lane+"&state=backlog", assertion, &filtered); status != http.StatusOK || len(filtered.Tasks) != 5 {
		t.Fatalf("state filter status=%d tasks=%d", status, len(filtered.Tasks))
	}
	var empty page
	if status := boardJSON(t, h.Client(), h.URL+"/ui/api/board/tasks?lane="+lane+"&state=merged", assertion, &empty); status != http.StatusOK || len(empty.Tasks) != 0 {
		t.Fatalf("merged filter status=%d tasks=%d", status, len(empty.Tasks))
	}
	for _, query := range []string{"state=bogus", "lane=bad lane!", "after_id=abc", "after_id=-1", "limit=0", "limit=501", "limit=xyz"} {
		response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/api/board/tasks?"+query, assertion, "")
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s status=%d", query, response.StatusCode)
		}
		response.Body.Close()
	}
}

func TestUIBoardTaskDetail(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	lane := uiLane(t, "board-detail")

	task, err := s.CreateTask(t.Context(), store.Task{
		Lane: lane, Title: "detail task", Kind: "implement", Priority: 3, CreatedBy: "test-node",
		Refs: store.TaskRefs{PR: "https://github.com/x/y/pull/1", HeadSHA: "abcdef1234567890", ReportPath: "report/x.md", JobID: "job-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimTask(t.Context(), task.ID, "worker-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTask(t.Context(), task.ID, "in_progress", "worker-a", "started", nil); err != nil {
		t.Fatal(err)
	}

	type detail struct {
		Task struct {
			ID       int64          `json:"id"`
			State    string         `json:"state"`
			Priority int            `json:"priority"`
			Refs     store.TaskRefs `json:"refs"`
		} `json:"task"`
		Events []struct {
			From string `json:"from"`
			To   string `json:"to"`
			By   string `json:"by"`
			Note string `json:"note"`
		} `json:"events"`
		Dwell []struct {
			State   string `json:"state"`
			Seconds int64  `json:"seconds"`
			Open    bool   `json:"open"`
		} `json:"dwell"`
		Linear *struct {
			Identifier string `json:"identifier"`
		} `json:"linear"`
		Participants struct {
			TaskRef   string `json:"task_ref"`
			Coverage  string `json:"coverage"`
			Truncated bool   `json:"truncated"`
			Segments  []struct {
				Role        *string `json:"role"`
				ModelID     *string `json:"model_id"`
				Reps        int     `json:"reps"`
				InputTokens *int64  `json:"input_tokens"`
			} `json:"segments"`
		} `json:"participants"`
	}

	var got detail
	url := h.URL + "/ui/api/board/tasks/" + strconv.FormatInt(task.ID, 10)
	if status := boardJSON(t, h.Client(), url, assertion, &got); status != http.StatusOK {
		t.Fatalf("detail status=%d", status)
	}
	if got.Task.Refs.PR != "https://github.com/x/y/pull/1" || got.Task.Refs.HeadSHA != "abcdef1234567890" || got.Task.Refs.ReportPath != "report/x.md" || got.Task.Refs.JobID != "job-1" {
		t.Fatalf("refs not preserved: %+v", got.Task.Refs)
	}
	if len(got.Events) != 2 || got.Events[0].From != "backlog" || got.Events[0].To != "claimed" || got.Events[1].From != "claimed" || got.Events[1].To != "in_progress" || got.Events[1].Note != "started" {
		t.Fatalf("events=%+v", got.Events)
	}
	open := 0
	for _, segment := range got.Dwell {
		if segment.State == "in_progress" && segment.Open {
			open++
		}
	}
	if open != 1 {
		t.Fatalf("dwell missing open in_progress segment: %+v", got.Dwell)
	}
	if got.Linear != nil {
		t.Fatalf("unexpected linear: %+v", got.Linear)
	}
	if got.Participants.TaskRef != "hk:task/"+strconv.FormatInt(task.ID, 10) || got.Participants.Coverage != "not_collected" || len(got.Participants.Segments) != 0 {
		t.Fatalf("participants=%+v", got.Participants)
	}

	// Exact task_ref join: hk:task/<id> matches; a ref that merely shares the
	// prefix must not leak into this task's telemetry.
	input := int64(1000)
	seedBenchRep(t, s, 1, "hk:task/"+strconv.FormatInt(task.ID, 10), "impl", "model-a", &input)
	seedBenchRep(t, s, 2, "hk:task/"+strconv.FormatInt(task.ID, 10), "verify", "model-b", nil)
	seedBenchRep(t, s, 3, "hk:task/"+strconv.FormatInt(task.ID, 10), "impl", "model-a", &input)
	seedBenchRep(t, s, 4, "hk:task/"+strconv.FormatInt(task.ID, 10)+"0", "impl", "model-c", &input)
	seedBenchRep(t, s, 5, "hk:task/"+strconv.FormatInt(task.ID, 10), "", "", nil)

	got = detail{}
	if status := boardJSON(t, h.Client(), url, assertion, &got); status != http.StatusOK {
		t.Fatalf("detail2 status=%d", status)
	}
	p := got.Participants
	if p.Coverage != "collected" || p.Truncated || len(p.Segments) != 3 {
		t.Fatalf("segments=%+v", p.Segments)
	}
	totalReps := 0
	implSeen, unknownSeen := false, false
	for _, segment := range p.Segments {
		totalReps += segment.Reps
		if segment.Role != nil && *segment.Role == "impl" {
			implSeen = true
			if segment.ModelID == nil || *segment.ModelID != "model-a" || segment.Reps != 2 || segment.InputTokens == nil || *segment.InputTokens != 2000 {
				t.Fatalf("impl segment collapsed or misattributed: %+v", segment)
			}
		}
		if segment.Role == nil && segment.ModelID == nil {
			unknownSeen = true
			if segment.InputTokens != nil {
				t.Fatalf("unknown segment invented tokens: %+v", segment)
			}
		}
	}
	if totalReps != 4 || !implSeen || !unknownSeen {
		t.Fatalf("participant segments wrong: reps=%d impl=%t unknown=%t %+v", totalReps, implSeen, unknownSeen, p.Segments)
	}

	// Linear issue projection is included only when the join row exists.
	db, err := pgx.Connect(t.Context(), os.Getenv("HANDOFFKEEP_TEST_DB_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(t.Context())
	if _, err := db.Exec(t.Context(), `INSERT INTO linear_issues(task_id,issue_id,identifier,created_at,updated_at) VALUES($1,'issue-1','ROB-99',now(),now())`, task.ID); err != nil {
		t.Fatal(err)
	}
	got = detail{}
	if status := boardJSON(t, h.Client(), url, assertion, &got); status != http.StatusOK || got.Linear == nil || got.Linear.Identifier != "ROB-99" {
		t.Fatalf("linear status=%d linear=%+v", status, got.Linear)
	}

	for _, path := range []string{"/ui/api/board/tasks/abc", "/ui/api/board/tasks/-1"} {
		response := uiRequest(t, h.Client(), http.MethodGet, h.URL+path, assertion, "")
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s status=%d", path, response.StatusCode)
		}
		response.Body.Close()
	}
	response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/api/board/tasks/99999999", assertion, "")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("missing task status=%d", response.StatusCode)
	}
	response.Body.Close()
}

// More reps than the detail bound must surface as explicitly partial: the
// segment totals cover the first 500 rows only, and truncated says so instead
// of letting a capped sum masquerade as complete telemetry.
func TestUIBoardTaskDetailRepsTruncated(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	lane := uiLane(t, "board-trunc")

	task := createUITask(t, s, lane, "truncated telemetry")
	taskRef := "hk:task/" + strconv.FormatInt(task.ID, 10)
	role, model := "impl", "model-a"
	input := int64(10)
	// created_by+origin_id is the dedup key; a per-task creator keeps the 501
	// rows unique regardless of what earlier tests seeded.
	reps := make([]store.BenchRep, 0, 501)
	for i := 0; i < 501; i++ {
		reps = append(reps, store.BenchRep{
			OriginID:    int64(i + 1),
			Profile:     "board-test",
			TaskRef:     &taskRef,
			Role:        &role,
			ModelID:     &model,
			InputTokens: &input,
			RecordedAt:  time.Now().UTC(),
			CreatedBy:   fmt.Sprintf("board-trunc-%d", task.ID),
		})
	}
	if _, err := s.UpsertBenchReps(t.Context(), reps); err != nil {
		t.Fatal(err)
	}

	var got struct {
		Participants struct {
			TaskRef   string `json:"task_ref"`
			Coverage  string `json:"coverage"`
			Truncated bool   `json:"truncated"`
			Segments  []struct {
				Reps        int    `json:"reps"`
				InputTokens *int64 `json:"input_tokens"`
			} `json:"segments"`
		} `json:"participants"`
	}
	url := h.URL + "/ui/api/board/tasks/" + strconv.FormatInt(task.ID, 10)
	if status := boardJSON(t, h.Client(), url, assertion, &got); status != http.StatusOK {
		t.Fatalf("detail status=%d", status)
	}
	p := got.Participants
	if p.Coverage != "collected" || !p.Truncated {
		t.Fatalf("501 reps must report collected+truncated: %+v", p)
	}
	total := 0
	for _, segment := range p.Segments {
		total += segment.Reps
	}
	if total != 500 {
		t.Fatalf("partial totals must cover exactly the 500-rep bound, got %d: %+v", total, p.Segments)
	}
}

func TestUIBoardPolicyActive(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)

	// policy/active is a single fixed key in a shared database: remove any
	// pointer a previous run left behind so each status is exercised from a
	// known state.
	db, err := pgx.Connect(t.Context(), os.Getenv("HANDOFFKEEP_TEST_DB_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(t.Context())
	if _, err := db.Exec(t.Context(), `DELETE FROM documents WHERE key='policy/active' OR key LIKE 'policy/test-%'`); err != nil {
		t.Fatal(err)
	}

	type policy struct {
		Status        string `json:"status"`
		PointerKey    string `json:"pointer_key"`
		PointerDocURL string `json:"pointer_doc_url"`
		ManifestKey   string `json:"manifest_key"`
		ManifestURL   string `json:"manifest_doc_url"`
		Release       string `json:"release"`
		Items         []struct {
			Key    string `json:"key"`
			Title  string `json:"title"`
			DocURL string `json:"doc_url"`
			Exists bool   `json:"exists"`
		} `json:"items"`
		Truncated bool `json:"truncated"`
	}
	get := func() policy {
		var out policy
		if status := boardJSON(t, h.Client(), h.URL+"/ui/api/policy/active", assertion, &out); status != http.StatusOK {
			t.Fatalf("policy status=%d", status)
		}
		return out
	}

	if got := get(); got.Status != "not_configured" || got.PointerKey != "policy/active" || len(got.Items) != 0 {
		t.Fatalf("empty policy=%+v", got)
	}

	seedPolicyDoc(t, s, "policy/active", "not a valid key")
	if got := get(); got.Status != "invalid_pointer" {
		t.Fatalf("bad pointer=%+v", got)
	}

	manifest := boardPolicyKey(t, "manifest")
	seedPolicyDoc(t, s, "policy/active", manifest+"\n")
	if got := get(); got.Status != "manifest_missing" || got.ManifestKey != manifest || got.ManifestURL != "/ui/doc/"+manifest {
		t.Fatalf("missing manifest=%+v", got)
	}

	seedPolicyDoc(t, s, manifest, "{not json")
	if got := get(); got.Status != "invalid_manifest" {
		t.Fatalf("bad manifest=%+v", got)
	}

	seedPolicyDoc(t, s, manifest, `{"release":"r1","items":[{"key":"bad key!"}]}`)
	if got := get(); got.Status != "invalid_manifest" {
		t.Fatalf("bad item key=%+v", got)
	}

	// The manifest must be one complete JSON document: a second value or any
	// trailing non-whitespace content is invalid, not silently ignored.
	seedPolicyDoc(t, s, manifest, `{"release":"r1","items":[]} {"extra":1}`)
	if got := get(); got.Status != "invalid_manifest" {
		t.Fatalf("trailing json manifest=%+v", got)
	}
	seedPolicyDoc(t, s, manifest, `{"release":"r1","items":[]} junk`)
	if got := get(); got.Status != "invalid_manifest" {
		t.Fatalf("trailing junk manifest=%+v", got)
	}

	itemA := boardPolicyKey(t, "a")
	itemB := boardPolicyKey(t, "b")
	seedPolicyDoc(t, s, itemA, "doc A body")
	seedPolicyDoc(t, s, manifest, fmt.Sprintf(`{"release":"2026-09-19","items":[{"key":%q,"title":"Doc A"},{"key":%q}]}`+"\n  \n", itemA, itemB))
	got := get()
	if got.Status != "ok" || got.Release != "2026-09-19" || len(got.Items) != 2 {
		t.Fatalf("ok policy=%+v", got)
	}
	if got.Items[0].Key != itemA || got.Items[0].Title != "Doc A" || got.Items[0].DocURL != "/ui/doc/"+itemA || !got.Items[0].Exists {
		t.Fatalf("item A=%+v", got.Items[0])
	}
	if got.Items[1].Key != itemB || got.Items[1].Exists {
		t.Fatalf("missing item not explicit: %+v", got.Items[1])
	}

	// The manifest is bounded: beyond the cap the response reports truncation
	// rather than implying complete coverage.
	var bulk strings.Builder
	bulk.WriteString(`{"release":"bulk","items":[`)
	for i := 0; i < 205; i++ {
		if i > 0 {
			bulk.WriteString(",")
		}
		fmt.Fprintf(&bulk, `{"key":%q}`, boardPolicyKey(t, strconv.Itoa(i)))
	}
	bulk.WriteString(`]}`)
	seedPolicyDoc(t, s, manifest, bulk.String())
	got = get()
	if got.Status != "ok" || !got.Truncated || len(got.Items) != 200 {
		t.Fatalf("bulk policy status=%q truncated=%t items=%d", got.Status, got.Truncated, len(got.Items))
	}
}

// A closed pool makes every store call fail; the BFF must surface the
// unavailable state as a bounded 500, not a partial payload.
func TestUIBoardStoreFailure(t *testing.T) {
	url := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if url == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for UI tests")
	}
	s, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	s.Close()
	for _, path := range []string{"/ui/api/board/tasks", "/ui/api/board/tasks/1", "/ui/api/policy/active"} {
		response := uiRequest(t, h.Client(), http.MethodGet, h.URL+path, assertion, "")
		if response.StatusCode != http.StatusInternalServerError {
			t.Fatalf("%s status=%d", path, response.StatusCode)
		}
		response.Body.Close()
	}
}
