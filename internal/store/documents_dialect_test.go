package store

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func init() {
	registerTaskAreaCase("documents/put-get-list", func(t *testing.T, s *Store) {
		ctx := t.Context()
		input := Document{Key: "copy/example", Kind: "brief", Body: strings.Repeat("내용", 40000), CreatedBy: "tester"}
		saved, changed, err := s.PutDocument(ctx, input)
		if err != nil || !changed || saved.ID == 0 {
			t.Fatalf("put: %+v changed=%v err=%v", saved, changed, err)
		}
		got, found, err := s.GetDocument(ctx, input.Key)
		if err != nil || !found || got.Body != input.Body {
			t.Fatalf("get: found=%v body length=%d err=%v", found, len(got.Body), err)
		}
		if _, changed, err := s.PutDocument(ctx, input); err != nil || changed {
			t.Fatalf("unchanged: changed=%v err=%v", changed, err)
		}
		list, err := s.ListDocuments(ctx, "copy/", "", "", 0, nil, 10)
		if err != nil || len(list) != 1 || list[0].ID != saved.ID {
			t.Fatalf("list: %+v err=%v", list, err)
		}
	})
	registerTaskAreaCase("translations/roundtrip", func(t *testing.T, s *Store) {
		ctx := t.Context()
		tr := Translation{Entity: "task", EntityID: "1", Field: "title", Lang: "ko", SourceSHA256: SourceSHA256("title"), Text: "제목", Translator: "tester"}
		if err := s.PutTranslations(ctx, []Translation{tr}); err != nil {
			t.Fatal(err)
		}
		got, err := s.ListTranslations(ctx, "task", []TranslationSource{{EntityID: "1", Field: "title", Original: "title"}})
		if err != nil || len(got) != 1 || got[0].Stale || got[0].Text != tr.Text {
			t.Fatalf("translations: %+v err=%v", got, err)
		}
		got, err = s.ListTranslations(ctx, "task", []TranslationSource{{EntityID: "1", Field: "title", Original: "changed"}})
		if err != nil || len(got) != 1 || !got[0].Stale {
			t.Fatalf("stale: %+v err=%v", got, err)
		}
	})
}

func init() {
	registerTaskAreaCase("comments/create-list-and-replay", func(t *testing.T, s *Store) {
		ctx := t.Context()
		task, _, err := s.IntakeTask(ctx, IntakeInput{Source: "comments-test", SourceRef: "one", Title: "Task", CreatedBy: "tester"})
		if err != nil {
			t.Fatal(err)
		}
		first, err := s.CreateTaskComment(ctx, task.ID, "tester", "first")
		if err != nil {
			t.Fatal(err)
		}
		w := MCPWrite{Tool: "task_comment", Actor: "actor", Channel: "chatgpt", Author: "tester", Key: "one"}
		second, replayed, err := s.CreateMCPTaskComment(ctx, task.ID, "second", w)
		if err != nil || replayed {
			t.Fatalf("create MCP: %+v replayed=%v err=%v", second, replayed, err)
		}
		again, replayed, err := s.CreateMCPTaskComment(ctx, task.ID, "ignored on replay", w)
		if err != nil || !replayed || again.ID != second.ID {
			t.Fatalf("replay: %+v replayed=%v err=%v", again, replayed, err)
		}
		comments, err := s.ListTaskComments(ctx, task.ID, first.ID, 10)
		if err != nil || len(comments) != 1 || comments[0].ID != second.ID {
			t.Fatalf("list: %+v err=%v", comments, err)
		}
	})
}

func init() {
	registerTaskAreaCase("documents/prune", func(t *testing.T, s *Store) {
		ctx := t.Context()
		doc, _, err := s.PutDocument(ctx, Document{Key: DocPrunePrefix + "test", Kind: "brief", Body: "prunable", CreatedBy: "tester"})
		if err != nil {
			t.Fatal(err)
		}
		tx, release, err := s.begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, s.q(`UPDATE documents SET updated_at=$1 WHERE id=$2`, `UPDATE documents SET updated_at=? WHERE id=?`), time.Now().UTC().Add(-48*time.Hour), doc.ID)
		if err == nil {
			err = tx.Commit(ctx)
		}
		_ = tx.Rollback(ctx)
		release()
		if err != nil {
			t.Fatal(err)
		}
		citer := docRefsTask(t, s, "cites a pruned document")
		if _, err := s.AddTaskDocRef(ctx, citer.ID, doc.Key, "tester"); err != nil {
			t.Fatal(err)
		}
		cutoff := time.Now().UTC().Add(-24 * time.Hour)
		res, err := s.PruneDocuments(ctx, DocPrunePrefix, cutoff, 10, false)
		if err != nil || res.Matched != 1 || res.Deleted != 0 || res.Bytes != int64(len("prunable")) {
			t.Fatalf("dry run: %+v err=%v", res, err)
		}
		res, err = s.PruneDocuments(ctx, DocPrunePrefix, cutoff, 10, true)
		if err != nil || res.Deleted != 1 {
			t.Fatalf("apply: %+v err=%v", res, err)
		}
		if n := docRefCount(t, s, doc.Key); n != 0 {
			t.Fatalf("prune left %d task reference(s) to the deleted document", n)
		}
		if _, found, err := s.GetDocument(ctx, doc.Key); err != nil || found {
			t.Fatalf("get after prune: found=%v err=%v", found, err)
		}
	})
}

