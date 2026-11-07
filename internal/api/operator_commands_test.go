package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func operatorCommandFixture(t *testing.T, st *store.Store) (store.Task, string) {
	t.Helper()
	lane := fmt.Sprintf("opcmd-api-%d", time.Now().UnixNano())
	task, err := st.CreateTask(t.Context(), store.Task{Lane: lane, Title: "operator command api", Kind: "implement", CreatedBy: "opcmd-test", Project: &apiTestProject})
	if err != nil {
		t.Fatal(err)
	}
	job := "job-" + lane
	if _, _, err = st.ClaimJob(t.Context(), store.Job{JobID: job, TaskID: &task.ID, Host: "example-host", CreatedBy: "opcmd-test"}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.UpdateJob(t.Context(), job, store.JobUpdate{OwnerLane: &lane}); err != nil {
		t.Fatal(err)
	}
	return task, lane
}

func recordAPICommand(t *testing.T, st *store.Store, taskID int64, key string) store.OperatorCommand {
	t.Helper()
	payload, _ := json.Marshal(store.WorkerMessagePayload{Text: "테스트부터 확인", Intent: store.OperatorCommandIntentSteer})
	command, err := st.RecordOperatorCommand(t.Context(), store.OperatorCommandInput{
		CommandKey: key, Kind: store.OperatorCommandKindWorkerMessage, TaskID: &taskID, Payload: payload, Actor: "operator:test", Channel: "console",
	})
	if err != nil {
		t.Fatal(err)
	}
	return command
}

func TestCommandReceiptAuthAndIdempotency(t *testing.T) {
	h, st := changesServer(t)
	task, lane := operatorCommandFixture(t, st)
	command := recordAPICommand(t, st, task.ID, "opcmd-api-receipt")
	path := fmt.Sprintf("/v1/commands/%d/receipt", command.ID)
	if status, _ := changesCall(t, h, http.MethodPost, path, "", map[string]any{"state": "acked"}); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", status)
	}
	status, body := changesCall(t, h, http.MethodPost, path, "peer-token", map[string]any{"state": "acked"})
	if status != http.StatusOK || body["state"] != "acked" {
		t.Fatalf("acked: %d %v", status, body)
	}
	status, body = changesCall(t, h, http.MethodPost, path, "peer-token", map[string]any{"state": "acked"})
	if status != http.StatusOK || body["state"] != "acked" {
		t.Fatalf("acked replay: %d %v", status, body)
	}
	status, body = changesCall(t, h, http.MethodPost, path, "peer-token", map[string]any{"state": "done"})
	if status != http.StatusOK || body["state"] != "done" {
		t.Fatalf("done: %d %v", status, body)
	}
	status, body = changesCall(t, h, http.MethodPost, path, "peer-token", map[string]any{"state": "failed", "error_code": "late"})
	if status != http.StatusConflict || body["error"] != "operator_command_state" || body["reason"] != "done cannot become failed" {
		t.Fatalf("backward: %d %v", status, body)
	}
	status, body = changesCall(t, h, http.MethodPost, path, "peer-token", map[string]any{"state": "delivered"})
	if status != http.StatusBadRequest || body["error"] != "invalid_operator_command" {
		t.Fatalf("hub-owned state: %d %v", status, body)
	}
	list, err := st.ListOperatorCommands(t.Context(), lane, "", 0, 10)
	if err != nil || len(list) != 1 || list[0].State != "done" || list[0].TaskID == nil || *list[0].TaskID != task.ID {
		t.Fatalf("stored: %+v err=%v", list, err)
	}
}

func TestCommandReceiptRefusesInvalidState(t *testing.T) {
	h, st := changesServer(t)
	task, _ := operatorCommandFixture(t, st)
	command := recordAPICommand(t, st, task.ID, "opcmd-api-invalid")
	status, body := changesCall(t, h, http.MethodPost, fmt.Sprintf("/v1/commands/%d/receipt", command.ID), "peer-token", map[string]any{"state": "failed"})
	if status != http.StatusBadRequest || body["error"] != "invalid_operator_command" {
		t.Fatalf("failed without code: %d %v", status, body)
	}
	status, body = changesCall(t, h, http.MethodPost, "/v1/commands/999999/receipt", "peer-token", map[string]any{"state": "acked"})
	if status != http.StatusNotFound || body["error"] != "not_found" {
		t.Fatalf("missing: %d %v", status, body)
	}
}

func TestCommandsListOrdersById(t *testing.T) {
	h, st := changesServer(t)
	task, lane := operatorCommandFixture(t, st)
	first := recordAPICommand(t, st, task.ID, "opcmd-api-list-a")
	second := recordAPICommand(t, st, task.ID, "opcmd-api-list-b")
	status, body := changesCall(t, h, http.MethodGet, "/v1/commands?executor_lane="+lane, "peer-token", nil)
	list, _ := body["commands"].([]any)
	if status != http.StatusOK || len(list) != 2 {
		t.Fatalf("list: %d %v", status, body)
	}
	if int64(list[0].(map[string]any)["id"].(float64)) != first.ID || int64(list[1].(map[string]any)["id"].(float64)) != second.ID {
		t.Fatalf("order: %v", list)
	}
	status, body = changesCall(t, h, http.MethodGet, fmt.Sprintf("/v1/commands?executor_lane=%s&after_id=%d&limit=1", lane, first.ID), "peer-token", nil)
	list, _ = body["commands"].([]any)
	if status != http.StatusOK || len(list) != 1 || int64(list[0].(map[string]any)["id"].(float64)) != second.ID {
		t.Fatalf("after cursor: %d %v", status, body)
	}
	if status, body = changesCall(t, h, http.MethodGet, "/v1/commands", "peer-token", nil); status != http.StatusBadRequest {
		t.Fatalf("missing lane: %d %v", status, body)
	}
	if status, body = changesCall(t, h, http.MethodGet, "/v1/commands?executor_lane="+lane+"&state=bogus", "peer-token", nil); status != http.StatusBadRequest {
		t.Fatalf("bad state: %d %v", status, body)
	}
}
