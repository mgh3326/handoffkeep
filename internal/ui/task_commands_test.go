package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestTaskCommandRoute(t *testing.T) {
	ctx := context.Background()
	_, url := testdb.Schema(t, "ui_command")
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	project := "experiment"
	lane := "ui-command-lane"
	task, err := st.CreateTask(ctx, store.Task{Lane: lane, Title: "command", Kind: "implement", CreatedBy: "test", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	job := "job-ui-command"
	if _, _, err = st.ClaimJob(ctx, store.Job{JobID: job, TaskID: &task.ID, Host: "example-host", CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.UpdateJob(ctx, job, store.JobUpdate{OwnerLane: &lane}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{store: st, csrfKey: []byte("command-key")}
	person := cfaccess.Identity{Email: "test@example.com"}
	call := func(taskID int64, body string) (int, map[string]any) {
		r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("http://example.com/ui/api/tasks/%d/commands", taskID), strings.NewReader(body))
		r.Header.Set("Origin", "http://example.com")
		token, e := h.newCSRFToken(person.Email)
		if e != nil {
			t.Fatal(e)
		}
		r.AddCookie(&http.Cookie{Name: csrfCookie, Value: token})
		r.Header.Set("X-CSRF-Token", token)
		w := httptest.NewRecorder()
		h.serveAPI(w, r, person)
		var result map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &result)
		return w.Code, result
	}
	body := `{"kind":"worker.message","text":"테스트부터 확인","intent":"queue","event_key":"ui-command-key"}`
	status, created := call(task.ID, body)
	if status != http.StatusCreated || created["state"] != "recorded" || created["intent"] != "queue" || created["text"] != "테스트부터 확인" || created["kind"] != "worker.message" {
		t.Fatalf("record: %d %v", status, created)
	}
	// A replay with the same key creates no second command and returns it.
	status, replay := call(task.ID, body)
	if status != http.StatusCreated || replay["id"] != created["id"] {
		t.Fatalf("replay: %d %v", status, replay)
	}
	if status, got := call(task.ID, `{"kind":"job.stop","event_key":"ui-command-stop"}`); status != http.StatusBadRequest || got["error"] != "operator_command_not_executable" {
		t.Fatalf("reserved kind: %d %v", status, got)
	}
	// The board detail carries the ladder and the row the chip.
	r := httptest.NewRequest(http.MethodGet, fmt.Sprintf("http://example.com/ui/api/board/tasks/%d", task.ID), nil)
	w := httptest.NewRecorder()
	h.serveAPI(w, r, person)
	var detail struct {
		Commands []struct {
			ID     int64  `json:"id"`
			State  string `json:"state"`
			Intent string `json:"intent"`
			Text   string `json:"text"`
		} `json:"commands"`
		Task struct {
			Command *struct {
				ID    int64  `json:"id"`
				State string `json:"state"`
			} `json:"command"`
		} `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("detail: %s", w.Body.String())
	}
	if len(detail.Commands) != 1 || detail.Commands[0].State != "recorded" || detail.Commands[0].Text != "테스트부터 확인" {
		t.Fatalf("commands: %+v", detail.Commands)
	}
	r = httptest.NewRequest(http.MethodGet, "http://example.com/ui/api/board/tasks?limit=50", nil)
	w = httptest.NewRecorder()
	h.serveAPI(w, r, person)
	var board struct {
		Tasks []struct {
			ID      int64 `json:"id"`
			Command *struct {
				ID    int64  `json:"id"`
				State string `json:"state"`
			} `json:"command"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &board); err != nil {
		t.Fatalf("board: %s", w.Body.String())
	}
	found := false
	for _, row := range board.Tasks {
		if row.ID == task.ID {
			found = row.Command != nil && row.Command.State == "recorded"
		}
	}
	if !found {
		t.Fatalf("row chip: %+v", board.Tasks)
	}
	// A service principal gets the task detail but not the operator command
	// text or actor email.
	r = httptest.NewRequest(http.MethodGet, fmt.Sprintf("http://example.com/ui/api/board/tasks/%d", task.ID), nil)
	w = httptest.NewRecorder()
	h.serveAPI(w, r, cfaccess.Identity{ServiceName: "svc"})
	if !strings.Contains(w.Body.String(), `"commands":[]`) || !strings.Contains(w.Body.String(), `"title":"command"`) {
		t.Fatalf("service detail: %s", w.Body.String())
	}

	// A task with no live job has no executor to address.
	other, err := st.CreateTask(ctx, store.Task{Lane: "ui-command-cold", Title: "cold", Kind: "implement", CreatedBy: "test", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	if status, got := call(other.ID, `{"kind":"worker.message","text":"지시","intent":"steer","event_key":"ui-command-cold"}`); status != http.StatusConflict || got["error"] != "operator_command_no_executor" {
		t.Fatalf("no executor: %d %v", status, got)
	}
}
