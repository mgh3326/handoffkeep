package tests

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// #782: tasks claim binds claimed_by and refs.job_id in one transaction so a
// spawn consumer can rely on the link being present in the claim response.

func TestTaskClaimWithJobIDRecordsBothAtomically(t *testing.T) {
	s := taskTestStore(t)
	task := newTask(t, s, taskLane(t), "claim with job", 0)
	h := taskHTTP(s)
	defer h.Close()
	c := remote.Client{URL: h.URL, Token: "node-token", HTTP: h.Client()}

	got, err := c.ClaimTask(t.Context(), task.ID, "captain-a", "job-782-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "claimed" || got.ClaimedBy != "captain-a" || got.Refs.JobID != "job-782-1" {
		t.Fatalf("claim response=%+v", got)
	}

	row, found, err := s.GetTask(t.Context(), task.ID)
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if row.State != "claimed" || row.ClaimedBy != "captain-a" || row.Refs.JobID != "job-782-1" {
		t.Fatalf("stored task=%+v", row)
	}
	if len(row.Events) != 1 || row.Events[0].From != "backlog" || row.Events[0].To != "claimed" {
		t.Fatalf("events=%+v", row.Events)
	}
	if row.Events[0].Refs == nil || row.Events[0].Refs.JobID != "job-782-1" {
		t.Fatalf("claim event refs=%+v, want job_id recorded", row.Events[0].Refs)
	}
}

func TestTaskClaimWithoutJobIDRequiresNoJobReason(t *testing.T) {
	s := taskTestStore(t)
	task := newTask(t, s, taskLane(t), "claim without job", 0)
	h := taskHTTP(s)
	defer h.Close()
	c := remote.Client{URL: h.URL, Token: "node-token", HTTP: h.Client()}

	// A bare claim — no job_id, no no_job reason — is refused outright.
	if _, err := c.ClaimTask(t.Context(), task.ID, "captain-a", "", ""); err == nil {
		t.Fatal("bare claim succeeded; want task_job_required")
	} else {
		var he *remote.HTTPError
		if !errors.As(err, &he) || he.Status != http.StatusConflict || he.Code != "task_job_required" {
			t.Fatalf("bare claim err=%v, want 409 task_job_required", err)
		}
	}
	row, _, _ := s.GetTask(t.Context(), task.ID)
	if row.State != "backlog" || len(row.Events) != 0 {
		t.Fatalf("refused claim wrote state/events: %+v", row)
	}

	// The explicit exception: a recorded no-job reason claims the task
	// without a job link.
	got, err := c.ClaimTask(t.Context(), task.ID, "captain-a", "", "manual queue item")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "claimed" || got.ClaimedBy != "captain-a" || got.Refs.JobID != "" {
		t.Fatalf("claim response=%+v", got)
	}
	row, _, _ = s.GetTask(t.Context(), task.ID)
	if len(row.Events) != 1 || row.Events[0].NoJob != "manual queue item" {
		t.Fatalf("claim event=%+v, want recorded no_job reason", row.Events[0])
	}

	// A re-claim by a different lane is still a conflict; the same claimant
	// repeating its jobless claim is a no-op, but a bare claim is not.
	if _, err := c.ClaimTask(t.Context(), task.ID, "captain-b", "", "other"); err == nil {
		t.Fatal("re-claim by captain-b succeeded; want task_conflict")
	} else {
		var he *remote.HTTPError
		if !errors.As(err, &he) || he.Status != http.StatusConflict || he.Code != "task_conflict" {
			t.Fatalf("re-claim by captain-b err=%v", err)
		}
	}
	if _, err := c.ClaimTask(t.Context(), task.ID, "captain-a", "", "manual queue item"); err != nil {
		t.Fatalf("identical jobless re-claim should be a no-op: %v", err)
	}
	if _, err := c.ClaimTask(t.Context(), task.ID, "captain-a", "", ""); err == nil {
		t.Fatal("bare re-claim succeeded; want task_conflict")
	}
	row, _, _ = s.GetTask(t.Context(), task.ID)
	if row.Refs.JobID != "" || len(row.Events) != 1 {
		t.Fatalf("task after conflicts=%+v", row)
	}
}

