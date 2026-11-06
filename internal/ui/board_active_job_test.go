package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestNewestLiveJob(t *testing.T) {
	now := time.Now().UTC()
	harness, effort := "codex", "high"
	jobs := []store.Job{
		{JobID: "a", State: "claimed", CreatedAt: now, Model: "old"},
		{JobID: "c", State: "completed", CreatedAt: now.Add(time.Hour)},
		{JobID: "b", State: "spawned", CreatedAt: now, Role: "worker", Model: "model", Profile: "profile", Host: "host", Harness: &harness, Effort: &effort},
	}
	got := newestLiveJob(jobs)
	if got == nil || got.Model != "model" || got.Role != "worker" || got.Profile != "profile" || got.Host != "host" || *got.Harness != harness || *got.Effort != effort {
		t.Fatalf("unexpected job: %+v", got)
	}
	if newestLiveJob(jobs[1:2]) != nil {
		t.Fatal("terminal job was included")
	}
}

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
	task, err := st.CreateTask(t.Context(), store.Task{Lane: "example", Title: "Example", Kind: "implement", CreatedBy: "test"})
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
			ActiveJob *boardActiveJob `json:"active_job"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Tasks) != 1 || response.Tasks[0].ActiveJob == nil || response.Tasks[0].ActiveJob.Model != "example-model" || response.Tasks[0].ActiveJob.Host != "example-host" {
		t.Fatalf("response: %+v", response)
	}
}
