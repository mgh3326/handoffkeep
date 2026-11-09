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
	if _, err := s.pool.Exec(ctx, `INSERT INTO documents(key,kind,body,sha256,created_by,created_at,updated_at,task_id) VALUES('reports/mwr-review/report.md','report','review',$1,'svc-mergewake',$2,$3,$4)`, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", oldAt, newAt, oldTask); err != nil {
		t.Fatal(err)
	}
	tasks, err := s.ImportTasksUpdatedPage(ctx, ptrString("import-test"), cutoff, 0, 10)
	if err != nil || len(tasks) != 2 || tasks[0].ID != newTask || tasks[1].ID != oldTask {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
	if want := time.Date(2026, 11, 9, 7, 17, 0, 0, time.UTC); !tasks[1].ImportUpdatedAt.Equal(want) {
		t.Fatalf("linked document backfill cursor=%v want %v", tasks[1].ImportUpdatedAt, want)
	}
	linked, err := s.ImportTaskDocumentsPage(ctx, []int64{oldTask}, 0, 10)
	if err != nil || len(linked) != 1 || linked[0].Key != "reports/mwr-review/report.md" {
		t.Fatalf("task documents=%+v err=%v", linked, err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO task_comments(task_id,body,author,created_at) VALUES($1,'old','tester',$2),($1,'new','tester',$3)`, newTask, oldAt, newAt); err != nil {
		t.Fatal(err)
	}
	commentAuthors, err := s.ImportCommentAuthorsPage(ctx, "", 10)
	if err != nil || len(commentAuthors) != 1 || commentAuthors[0].Author != "tester" {
		t.Fatalf("comment authors=%+v err=%v", commentAuthors, err)
	}
	comments, err := s.ImportCommentsUpdatedPage(ctx, []int64{newTask}, cutoff, 0, 10)
	if err != nil || len(comments) != 1 || comments[0].Body != "new" {
		t.Fatalf("comments=%+v err=%v", comments, err)
	}
	commentsSince, err := s.ImportCommentsUpdatedPage(ctx, nil, cutoff, 0, 10)
	if err != nil || len(commentsSince) != 1 || commentsSince[0].Body != "new" {
		t.Fatalf("comments since time=%+v err=%v", commentsSince, err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO documents(key,kind,body,sha256,created_by,created_at,updated_at,task_id) VALUES('import-test/old','note','old',$1,'tester',$2,$2,$3),('import-test/new','note','new',$1,'tester',$4,$4,$3)`, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", oldAt, newTask, newAt); err != nil {
		t.Fatal(err)
	}
	docs, err := s.ImportDocumentsUpdatedPage(ctx, "import-test", cutoff, 0, 10)
	if err != nil || len(docs) != 2 || !hasDocumentBody(docs, "new") || !hasDocumentBody(docs, "review") {
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

func hasDocumentBody(docs []Document, body string) bool {
	for _, doc := range docs {
		if doc.Body == body {
			return true
		}
	}
	return false
}