func TestTaskClaimSameLaneSameJobIsIdempotentNoOp(t *testing.T) {
	s := taskTestStore(t)
	task := newTask(t, s, taskLane(t), "idempotent claim", 0)

	first, err := s.ClaimTask(t.Context(), task.ID, "captain-a", "job-1", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ClaimTask(t.Context(), task.ID, "captain-a", "job-1", "")
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if second.ID != first.ID || second.State != "claimed" || second.ClaimedBy != "captain-a" || second.Refs.JobID != "job-1" {
		t.Fatalf("re-claim response=%+v", second)
	}
	if !second.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("no-op must not touch updated_at: first=%v second=%v", first.UpdatedAt, second.UpdatedAt)
	}
	row, _, _ := s.GetTask(t.Context(), task.ID)
	if len(row.Events) != 1 {
		t.Fatalf("no-op must not add events, got %+v", row.Events)
	}

	// The same replay is still a no-op after the task moved on.
	if _, err := s.TransitionTask(t.Context(), task.ID, "in_progress", "captain-a", "", nil, ""); err != nil {
		t.Fatal(err)
	}
	again, err := s.ClaimTask(t.Context(), task.ID, "captain-a", "job-1", "")
	if err != nil {
		t.Fatalf("replay on in_progress: %v", err)
	}
	if again.State != "in_progress" || again.ClaimedBy != "captain-a" || again.Refs.JobID != "job-1" {
		t.Fatalf("replay response=%+v", again)
	}
	row, _, _ = s.GetTask(t.Context(), task.ID)
	if len(row.Events) != 2 {
		t.Fatalf("replay must not add events, got %+v", row.Events)
	}
}

func TestTaskClaimConflictsCarryReasonAndWriteNothing(t *testing.T) {
	s := taskTestStore(t)
	task := newTask(t, s, taskLane(t), "conflict matrix", 0)
	if _, err := s.ClaimTask(t.Context(), task.ID, "captain-a", "job-1", ""); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, by, jobID, want string
	}{
		{"different lane same job", "captain-b", "job-1", "already claimed by captain-a"},
		{"different lane different job", "captain-b", "job-2", "already claimed by captain-a"},
		{"same lane different job", "captain-a", "job-2", "already claimed with job_id job-1"},
		{"same lane no job", "captain-a", "", "already claimed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.ClaimTask(t.Context(), task.ID, tc.by, tc.jobID, "")
			if !errors.Is(err, store.ErrTaskConflict) {
				t.Fatalf("err=%v, want task_conflict", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%q, want reason containing %q", err.Error(), tc.want)
			}
		})
	}
	// Every failure path is read-only: the row keeps its first claim.
	row, _, _ := s.GetTask(t.Context(), task.ID)
	if row.State != "claimed" || row.ClaimedBy != "captain-a" || row.Refs.JobID != "job-1" || len(row.Events) != 1 {
		t.Fatalf("task after failed claims=%+v", row)
	}

	// A task claimed without a job refuses a later claim carrying one.
	noJob := newTask(t, s, taskLane(t), "claimed without job", 0)
	if _, err := s.ClaimTask(t.Context(), noJob.ID, "captain-c", "", "manual queue item"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimTask(t.Context(), noJob.ID, "captain-c", "job-9", ""); !errors.Is(err, store.ErrTaskConflict) {
		t.Fatalf("attach job on claimed task err=%v, want task_conflict", err)
	}
}

func TestTaskClaimMissingTaskStillConflicts(t *testing.T) {
	s := taskTestStore(t)
	if _, err := s.ClaimTask(t.Context(), 424242, "captain-a", "job-1", ""); !errors.Is(err, store.ErrTaskConflict) {
		t.Fatalf("missing task err=%v, want task_conflict", err)
	}
	if _, err := s.ClaimTask(t.Context(), 424242, "captain-a", "job-1", ""); !errors.Is(err, store.ErrTaskConflict) {
		t.Fatalf("missing task err=%v, want task_conflict", err)
	}
}

