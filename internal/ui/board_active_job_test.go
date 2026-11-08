package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestBoardListIncludesNewestLiveJob(t *testing.T) {
	_, url := testdb.Schema(t, "ui_active_job")
	pool, err := pgxpool.New(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	st, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	project := "experiment"
	task, err := st.CreateTask(t.Context(), store.Task{Lane: "example", Title: "Example", Kind: "implement", Project: &project, CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = st.ClaimJob(t.Context(), store.Job{JobID: "example-job", TaskID: &task.ID, Host: "example-host", CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(t.Context(), `UPDATE jobs SET role='worker', model='example-model', profile='high', harness='codex', effort='high' WHERE job_id='example-job'`)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{store: st}
	rec := httptest.NewRecorder()
	h.boardTasks(rec, httptest.NewRequest(http.MethodGet, "/ui/api/board/tasks?lane=example", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Tasks []struct {
			ActiveJob *store.ActiveJobSummary `json:"active_job"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Tasks) != 1 || response.Tasks[0].ActiveJob == nil || response.Tasks[0].ActiveJob.Model != "example-model" || response.Tasks[0].ActiveJob.Host != "example-host" {
		t.Fatalf("response: %+v", response)
	}
}

// The task detail serves each job's readable run summary and never the raw
// selection record (hk 3516).
func TestBoardDetailJobRun(t *testing.T) {
	_, url := testdb.Schema(t, "ui_job_run")
	st, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	project := "experiment"
	task, err := st.CreateTask(t.Context(), store.Task{Lane: "example", Title: "Example", Kind: "implement", Project: &project, CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"job-explained", "job-bare"} {
		if _, _, err := st.ClaimJob(t.Context(), store.Job{JobID: id, TaskID: &task.ID, Host: "example-host", CreatedBy: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	model, spawned := "example-model", "spawned"
	selection := `{"chosen":{"grade":"A+","why":"for task B","account_key":"codex:secret-account"},"candidates":[{"seat":"other","outcome":"dropped:role_filtered"}]}`
	if _, err := st.UpdateJob(t.Context(), "job-explained", store.JobUpdate{State: &spawned, Model: &model, Selection: json.RawMessage(selection)}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{store: st}
	rec := httptest.NewRecorder()
	h.boardTaskDetail(rec, httptest.NewRequest(http.MethodGet, "/ui/api/board/tasks/"+strconv.FormatInt(task.ID, 10), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "codex:secret-account") || strings.Contains(rec.Body.String(), `"selection"`) {
		t.Fatalf("raw selection leaked: %s", rec.Body.String())
	}
	var response struct {
		Jobs []struct {
			JobID string       `json:"job_id"`
			Run   store.JobRun `json:"run"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Jobs) != 2 {
		t.Fatalf("jobs: %+v", response.Jobs)
	}
	runs := map[string]store.JobRun{}
	for _, j := range response.Jobs {
		runs[j.JobID] = j.Run
	}
	explained, bare := runs["job-explained"], runs["job-bare"]
	if !explained.Recorded || !strings.Contains(explained.Why, "작업 등급 B에 모델 등급 A+") || len(explained.Others) != 1 || !strings.Contains(explained.What, "모델 example-model") {
		t.Fatalf("explained: %+v", explained)
	}
	if bare.Recorded || bare.Why != store.NoSelectionText {
		t.Fatalf("bare: %+v", bare)
	}
}