func init() {
	registerTaskAreaCase("translations/missing-fields", func(t *testing.T, s *Store) {
		ctx := t.Context()
		task, _, err := s.IntakeTask(ctx, IntakeInput{Source: "translation-test", SourceRef: "missing", Title: "title", Body: "body", CreatedBy: "tester"})
		if err != nil {
			t.Fatal(err)
		}
		comment, err := s.CreateTaskComment(ctx, task.ID, "tester", "comment")
		if err != nil {
			t.Fatal(err)
		}
		tasks, err := s.MissingTaskTranslations(ctx, 10)
		if err != nil || len(tasks) != 2 || tasks[0].Field != "title" || tasks[1].Field != "body" {
			t.Fatalf("missing tasks: %+v err=%v", tasks, err)
		}
		comments, err := s.MissingCommentTranslations(ctx, 10)
		if err != nil || len(comments) != 1 || comments[0].EntityID != strconv.FormatInt(comment.ID, 10) {
			t.Fatalf("missing comments: %+v err=%v", comments, err)
		}
		tr := Translation{Entity: "task_comment", EntityID: strconv.FormatInt(comment.ID, 10), Field: "body", Lang: "ko", SourceSHA256: SourceSHA256("comment"), Text: "댓글", Translator: "tester"}
		if err = s.PutTranslation(ctx, tr); err != nil {
			t.Fatal(err)
		}
		comments, err = s.MissingCommentTranslations(ctx, 10)
		if err != nil || len(comments) != 0 {
			t.Fatalf("translated comments: %+v err=%v", comments, err)
		}
	})
}