func TestTaskClaimConcurrentIdenticalClaimsAllSucceedOnce(t *testing.T) {
	s := taskTestStore(t)
	task := newTask(t, s, taskLane(t), "concurrent identical", 0)
	h := taskHTTP(s)
	defer h.Close()
	c := remote.Client{URL: h.URL, Token: "node-token", HTTP: h.Client()}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, conflicts := 0, 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.ClaimTask(t.Context(), task.ID, "captain-a", "job-same", "")
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
			} else {
				var he *remote.HTTPError
				if errors.As(err, &he) && he.Code == "task_conflict" {
					conflicts++
				} else {
					t.Errorf("claim: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	if wins != 16 || conflicts != 0 {
		t.Fatalf("identical concurrent claims: wins=%d conflicts=%d, want 16/0", wins, conflicts)
	}
	row, _, _ := s.GetTask(t.Context(), task.ID)
	if row.ClaimedBy != "captain-a" || row.Refs.JobID != "job-same" || len(row.Events) != 1 {
		t.Fatalf("task=%+v", row)
	}
}

func TestTaskClaimHTTPConflictBodyCarriesReason(t *testing.T) {
	s := taskTestStore(t)
	task := newTask(t, s, taskLane(t), "http reason", 0)
	h := taskHTTP(s)
	defer h.Close()
	resp := request(t, h.Client(), http.MethodPost, h.URL+"/v1/tasks/"+fmt.Sprint(task.ID)+"/claim", "node-token", map[string]string{"claimed_by": "captain-a", "job_id": "job-1"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim status=%d", resp.StatusCode)
	}
	var got store.Task
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.ClaimedBy != "captain-a" || got.Refs.JobID != "job-1" {
		t.Fatalf("claim body=%+v", got)
	}

	resp2 := request(t, h.Client(), http.MethodPost, h.URL+"/v1/tasks/"+fmt.Sprint(task.ID)+"/claim", "node-token", map[string]string{"claimed_by": "captain-b", "job_id": "job-2"})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("conflict status=%d", resp2.StatusCode)
	}
	var body struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Error != "task_conflict" || !strings.Contains(body.Reason, "already claimed by captain-a") {
		t.Fatalf("conflict body=%+v", body)
	}
}

func TestTaskClaimClientDetectsServerDroppingJobID(t *testing.T) {
	// A pre-#782 server answers 200 with the task but without refs.job_id.
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":7,"lane":"l","title":"t","kind":"implement","state":"claimed","priority":0,"refs":{},"claimed_by":"captain-a","created_by":"x","created_at":"2026-09-27T00:00:00Z","updated_at":"2026-09-27T00:00:00Z"}`)
	}))
	defer h.Close()
	c := remote.Client{URL: h.URL, Token: "t", HTTP: h.Client()}
	if _, err := c.ClaimTask(t.Context(), 7, "captain-a", "job-1", ""); err == nil || !strings.Contains(err.Error(), "claim_job_id_not_recorded") {
		t.Fatalf("dropped job_id err=%v, want claim_job_id_not_recorded", err)
	}
	// Without --job-id the same old server is fine: behaviour unchanged.
	if _, err := c.ClaimTask(t.Context(), 7, "captain-a", "", ""); err != nil {
		t.Fatalf("claim without job_id on old server: %v", err)
	}
}

func TestTaskClaimClientTranslatesUnknownFieldRejection(t *testing.T) {
	// The deployed pre-#782 server decodes with DisallowUnknownFields, so a
	// claim carrying job_id fails with the generic invalid_context instead
	// of claiming the task. The client must still fail clearly.
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_context"}`)
	}))
	defer h.Close()
	c := remote.Client{URL: h.URL, Token: "t", HTTP: h.Client()}
	if _, err := c.ClaimTask(t.Context(), 7, "captain-a", "job-1", ""); err == nil || !strings.Contains(err.Error(), "claim_job_id_rejected") {
		t.Fatalf("rejected claim err=%v, want claim_job_id_rejected", err)
	}
	// Without --job-id the same response stays the raw server error.
	if _, err := c.ClaimTask(t.Context(), 7, "captain-a", "", ""); err == nil || err.Error() != "invalid_context" {
		t.Fatalf("claim without job_id err=%v, want invalid_context", err)
	}
}
