package store

import (
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
		if s.dialect == mysqlDialect {
			t.Skip("prune requires the YouTrack binding and attachment areas")
		}
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
		cutoff := time.Now().UTC().Add(-24 * time.Hour)
		res, err := s.PruneDocuments(ctx, DocPrunePrefix, cutoff, 10, false)
		if err != nil || res.Matched != 1 || res.Deleted != 0 || res.Bytes != int64(len("prunable")) {
			t.Fatalf("dry run: %+v err=%v", res, err)
		}
		res, err = s.PruneDocuments(ctx, DocPrunePrefix, cutoff, 10, true)
		if err != nil || res.Deleted != 1 {
			t.Fatalf("apply: %+v err=%v", res, err)
		}
		if _, found, err := s.GetDocument(ctx, doc.Key); err != nil || found {
			t.Fatalf("get after prune: found=%v err=%v", found, err)
		}
	})
}
