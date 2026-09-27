package tests

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// #769 (decision/2026-09-27/task-job-linkage item 2): a transition that lands
// a task in claimed or in_progress must leave the row with claimed_by and
// refs.job_id, or carry a recorded no-job reason on the transition event.
// Everything below exercises that contract — refusals, the exception path,
// legacy rows, races, and the non-TransitionTask writers.

func TestTaskTransitionIntoActiveWithoutJobIsRefused(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()

	// backlog -> claimed on a task nobody claimed: no claimant, no job.
	raw := newTask(t, s, taskLane(t), "unclaimed to claimed", 0)
	for _, to := range []string{"claimed", "in_progress"} {
		// in_progress is not a legal backlog edge either way; claim first so
		// the store-level check still sees an active target on a jobless row.
		var err error
		if to == "claimed" {
			_, err = s.TransitionTask(t.Context(), raw.ID, to, "node", "hop", nil, "")
		} else {
			claimed := newTask(t, s, taskLane(t), "claimed jobless", 0)
			if _, cerr := s.ClaimTask(t.Context(), claimed.ID, "captain-a", "", "manual queue item"); cerr != nil {
				t.Fatal(cerr)
			}
			_, err = s.TransitionTask(t.Context(), claimed.ID, to, "node", "", nil, "")
		}
		if !errors.Is(err, store.ErrTaskJobRequired) {
			t.Fatalf("store transition to %s err=%v, want task_job_required", to, err)
		}
	}
	resp := request(t, h.Client(), http.MethodPost, h.URL+"/v1/tasks/"+fmt.Sprint(raw.ID)+"/transition", "node-token", map[string]any{"to": "claimed"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("http status=%d", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "task_job_required" || !strings.Contains(body["reason"], "claimed_by") {
		t.Fatalf("body=%v", body)
	}
	got, _, _ := s.GetTask(t.Context(), raw.ID)
	if got.State != "backlog" || len(got.Events) != 0 {
		t.Fatalf("refused transition wrote: %+v", got)
	}
}

func TestTaskClaimAndTransitionWithJobSatisfyGuard(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	c := remote.Client{URL: h.URL, Token: "node-token", HTTP: h.Client()}

	task := newTask(t, s, taskLane(t), "atomic claim", 0)
	got, err := c.ClaimTask(t.Context(), task.ID, "captain-a", "job-769-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "claimed" || got.ClaimedBy != "captain-a" || got.Refs.JobID != "job-769-1" {
		t.Fatalf("claim=%+v", got)
	}
	// The linked task moves claimed -> in_progress without any exception.
	if _, err := c.TransitionTask(t.Context(), task.ID, "in_progress", "started", nil, ""); err != nil {
		t.Fatal(err)
	}
	row, _, _ := s.GetTask(t.Context(), task.ID)
	for _, e := range row.Events {
		if e.NoJob != "" {
			t.Fatalf("linked task event carries phantom no_job=%q", e.NoJob)
		}
	}
}

func TestTaskNoJobReasonRecordedAndEmptyRefused(t *testing.T) {
	s := taskTestStore(t)
	task := newTask(t, s, taskLane(t), "no-job exception", 0)
	// A decide/ops-style jobless task enters claimed via the explicit flag.
	got, err := s.TransitionTask(t.Context(), task.ID, "claimed", "node", "", nil, "ops queue item, no hub job")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "claimed" || got.ClaimedBy != "" || got.Refs.JobID != "" {
		t.Fatalf("exception transition=%+v", got)
	}
	row, _, _ := s.GetTask(t.Context(), task.ID)
	if len(row.Events) != 1 || row.Events[0].NoJob != "ops queue item, no hub job" {
		t.Fatalf("events=%+v, want recorded no_job reason", row.Events)
	}

	// Whitespace-only and empty reasons are refused when the exemption is
	// needed; the reason is trimmed before it is judged.
	for _, bad := range []string{"", "   ", "\t\n"} {
		next := newTask(t, s, taskLane(t), "empty reason", 0)
		if _, err := s.TransitionTask(t.Context(), next.ID, "claimed", "node", "", nil, bad); !errors.Is(err, store.ErrTaskJobRequired) {
			t.Fatalf("no_job=%q err=%v, want task_job_required", bad, err)
		}
	}
}

func TestTaskJobIDRefsPatchAloneCannotEnterActive(t *testing.T) {
	s := taskTestStore(t)
	// A refs.job_id patch satisfies only half the invariant: a never-claimed
	// task still has no claimant, so claimed stays refused.
	raw := newTask(t, s, taskLane(t), "refs patch cannot claim", 0)
	if _, err := s.TransitionTask(t.Context(), raw.ID, "claimed", "node", "", &store.TaskRefs{JobID: "job-smuggle"}, ""); !errors.Is(err, store.ErrTaskJobRequired) {
		t.Fatalf("smuggled job_id err=%v, want task_job_required", err)
	}
	got, _, _ := s.GetTask(t.Context(), raw.ID)
	if got.State != "backlog" || got.Refs.JobID != "" || len(got.Events) != 0 {
		t.Fatalf("refused smuggle wrote: %+v", got)
	}

	// On a claimed task the same patch is the repair path: job_id lands in
	// refs before the guard reads it, so in_progress is allowed.
	claimed := newTask(t, s, taskLane(t), "job repair", 0)
	if _, err := s.ClaimTask(t.Context(), claimed.ID, "captain-a", "", "pre-job pull"); err != nil {
		t.Fatal(err)
	}
	moved, err := s.TransitionTask(t.Context(), claimed.ID, "in_progress", "node", "attach job", &store.TaskRefs{JobID: "job-repaired"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if moved.Refs.JobID != "job-repaired" {
		t.Fatalf("refs=%+v", moved.Refs)
	}
	row, _, _ := s.GetTask(t.Context(), claimed.ID)
	for _, e := range row.Events {
		if e.To == "in_progress" && e.NoJob != "" {
			t.Fatalf("linked transition recorded a phantom no_job=%q", e.NoJob)
		}
	}
}

func taskDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	db, err := pgxpool.New(t.Context(), os.Getenv("HANDOFFKEEP_TEST_DB_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

// insertLegacyTask writes a pre-guard row directly: active state, no
// claimant, no job — the shape the decision forbids going forward.
func insertLegacyTask(t *testing.T, db *pgxpool.Pool, lane, title, state string) store.Task {
	t.Helper()
	var id int64
	err := db.QueryRow(t.Context(), `INSERT INTO tasks(lane,title,kind,state,refs,claimed_by,created_by,created_at,updated_at) VALUES($1,$2,'implement',$3,'{}'::jsonb,'','legacy',now(),now()) RETURNING id`, lane, title, state).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return store.Task{ID: id, Lane: lane, Title: title, Kind: "implement", State: state}
}

func TestTaskLegacyActiveTaskTransitionsOnwardWithoutJob(t *testing.T) {
	s := taskTestStore(t)
	db := taskDB(t)

	// A legacy in_progress row moves onward to verifying without anything.
	prog := insertLegacyTask(t, db, taskLane(t), "legacy in_progress", "in_progress")
	if _, err := s.TransitionTask(t.Context(), prog.ID, "verifying", "node", "", nil, ""); err != nil {
		t.Fatalf("legacy in_progress -> verifying: %v", err)
	}
	// A legacy claimed row moves to hold/dropped freely, refuses in_progress
	// without a reason, and accepts it with one — then onward is plain.
	legacy := insertLegacyTask(t, db, taskLane(t), "legacy claimed", "claimed")
	if _, err := s.TransitionTask(t.Context(), legacy.ID, "hold", "node", "", nil, ""); err != nil {
		t.Fatalf("legacy claimed -> hold: %v", err)
	}
	legacy2 := insertLegacyTask(t, db, taskLane(t), "legacy claimed 2", "claimed")
	if _, err := s.TransitionTask(t.Context(), legacy2.ID, "in_progress", "node", "", nil, ""); !errors.Is(err, store.ErrTaskJobRequired) {
		t.Fatalf("legacy claimed -> in_progress without reason err=%v", err)
	}
	moved, err := s.TransitionTask(t.Context(), legacy2.ID, "in_progress", "node", "", nil, "predates job linkage")
	if err != nil {
		t.Fatal(err)
	}
	if moved.State != "in_progress" {
		t.Fatalf("legacy move state=%s", moved.State)
	}
	row, _, _ := s.GetTask(t.Context(), legacy2.ID)
	last := row.Events[len(row.Events)-1]
	if last.NoJob != "predates job linkage" {
		t.Fatalf("legacy exception not recorded: %+v", last)
	}
	if _, err := s.TransitionTask(t.Context(), legacy2.ID, "verifying", "node", "", nil, ""); err != nil {
		t.Fatalf("legacy onward after exception: %v", err)
	}
	if _, err := s.TransitionTask(t.Context(), legacy2.ID, "merged", "node", "", nil, ""); err != nil {
		t.Fatalf("legacy to merged: %v", err)
	}
	// Nothing was backfilled: the row still carries no claimant and no job.
	row, _, _ = s.GetTask(t.Context(), legacy2.ID)
	if row.ClaimedBy != "" || row.Refs.JobID != "" {
		t.Fatalf("legacy row was rewritten: %+v", row)
	}
}

func TestTaskClaimAndTransitionRaceKeepsLinkage(t *testing.T) {
	s := taskTestStore(t)
	task := newTask(t, s, taskLane(t), "claim vs transition race", 0)
	// claim --job-id races transition --to claimed --no-job: exactly one wins
	// the row lock; the loser gets a conflict for the moved row. Either way
	// the landed claimed state is accountable — linked or reasoned.
	var wg sync.WaitGroup
	res := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := s.ClaimTask(t.Context(), task.ID, "captain-a", "job-race", "")
		res <- err
	}()
	go func() {
		defer wg.Done()
		_, err := s.TransitionTask(t.Context(), task.ID, "claimed", "node", "", nil, "ops exception")
		res <- err
	}()
	wg.Wait()
	close(res)
	wins := 0
	for err := range res {
		if err == nil {
			wins++
		} else if !errors.Is(err, store.ErrTaskConflict) {
			t.Fatalf("loser err=%v, want task_conflict", err)
		}
	}
	if wins != 1 {
		t.Fatalf("wins=%d want exactly 1", wins)
	}
	row, _, _ := s.GetTask(t.Context(), task.ID)
	linked := row.ClaimedBy != "" && row.Refs.JobID != ""
	reasoned := len(row.Events) == 1 && row.Events[0].NoJob != ""
	if row.State != "claimed" || len(row.Events) != 1 || !(linked || reasoned) {
		t.Fatalf("raced task=%+v events=%+v", row, row.Events)
	}
}

func TestTaskConcurrentNoJobTransitionsOneWinner(t *testing.T) {
	s := taskTestStore(t)
	task := newTask(t, s, taskLane(t), "racing no-job transitions", 0)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.TransitionTask(t.Context(), task.ID, "claimed", "node", "", nil, fmt.Sprintf("reason-%d", i))
			if err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			} else if !errors.Is(err, store.ErrTaskConflict) {
				t.Errorf("transition err=%v, want task_conflict", err)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("wins=%d want 1", wins)
	}
	row, _, _ := s.GetTask(t.Context(), task.ID)
	if len(row.Events) != 1 || row.Events[0].NoJob == "" {
		t.Fatalf("events=%+v want exactly one recorded reason", row.Events)
	}
}

func TestTaskNextRequiresJobOrReason(t *testing.T) {
	s := taskTestStore(t)
	lane := taskLane(t)
	newTask(t, s, lane, "next without flags", 0)
	if _, err := s.NextTask(t.Context(), lane, "captain-a", "", ""); !errors.Is(err, store.ErrTaskJobRequired) {
		t.Fatalf("bare next err=%v, want task_job_required", err)
	}
	got, err := s.NextTask(t.Context(), lane, "captain-a", "", "lane queue pull")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "claimed" || got.ClaimedBy != "captain-a" || got.Refs.JobID != "" {
		t.Fatalf("next=%+v", got)
	}
	row, _, _ := s.GetTask(t.Context(), got.ID)
	if len(row.Events) != 1 || row.Events[0].NoJob != "lane queue pull" {
		t.Fatalf("next event=%+v", row.Events)
	}
	// The row stays claimed: a second next is the honest queue_empty.
	if _, err := s.NextTask(t.Context(), lane, "captain-b", "job-2", ""); !errors.Is(err, store.ErrQueueEmpty) {
		t.Fatalf("empty next err=%v", err)
	}
}

func TestTaskClaimRejectsJobIDPlusNoJob(t *testing.T) {
	s := taskTestStore(t)
	task := newTask(t, s, taskLane(t), "exclusive flags", 0)
	if _, err := s.ClaimTask(t.Context(), task.ID, "captain-a", "job-1", "also no job"); err == nil || !strings.Contains(err.Error(), "exclusive") {
		t.Fatalf("job_id+no_job err=%v, want exclusivity refusal", err)
	}
	row, _, _ := s.GetTask(t.Context(), task.ID)
	if row.State != "backlog" || len(row.Events) != 0 {
		t.Fatalf("exclusive refusal wrote: %+v", row)
	}
}

func TestTaskRelaneNeverNeedsJobAndKeepsState(t *testing.T) {
	s := taskTestStore(t)
	task := newTask(t, s, taskLane(t), "relane jobless", 0)
	if _, err := s.ClaimTask(t.Context(), task.ID, "captain-a", "", "manual queue item"); err != nil {
		t.Fatal(err)
	}
	anchor := newTask(t, s, taskLane(t), "anchor", 0)
	moved, changed, err := s.RelaneTask(t.Context(), task.ID, anchor.Lane, "tester", "triage", false)
	if err != nil || !changed {
		t.Fatalf("relane changed=%v err=%v", changed, err)
	}
	if moved.State != "claimed" || moved.ClaimedBy != "captain-a" || moved.Refs.JobID != "" {
		t.Fatalf("relane touched linkage: %+v", moved)
	}
}

func TestTaskDecisionResolveOnJoblessTaskRecordsReason(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	c := remote.Client{URL: h.URL, Token: "node-token", HTTP: h.Client()}

	task := newTask(t, s, taskLane(t), "jobless decision", 0)
	if _, err := s.ClaimTask(t.Context(), task.ID, "captain-a", "", "decide queue item"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTask(t.Context(), task.ID, "needs_decision", "node", "pick an option", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ResolveDecision(t.Context(), "task", task.ID, "operator-desk", "A", "", true, "caller says jobless"); err != nil {
		t.Fatal(err)
	}
	row, _, _ := s.GetTask(t.Context(), task.ID)
	if row.State != "claimed" {
		t.Fatalf("resolved state=%s", row.State)
	}
	last := row.Events[len(row.Events)-1]
	if last.To != "claimed" || last.NoJob != "caller says jobless" {
		t.Fatalf("resolve event=%+v", last)
	}
}

// Dispositions are the system's own jobless items: every claimed/in_progress
// hop their internal SQL makes must still carry a recorded reason.
func TestDispositionInternalHopsRecordNoJob(t *testing.T) {
	s := taskTestStore(t)
	item := mustCreateDisposition(t, s, dispositionInput(taskLane(t), dispositionPR(t), 0, "A"))
	t.Cleanup(func() { drainOpenDispositions(t, s) })

	row, _, _ := s.GetTask(t.Context(), item.ID)
	if len(row.Events) != 2 || row.Events[0].To != "claimed" || row.Events[0].NoJob == "" {
		t.Fatalf("create hops=%+v want recorded no_job on the claimed hop", row.Events)
	}
	if _, err := s.AnswerDisposition(t.Context(), store.DispositionAnswerInput{ID: item.ID, Gen: openGen(t, s, item.ID), Key: "A", OperatorEmail: "op@example.com", EventID: "evt-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyDisposition(t.Context(), item.ID, "director", "apply"); err != nil {
		t.Fatal(err)
	}
	row, _, _ = s.GetTask(t.Context(), item.ID)
	for _, e := range row.Events {
		if (e.To == "claimed" || e.To == "in_progress") && e.NoJob == "" {
			t.Fatalf("active hop without recorded reason: %+v", e)
		}
	}
}

func TestTaskNoJobWireCompatWithOldServer(t *testing.T) {
	// A pre-change server decodes with DisallowUnknownFields: bodies carrying
	// no_job fail with the generic invalid_context, and the client translates
	// it into a named refusal instead of reporting a fake success.
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_context"}`)
	}))
	defer h.Close()
	c := remote.Client{URL: h.URL, Token: "t", HTTP: h.Client()}
	if _, err := c.TransitionTask(t.Context(), 7, "claimed", "", nil, "reason"); err == nil || !strings.Contains(err.Error(), "transition_no_job_rejected") {
		t.Fatalf("transition err=%v, want transition_no_job_rejected", err)
	}
	if _, err := c.ClaimTask(t.Context(), 7, "captain-a", "", "reason"); err == nil || !strings.Contains(err.Error(), "claim_no_job_rejected") {
		t.Fatalf("claim err=%v, want claim_no_job_rejected", err)
	}
	if _, err := c.NextTask(t.Context(), "lane", "captain-a", "", "reason"); err == nil || !strings.Contains(err.Error(), "next_no_job_rejected") {
		t.Fatalf("next err=%v, want next_no_job_rejected", err)
	}
	if _, err := c.ResolveDecision(t.Context(), "task", 7, "by", "A", "", true, "reason"); err == nil || !strings.Contains(err.Error(), "resolve_no_job_rejected") {
		t.Fatalf("resolve err=%v, want resolve_no_job_rejected", err)
	}
	// Without the flag the same server error stays the raw invalid_context.
	if _, err := c.TransitionTask(t.Context(), 7, "claimed", "", nil, ""); err == nil || err.Error() != "invalid_context" {
		t.Fatalf("plain transition err=%v, want invalid_context", err)
	}
}

// A 200 whose body contradicts the request is a lie: the client names it
// rather than reporting success on a write that never landed.
func TestTaskClientRejectsContradictory200(t *testing.T) {
	var h *httptest.Server
	h = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		state := "claimed"
		refs := map[string]any{}
		if strings.Contains(r.URL.Path, "/transition") {
			// Claim applied but the asked-for state did not.
			state = "backlog"
		}
		fmt.Fprintf(w, `{"id":7,"lane":"x","title":"t","state":%q,"claimed_by":"captain-a","refs":%s,"priority":0,"kind":"implement","created_by":"x","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`, state, mustJSON(refs))
	}))
	defer h.Close()
	c := remote.Client{URL: h.URL, Token: "t", HTTP: h.Client()}
	if _, err := c.TransitionTask(t.Context(), 7, "in_progress", "", nil, ""); err == nil || !strings.Contains(err.Error(), "transition_not_applied") {
		t.Fatalf("transition err=%v, want transition_not_applied", err)
	}
	if _, err := c.ClaimTask(t.Context(), 7, "captain-a", "job-1", ""); err == nil || !strings.Contains(err.Error(), "claim_job_id_not_recorded") {
		t.Fatalf("claim err=%v, want claim_job_id_not_recorded", err)
	}
	// A replayed claim on an already-active row is honest: in_progress is
	// accepted when claimant and job match the request.
	h2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":7,"lane":"x","title":"t","state":"in_progress","claimed_by":"captain-a","refs":{"job_id":"job-1"},"priority":0,"kind":"implement","created_by":"x","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`)
	}))
	defer h2.Close()
	c2 := remote.Client{URL: h2.URL, Token: "t", HTTP: h2.Client()}
	if got, err := c2.ClaimTask(t.Context(), 7, "captain-a", "job-1", ""); err != nil || got.State != "in_progress" {
		t.Fatalf("replayed claim got=%v err=%v", got.State, err)
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// The console detail endpoint (/ui/api/board/tasks/<id>) must project
// no_job through to the drawer — a recorded reason the UI drops is an
// invisible exemption.
func TestTaskBoardDetailProjectsNoJob(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)

	task := createUITask(t, s, uiLane(t, "lane-a"), "projected reason")
	if _, err := s.ClaimTask(t.Context(), task.ID, "captain-a", "", "manual queue item"); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Events []struct {
			To    string `json:"to"`
			NoJob string `json:"no_job"`
		} `json:"events"`
	}
	url := h.URL + "/ui/api/board/tasks/" + fmt.Sprint(task.ID)
	if status := boardJSON(t, h.Client(), url, assertion, &got); status != http.StatusOK {
		t.Fatalf("detail status=%d", status)
	}
	if len(got.Events) != 1 || got.Events[0].NoJob != "manual queue item" {
		t.Fatalf("detail events=%+v want the recorded no_job", got.Events)
	}
}

// The schema carries no_job and linked rows leave it empty.
func TestTaskEventsNoJobColumnDefaultsEmpty(t *testing.T) {
	s := taskTestStore(t)
	task := newTask(t, s, taskLane(t), "column default", 0)
	if _, err := s.ClaimTask(t.Context(), task.ID, "captain-a", "job-1", ""); err != nil {
		t.Fatal(err)
	}
	row, found, err := s.GetTask(t.Context(), task.ID)
	if err != nil || !found || len(row.Events) != 1 || row.Events[0].NoJob != "" {
		t.Fatalf("events=%+v found=%v err=%v", row.Events, found, err)
	}
}

// A reason made only of invisible Unicode (zero-width space, BOM) renders
// blank and must be refused like empty; visible reasons still record.
func TestTaskNoJobReasonInvisibleCharsRefused(t *testing.T) {
	s := taskTestStore(t)
	for _, reason := range []string{"\u200B", "\uFEFF", " \u200B \uFEFF ", "\u2007\u200B\u00AD"} {
		task := newTask(t, s, taskLane(t), "invisible", 0)
		if _, err := s.TransitionTask(t.Context(), task.ID, "claimed", "captain-a", "", nil, reason); !errors.Is(err, store.ErrTaskJobRequired) {
			t.Fatalf("invisible reason %q transition err=%v want ErrTaskJobRequired", reason, err)
		}
		if _, err := s.ClaimTask(t.Context(), task.ID, "captain-a", "", reason); !errors.Is(err, store.ErrTaskJobRequired) {
			t.Fatalf("invisible reason %q err=%v want ErrTaskJobRequired", reason, err)
		}
	}
	// A visible reason keeps its content even with leading invisible runes.
	task := newTask(t, s, taskLane(t), "visible", 0)
	got, err := s.ClaimTask(t.Context(), task.ID, "captain-a", "", "\u200Bmanual queue item")
	if err != nil {
		t.Fatal(err)
	}
	if row, found, err := s.GetTask(t.Context(), task.ID); err != nil || !found || len(row.Events) != 1 || row.Events[0].NoJob != "manual queue item" {
		t.Fatalf("events=%+v found=%v err=%v", row.Events, found, err)
	} else if got.State != "claimed" {
		t.Fatalf("state=%s", got.State)
	}
}

// A successful-looking response that lands in an active state without the
// claimant or job link is refused rather than reported as linked work.
func TestTaskClientRejectsUnlinkedActive200(t *testing.T) {
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":9,"lane":"x","title":"t","state":"claimed","claimed_by":"intruder","refs":{"job_id":"job-1"},"priority":0,"kind":"implement","created_by":"x","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`)
	}))
	defer h.Close()
	c := remote.Client{URL: h.URL, Token: "t", HTTP: h.Client()}
	if _, err := c.ClaimTask(t.Context(), 9, "captain-a", "job-1", ""); err == nil || !strings.Contains(err.Error(), "claimant_not_recorded") {
		t.Fatalf("claim err=%v", err)
	}
	if _, err := c.NextTask(t.Context(), "lane-a", "captain-a", "job-1", ""); err == nil || !strings.Contains(err.Error(), "claimant_not_recorded") {
		t.Fatalf("next err=%v", err)
	}
	h2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":9,"lane":"x","title":"t","state":"claimed","claimed_by":"","refs":{},"priority":0,"kind":"implement","created_by":"x","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`)
	}))
	defer h2.Close()
	c2 := remote.Client{URL: h2.URL, Token: "t", HTTP: h2.Client()}
	if _, err := c2.TransitionTask(t.Context(), 9, "claimed", "captain-a", &store.TaskRefs{JobID: "job-1"}, ""); err == nil || !strings.Contains(err.Error(), "linkage_missing") {
		t.Fatalf("transition err=%v", err)
	}
}

// A resolve whose no-job reason can never be recorded must be refused
// before the lane event commits — otherwise the task stays needs_decision
// with a resolved answer already queued for its lane.
func TestDecisionResolveBadNoJobNoPartialWrite(t *testing.T) {
	s := taskTestStore(t)
	svc := api.Service{Store: s}
	lane := taskLane(t)
	task := newTask(t, s, lane, "resolve partial write", 0)
	if _, err := s.ClaimTask(t.Context(), task.ID, "captain-a", "", "decide queue item"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTask(t.Context(), task.ID, "needs_decision", "node", "pick", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResolveDecision(t.Context(), api.DecisionResolveInput{Type: "task", ID: task.ID, By: "operator-desk", Answer: "A", NoJob: "sk-abcdefghijklmnopqrstuvwxyz"}); err == nil {
		t.Fatal("secret-like no_job reason resolved")
	}
	row, found, err := s.GetTask(t.Context(), task.ID)
	if err != nil || !found || row.State != "needs_decision" {
		t.Fatalf("state=%v found=%v err=%v", row.State, found, err)
	}
	events, err := s.ListRelayEvents(t.Context(), lane, false, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == "lane.event" && strings.Contains(e.Text, "resolved by operator-desk") {
			t.Fatalf("refused resolve still queued a lane event: %+v", e)
		}
	}
	// An invisible reason normalizes to empty and takes the path fallback,
	// so the resolve completes with a recorded reason, not a dangling event.
	got, err := svc.ResolveDecision(t.Context(), api.DecisionResolveInput{Type: "task", ID: task.ID, By: "operator-desk", Answer: "A", NoJob: "​"})
	if err != nil {
		t.Fatal(err)
	}
	if row, _, _ := s.GetTask(t.Context(), task.ID); row.State != "claimed" || row.Events[len(row.Events)-1].NoJob != "decision resolved; task has no job link" {
		t.Fatalf("state=%s events=%+v", row.State, row.Events)
	}
	_ = got
}
