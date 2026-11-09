package store

import (
	"testing"
	"time"
)

func TestImportUpdatedPagesUseTimestampAndIDCursors(t *testing.T) {
	s := schemaStore(t, "import_updates")
	ctx := t.Context()
	cutoff := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	oldAt, newAt := cutoff.Add(-time.Hour), cutoff.Add(time.Hour)
	var oldTask, newTask int64
	for _, tc := range []struct {
		title string
		at    time.Time
		dst   *int64
	}{{"old", oldAt, &oldTask}, {"new", newAt, &newTask}} {
		if err := s.pool.QueryRow(ctx, `INSERT INTO tasks(lane,title,kind,state,created_by,created_at,updated_at,last_activity_at,project) VALUES('import-test',$1,'implement','backlog','test',$2,$2,$2,'import-test') RETURNING id`, tc.title, tc.at).Scan(tc.dst); err != nil {
			t.Fatal(err)
		}
	}
	tasks, err := s.ImportTasksUpdatedPage(ctx, ptrString("import-test"), cutoff, 0, 10)
	if err != nil || len(tasks) != 1 || tasks[0].ID != newTask {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO task_comments(task_id,body,author,created_at) VALUES($1,'old','tester',$2),($1,'new','tester',$3)`, newTask, oldAt, newAt); err != nil {
		t.Fatal(err)
	}
	comments, err := s.ImportCommentsUpdatedPage(ctx, []int64{newTask}, cutoff, 0, 10)
	if err != nil || len(comments) != 1 || comments[0].Body != "new" {
		t.Fatalf("comments=%+v err=%v", comments, err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO documents(key,kind,body,sha256,created_by,created_at,updated_at,task_id) VALUES('import-test/old','note','old',$1,'tester',$2,$2,$3),('import-test/new','note','new',$1,'tester',$4,$4,$3)`, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", oldAt, newTask, newAt); err != nil {
		t.Fatal(err)
	}
	docs, err := s.ImportDocumentsUpdatedPage(ctx, "import-test", cutoff, 0, 10)
	if err != nil || len(docs) != 1 || docs[0].Body != "new" {
		t.Fatalf("docs=%+v err=%v", docs, err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO decision_requests(source,source_ref,question,created_at,updated_at) VALUES('chat','old','old',$1,$1),('chat','new','new',$2,$2)`, oldAt, newAt); err != nil {
		t.Fatal(err)
	}
	decisions, err := s.ImportDecisionsUpdatedPage(ctx, cutoff, 0, 10, newAt)
	if err != nil || len(decisions) != 1 || decisions[0].Question != "new" {
		t.Fatalf("decisions=%+v err=%v", decisions, err)
	}
}

func ptrString(value string) *string { return &value }
