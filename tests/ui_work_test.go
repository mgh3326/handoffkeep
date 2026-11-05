package tests

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestUIWorkRecords(t *testing.T) {
	_, url := testdb.Schema(t, "ui_work")
	s, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	pool, err := pgxpool.New(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	suffix := fmt.Sprint(time.Now().UnixNano())
	projects := []string{"work-a-" + suffix, "work-b-" + suffix}
	var tasks []store.Task
	for _, project := range projects {
		if _, err := s.AddTaskProject(t.Context(), project, "test"); err != nil {
			t.Fatal(err)
		}
		task, err := s.CreateTask(t.Context(), store.Task{Lane: "work-test", Title: "work record", Kind: "implement", CreatedBy: "test", Project: &project})
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, task)
	}
	task := tasks[0]
	if _, _, err := s.AddTaskOrigin(t.Context(), task.ID, "test", store.TaskOriginInput{Provider: "github", Namespace: "example/repo", ExternalID: "42", DisplayRef: "#42"}); err != nil {
		t.Fatal(err)
	}
	var changeID int64
	if err := pool.QueryRow(t.Context(), `INSERT INTO changes(repo,pr_number,state,opened_at,created_at,updated_at) VALUES('example/repo',42,'open',now(),now(),now()) RETURNING id`).Scan(&changeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO change_tasks(change_id,task_id,relation,created_at) VALUES($1,$2,'implements',now())`, changeID, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO reviews(change_id,reviewed_head,round,pass,lens,high,recorded_at) VALUES($1,'head',1,1,'security',NULL,now()),($1,'head',1,1,'correctness',2,now())`, changeID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 101; i++ {
		id := fmt.Sprintf("work-%s-%03d", suffix, i)
		if _, _, err := s.ClaimJob(t.Context(), store.Job{JobID: id, TaskID: &task.ID, Host: "test-host", CreatedBy: "test"}); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE jobs SET skills='[{"name":"test","source":"local","version_or_hash":"v1","calls":2}]'::jsonb, skills_supported=true WHERE job_id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	var list struct {
		Tasks []struct {
			ID int64 `json:"id"`
		} `json:"tasks"`
	}
	if status := boardJSON(t, h.Client(), h.URL+"/ui/api/board/tasks?project="+projects[0], assertion, &list); status != http.StatusOK || len(list.Tasks) != 1 || list.Tasks[0].ID != task.ID {
		t.Fatalf("list status=%d rows=%+v", status, list.Tasks)
	}
	var p struct {
		Projects []string `json:"projects"`
	}
	if status := boardJSON(t, h.Client(), h.URL+"/ui/api/board/projects", assertion, &p); status != http.StatusOK || !strings.Contains(strings.Join(p.Projects, ","), projects[0]) || !strings.Contains(strings.Join(p.Projects, ","), projects[1]) {
		t.Fatalf("projects=%+v status=%d", p, status)
	}
	var d map[string]any
	path := fmt.Sprintf("%s/ui/api/board/tasks/%d", h.URL, task.ID)
	if status := boardJSON(t, h.Client(), path, assertion, &d); status != http.StatusOK {
		t.Fatalf("detail status=%d", status)
	}
	for _, key := range []string{"task", "events", "dwell", "linear", "participants", "decision_requests", "origins", "changes", "jobs"} {
		if _, ok := d[key]; !ok {
			t.Errorf("missing %s", key)
		}
	}
	if len(d["origins"].([]any)) != 1 || len(d["changes"].([]any)[0].(map[string]any)["reviews"].([]any)) != 2 || len(d["jobs"].([]any)) != 100 || d["jobs_truncated"] != true {
		t.Fatalf("detail=%v", d)
	}
	for _, j := range d["jobs"].([]any) {
		job := j.(map[string]any)
		if _, ok := job["result"]; ok {
			t.Fatal("result leaked")
		}
		if _, ok := job["usage"]; ok {
			t.Fatal("usage leaked")
		}
		if len(job["skills"].([]any)) != 1 {
			t.Fatal("missing skills")
		}
	}
	if _, err := pool.Exec(t.Context(), `ALTER TABLE task_origins RENAME COLUMN provider TO invalid_provider`); err != nil {
		t.Fatal(err)
	}
	res := uiRequest(t, h.Client(), http.MethodGet, path, assertion, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusInternalServerError || !strings.Contains(responseText(t, res), `"error":"internal_error"`) {
		t.Fatal("expected JSON 500")
	}
}