func init() {
	registerTaskAreaCase("documents/retention", func(t *testing.T, s *Store) {
		ctx := t.Context()
		now := time.Now().UTC()
		old := now.Add(-40 * 24 * time.Hour)
		fresh := now.Add(-5 * 24 * time.Hour)
		cutoff := now.Add(-30 * 24 * time.Hour)
		exec := func(pg, my string, args ...any) {
			t.Helper()
			if _, err := s.db().Exec(ctx, s.q(pg, my), args...); err != nil {
				t.Fatal(err)
			}
		}
		exec(`INSERT INTO changes(repo, pr_number, state, head_sha, base_sha, opened_at, merged_at, created_at, updated_at) VALUES($1,$2,'merged','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',$3,$4,$5,$6)`,
			`INSERT INTO changes(repo, pr_number, state, head_sha, base_sha, opened_at, merged_at, created_at, updated_at) VALUES(?,?,'merged','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',?,?,?,?)`,
			"example/app", 201, now, old, now, now)
		var changeOld int64
		if err := s.db().QueryRow(ctx, s.q(`SELECT id FROM changes WHERE repo=$1 AND pr_number=$2`, `SELECT id FROM changes WHERE repo=? AND pr_number=?`), "example/app", 201).Scan(&changeOld); err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO changes(repo, pr_number, state, head_sha, base_sha, opened_at, merged_at, created_at, updated_at) VALUES($1,$2,'merged','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',$3,$4,$5,$6)`,
			`INSERT INTO changes(repo, pr_number, state, head_sha, base_sha, opened_at, merged_at, created_at, updated_at) VALUES(?,?,'merged','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',?,?,?,?)`,
			"example/app", 202, now, fresh, now, now)
		exec(`INSERT INTO reviews(change_id, reviewed_head, round, pass, lens, reviewer_job_id, recorded_at) VALUES($1,$2,1,1,'correctness',$3,$4)`,
			`INSERT INTO reviews(change_id, reviewed_head, round, pass, lens, reviewer_job_id, recorded_at) VALUES(?,?,1,1,'correctness',?,?)`,
			changeOld, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "mwr-201-aaa-p1-1", now)
		exec(`INSERT INTO jobs(job_id, task_id, host, state, created_by, created_at, updated_at, ended_at) VALUES($1,0,'test-host','completed','tester',$2,$3,$4)`,
			`INSERT INTO jobs(job_id, task_id, host, state, created_by, created_at, updated_at, ended_at) VALUES(?,0,'test-host','completed','tester',?,?,?)`,
			"job-ret-old", now, now, old)
		exec(`INSERT INTO jobs(job_id, task_id, host, state, created_by, created_at, updated_at, ended_at) VALUES($1,0,'test-host','completed','tester',$2,$3,$4)`,
			`INSERT INTO jobs(job_id, task_id, host, state, created_by, created_at, updated_at, ended_at) VALUES(?,0,'test-host','completed','tester',?,?,?)`,
			"job-ret-fresh", now, now, fresh)
		put := func(key, kind, job string) {
			t.Helper()
			if _, _, err := s.PutDocument(ctx, Document{Key: key, Kind: kind, Body: "body of " + key, CreatedBy: "tester", Job: job}); err != nil {
				t.Fatal(err)
			}
		}
		put("review/mwr-201-aaa-p1-1", "brief", "mwr-201-aaa-p1-1")
		put("review/mwr-202-bbb-p1-2", "brief", "mwr-202-bbb-p1-2")
		put("screens/job-ret-old/pane.txt", "evidence", "job-ret-old")
		put("screens/job-ret-fresh/pane.txt", "evidence", "job-ret-fresh")
		// One task cites the two expiring documents and one kept document.
		citer := docRefsTask(t, s, "cites retention candidates")
		for _, key := range []string{"review/mwr-201-aaa-p1-1", "screens/job-ret-old/pane.txt", "review/mwr-202-bbb-p1-2"} {
			if _, err := s.AddTaskDocRef(ctx, citer.ID, key, "tester"); err != nil {
				t.Fatal(err)
			}
		}
		dry, err := s.PruneRetention(ctx, cutoff, 10, false, "test")
		if err != nil {
			t.Fatal(err)
		}
		if dry.Deleted != 0 || dry.Matched != 2 || dry.SkippedBound != 0 || dry.KeptActive != 2 || dry.Bytes == 0 {
			t.Fatalf("dry run: %+v, want matched 2 kept 2 bytes > 0", dry)
		}
		res, err := s.PruneRetention(ctx, cutoff, 10, true, "test")
		if err != nil || res.Deleted != 2 || res.ReviewDeleted != 1 || res.ScreensDeleted != 1 {
			t.Fatalf("apply: %+v err=%v", res, err)
		}
		if _, found, err := s.GetDocument(ctx, "review/mwr-201-aaa-p1-1"); err != nil || found {
			t.Fatalf("review old still present: found=%v err=%v", found, err)
		}
		if _, found, err := s.GetDocument(ctx, "screens/job-ret-old/pane.txt"); err != nil || found {
			t.Fatalf("screens old still present: found=%v err=%v", found, err)
		}
		if _, found, err := s.GetDocument(ctx, "review/mwr-202-bbb-p1-2"); err != nil || !found {
			t.Fatalf("review fresh missing: found=%v err=%v", found, err)
		}
		if docRefCount(t, s, "review/mwr-201-aaa-p1-1") != 0 || docRefCount(t, s, "screens/job-ret-old/pane.txt") != 0 {
			t.Fatal("retention left task references to the deleted documents")
		}
		if docRefCount(t, s, "review/mwr-202-bbb-p1-2") != 1 {
			t.Fatal("retention dropped the reference to a kept document")
		}
		var logs int64
		if err := s.db().QueryRow(ctx, s.q(`SELECT count(*) FROM doc_prune_log WHERE pruned_by=$1`, `SELECT COUNT(*) FROM doc_prune_log WHERE pruned_by=?`), "test").Scan(&logs); err != nil {
			t.Fatal(err)
		}
		if logs != 2 {
			t.Fatalf("audit log rows = %d, want 2", logs)
		}
		again, err := s.PruneRetention(ctx, cutoff, 10, true, "test")
		if err != nil || again.Deleted != 0 {
			t.Fatalf("second run: %+v err=%v", again, err)
		}
		// A key re-created after its expiry must not list the old citation.
		put("review/mwr-201-aaa-p1-1", "brief", "mwr-201-aaa-p1-1")
		refs, err := s.ListTaskDocRefs(ctx, citer.ID)
		if err != nil || len(refs) != 1 || refs[0].Key != "review/mwr-202-bbb-p1-2" {
			t.Fatalf("refs after re-create = %+v err=%v, want only the kept document", refs, err)
		}
	})
}
