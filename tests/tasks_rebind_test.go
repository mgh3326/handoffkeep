package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestTaskRebindRouteChangesJobAndReturnsAuditEvent(t *testing.T) {
	s := taskTestStore(t)
	task := newTask(t, s, taskLane(t), "rebind route", 0)
	if _, err := s.ClaimTask(t.Context(), task.ID, "old-builder", "job-old", ""); err != nil {
		t.Fatal(err)
	}
	h := taskHTTP(s)
	defer h.Close()

	resp := request(t, h.Client(), http.MethodPost, h.URL+"/v1/tasks/"+fmt.Sprint(task.ID)+"/rebind", "node-token", map[string]string{
		"from_job": "job-old", "to_job": "job-new", "by": "new-builder", "reason": "builder takeover",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rebind status=%d", resp.StatusCode)
	}
	var rebound store.Task
	if err := json.NewDecoder(resp.Body).Decode(&rebound); err != nil {
		t.Fatal(err)
	}
	if rebound.State != "claimed" || rebound.ClaimedBy != "new-builder" || rebound.Refs.JobID != "job-new" {
		t.Fatalf("rebind response=%+v", rebound)
	}
	row, found, err := s.GetTask(t.Context(), task.ID)
	if err != nil || !found || len(row.Events) != 2 {
		t.Fatalf("task events=%+v found=%v err=%v", row.Events, found, err)
	}
	if event := row.Events[1]; event.From != "claimed" || event.To != "claimed" || event.By != "new-builder" ||
		event.Note != "builder takeover" || event.Rebind == nil || event.Rebind.FromJob != "job-old" ||
		event.Rebind.ToJob != "job-new" || event.Rebind.By != "new-builder" || event.Rebind.Reason != "builder takeover" {
		t.Fatalf("rebind event=%+v", event)
	}

	conflict := request(t, h.Client(), http.MethodPost, h.URL+"/v1/tasks/"+fmt.Sprint(task.ID)+"/rebind", "node-token", map[string]string{
		"from_job": "job-old", "to_job": "job-next", "by": "third-builder", "reason": "stale takeover",
	})
	defer conflict.Body.Close()
	if conflict.StatusCode != http.StatusConflict {
		t.Fatalf("stale rebind status=%d", conflict.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(conflict.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "task_conflict" || !strings.Contains(body["reason"], `current job_id is "job-new"`) {
		t.Fatalf("conflict response=%v", body)
	}
}
