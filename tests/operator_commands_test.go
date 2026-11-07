package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// TestOperatorCommandConsoleToReceipt is C3's closed path: the console records
// a worker.message, the relay delivery receipt moves it to delivered, and the
// executor's done receipt moves it to done. Nothing claims done before the
// receipt.
func TestOperatorCommandConsoleToReceipt(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	lane := taskLane(t)
	task := newTask(t, s, lane, "operator command tracer", 1)
	job := "job-" + lane
	if _, _, err := s.ClaimJob(t.Context(), store.Job{JobID: job, TaskID: &task.ID, Host: "example-host", CreatedBy: "opcmd-integration"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateJob(t.Context(), job, store.JobUpdate{OwnerLane: &lane}); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(store.WorkerMessagePayload{Text: "테스트부터 확인", Intent: store.OperatorCommandIntentSteer})
	command, err := s.RecordOperatorCommand(t.Context(), store.OperatorCommandInput{
		CommandKey: "opcmd-integration-" + lane, Kind: store.OperatorCommandKindWorkerMessage, TaskID: &task.ID,
		Payload: payload, Actor: "operator:test", Channel: "console",
	})
	if err != nil {
		t.Fatal(err)
	}

	readState := func() string {
		t.Helper()
		resp := request(t, http.DefaultClient, http.MethodGet, h.URL+"/v1/commands?executor_lane="+lane, "node-token", nil)
		defer resp.Body.Close()
		var body struct {
			Commands []store.OperatorCommand `json:"commands"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK || len(body.Commands) != 1 || body.Commands[0].ID != command.ID {
			t.Fatalf("list: %d %+v", resp.StatusCode, body)
		}
		return body.Commands[0].State
	}
	if got := readState(); got != store.OperatorCommandStateRecorded {
		t.Fatalf("recorded state=%q", got)
	}
	events, err := s.ListRelayEvents(t.Context(), lane, true, 0)
	if err != nil || len(events) != 1 || events[0].EventID != command.CommandKey {
		t.Fatalf("relay events: %+v err=%v", events, err)
	}
	resp := request(t, http.DefaultClient, http.MethodPost, fmt.Sprintf("%s/v1/relay/events/%d/delivered", h.URL, events[0].ID), "node-token", map[string]any{"machine": "example-host", "pane": "pane-1"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delivered relay: %d", resp.StatusCode)
	}
	if got := readState(); got != store.OperatorCommandStateDelivered {
		t.Fatalf("delivered state=%q", got)
	}
	resp = request(t, http.DefaultClient, http.MethodPost, fmt.Sprintf("%s/v1/commands/%d/receipt", h.URL, command.ID), "node-token", map[string]any{"state": "done", "note": "반영함"})
	body, _ := readJSON(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || body["state"] != "done" {
		t.Fatalf("receipt: %d %v", resp.StatusCode, body)
	}
	if got := readState(); got != store.OperatorCommandStateDone {
		t.Fatalf("done state=%q", got)
	}
	got, found, err := s.GetTask(t.Context(), task.ID)
	if err != nil || !found {
		t.Fatalf("task: %v %v", found, err)
	}
	var notes []string
	for _, event := range got.Events {
		if event.Source == "operator-command" {
			notes = append(notes, event.Note)
		}
	}
	joined := strings.Join(notes, "\n")
	if len(notes) != 3 || !strings.Contains(joined, "명령 기록") || !strings.Contains(joined, "명령 전달됨") || !strings.Contains(joined, "명령 완료") {
		t.Fatalf("task timeline: %q", notes)
	}
}

func readJSON(resp *http.Response) (map[string]any, error) {
	var body map[string]any
	err := json.NewDecoder(resp.Body).Decode(&body)
	return body, err
}
