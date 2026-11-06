package store

import (
	"testing"
	"time"
)

func TestReadActiveJobs(t *testing.T) {
	st := schemaStore(t, "active_jobs")
	first := linearTask(t, st, TaskRefs{})
	second := linearTask(t, st, TaskRefs{})
	third := linearTask(t, st, TaskRefs{})
	fourth := linearTask(t, st, TaskRefs{})
	ctx := t.Context()
	for _, job := range []struct {
		id   string
		task int64
	}{
		{"old", first.ID}, {"tie-a", first.ID}, {"tie-b", first.ID}, {"terminal", first.ID}, {"other", second.ID}, {"unrequested", third.ID}, {"only-done", fourth.ID},
	} {
		if _, _, err := st.ClaimJob(ctx, Job{JobID: job.id, TaskID: &job.task, Host: "example-host", CreatedBy: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := st.pool.Exec(ctx, `UPDATE jobs SET created_at=$1, role='worker', model=job_id, profile='high', harness='codex', effort='high'`, at)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.pool.Exec(ctx, `UPDATE jobs SET created_at=$1 WHERE job_id='old'`, at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.pool.Exec(ctx, `UPDATE jobs SET state='completed', created_at=$1 WHERE job_id IN ('terminal', 'only-done')`, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := st.ReadActiveJobs(ctx, []int64{first.ID, second.ID, fourth.ID})
	if err != nil {
		t.Fatal(err)
	}
	got := jobs[first.ID]
	if len(jobs) != 2 || got.Model != "tie-b" || got.Role != "worker" || got.Profile != "high" || got.Host != "example-host" || got.Harness == nil || *got.Harness != "codex" || got.Effort == nil || *got.Effort != "high" || jobs[second.ID].Model != "other" {
		t.Fatalf("jobs: %+v", jobs)
	}
	empty, err := st.ReadActiveJobs(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty: %v, %v", empty, err)
	}
}
