package store

import (
	"strconv"
	"testing"
	"time"
)

// These three paths are the ones the first review round found broken on MySQL:
// the unquoted reserved word `key` in the exact-document searches and in the
// rebuild drill's document read, and the NULL sum in the outbox status. Each
// case fails on the reviewed head (a 1064 syntax error, or "converting NULL to
// int64") and passes once the MySQL texts are fixed (hk 5077).
func init() {
	registerYTAreaCase("search/exact-document-lookups", func(t *testing.T, s *Store) {
		ctx := t.Context()
		doc, changed, err := s.PutDocument(ctx, Document{Key: "ytcase/search/doc", Kind: "note", Body: "searchable", CreatedBy: "ytcase"})
		if err != nil || !changed {
			t.Fatalf("put document: changed=%v err=%v", changed, err)
		}
		hits, found, err := s.ExactDocHit(ctx, doc.Key, "")
		if err != nil || !found || len(hits) != 1 || hits[0].Key != doc.Key {
			t.Fatalf("exact doc hit: found=%v hits=%+v err=%v", found, hits, err)
		}
		ok, err := s.DocumentKeyExists(ctx, doc.Key)
		if err != nil || !ok {
			t.Fatalf("document key exists: ok=%v err=%v", ok, err)
		}
		numHits, numeric, err := s.ExactNumericHit(ctx, strconv.FormatInt(doc.ID, 10), "docs", "")
		if err != nil || !numeric || len(numHits) != 1 || numHits[0].Key != doc.Key {
			t.Fatalf("exact numeric doc hit: numeric=%v hits=%+v err=%v", numeric, numHits, err)
		}
	})

	registerYTAreaCase("rebuild/task-comments-and-docs", func(t *testing.T, s *Store) {
		ctx := t.Context()
		task, _, err := s.IntakeTask(ctx, IntakeInput{Source: "yt-rebuild-case", SourceRef: "one", Title: "rebuild task", CreatedBy: "ytcase"})
		if err != nil {
			t.Fatal(err)
		}
		comment, err := s.CreateTaskComment(ctx, task.ID, "ytcase", "a comment")
		if err != nil {
			t.Fatal(err)
		}
		id := task.ID
		if _, _, err := s.PutDocument(ctx, Document{Key: "ytcase/rebuild/doc", Kind: "brief", Body: "body", TaskID: &id, CreatedBy: "ytcase"}); err != nil {
			t.Fatal(err)
		}
		got, err := s.YTRebuildTaskComments(ctx, task.ID, "2-1")
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Comments) != 1 || got.Comments[0].ID != comment.ID {
			t.Fatalf("rebuild comments: %+v", got.Comments)
		}
		if len(got.Docs) != 1 || got.Docs[0].Key != "ytcase/rebuild/doc" {
			t.Fatalf("rebuild docs: %+v", got.Docs)
		}
	})

	registerYTAreaCase("outbox/empty-status", func(t *testing.T, s *Store) {
		out, err := s.YTWriteOutboxStatus(t.Context(), time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if out.Retrying != 0 || out.Reconciling != 0 || len(out.Open) != 0 {
			t.Fatalf("empty outbox status: %+v", out)
		}
	})
}
