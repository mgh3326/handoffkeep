package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestWorkBoardConsoleProjection(t *testing.T) {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	board := store.LinearBoard{Task: store.Task{ID: 101, State: "in_progress", Refs: store.TaskRefs{JobID: "example-lead", PR: "https://github.com/example/project/pull/42", HeadSHA: "example-head"}}, Sessions: []store.LinearBoardSession{
		{JobID: "example-lead", Host: "example-a", Role: "lead", Family: "openai", Model: "example-builder", At: at, Active: false, Pane: "private-pane", Profile: "private-profile", Verdict: "private-verdict"},
		{JobID: "example-review", Host: "example-b", Role: "review", Family: "anthropic", Model: "example-reviewer", At: at, Active: true},
		{JobID: "example-consult", Host: "example-c", Role: "consult", Model: "example-consultant", At: at, Active: true},
		{JobID: "old-worker", Host: "old-host", Role: "worker", At: at, Active: false},
	}}
	view := projectWorkBoard(board)
	if view.PrimaryJobID != "example-lead" || view.PR != board.Task.Refs.PR || view.HeadSHA != "example-head" || len(view.Sessions) != 3 {
		t.Fatalf("projection=%+v", view)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private-pane", "private-profile", "private-verdict", "old-worker", "account_key", "candidates", "power"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("unexpected field %s in %s", private, raw)
		}
	}
	if view.Sessions[0].Host != "example-a" || view.Sessions[0].Model != "example-builder" || view.Sessions[1].Role != "review" {
		t.Fatal(view)
	}
	if projectWorkBoard(store.LinearBoard{}) != nil {
		t.Fatal("empty projection must preserve old-server fallback")
	}
}

func TestBoardWorkBoardListAndDetail(t *testing.T) {
	_, url := testdb.Schema(t, "ui_work_board")
	st, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	project := "experiment"
	task, err := st.CreateTask(t.Context(), store.Task{Lane: "example-project", Title: "Example console work", Kind: "implement", Project: &project, CreatedBy: "example-operator"})
	if err != nil {
		t.Fatal(err)
	}
	host := "example-host"
	_, _, err = st.RecordPlacement(t.Context(), store.PlacementDecision{JobID: "example-lead", Attempt: 1, TaskID: &task.ID, DecidedAt: time.Now().UTC(), DecidedOn: "example-reporter", Source: "forced", Ranking: "forced", StateSource: "hub", ChosenHost: &host, Outcome: "placed", Family: "openai", Model: "example-model", Role: "lead", Candidates: []store.PlacementCandidate{}})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{store: st}
	for _, detail := range []bool{false, true} {
		rec := httptest.NewRecorder()
		if detail {
			h.boardTaskDetail(rec, httptest.NewRequest(http.MethodGet, "/ui/api/board/tasks/"+strconv.FormatInt(task.ID, 10), nil))
		} else {
			h.boardTasks(rec, httptest.NewRequest(http.MethodGet, "/ui/api/board/tasks?lane=example-project", nil))
		}
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"work_board"`) || !strings.Contains(rec.Body.String(), `"host":"example-host"`) || !strings.Contains(rec.Body.String(), `"model":"example-model"`) {
			t.Fatalf("detail=%t status=%d body=%s", detail, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "example-reporter") || strings.Contains(rec.Body.String(), "candidates") {
			t.Fatal("raw placement leaked", rec.Body.String())
		}
	}
}
