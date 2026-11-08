package store

import (
	"context"
	"testing"
	"time"
)

func TestYouTrackDocsDueFiltersByPatternAndHash(t *testing.T) {
	s := schemaStore(t, "youtrack_docs_due")
	ctx := context.Background()
	for _, key := range []string{"design/docs-a", "decision/docs-b", "policy/docs-c"} {
		if _, _, err := s.PutDocument(ctx, Document{Key: key, Kind: "design", Body: "# " + key + "\n\nbody", CreatedBy: "test"}); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	if err := s.PutYouTrackProject(ctx, YouTrackProject{HKProject: "handoffkeep", ShortName: "HK"}); err != nil {
		t.Fatal(err)
	}
	patterns := []string{"design/%", "decision/%"}
	due, err := s.DueYouTrackDocs(ctx, patterns, "handoffkeep", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 2 || due[0].Key != "decision/docs-b" || due[1].Key != "design/docs-a" {
		t.Fatalf("due = %+v, want decision/docs-b then design/docs-a (policy excluded, key order)", keysOf(due))
	}
	if due[1].Body == "" {
		t.Fatal("due document lacks its body")
	}

	design := due[1]
	if err := s.UpsertYouTrackDocArticle(ctx, YouTrackDocArticle{DocKey: design.Key, HKProject: "handoffkeep", ArticleID: "A-1", SourceSHA: design.SHA256, Bytes: 10, SyncedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	due, err = s.DueYouTrackDocs(ctx, patterns, "handoffkeep", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Key != "decision/docs-b" {
		t.Fatalf("after writing design/docs-a, due = %+v", keysOf(due))
	}

	if _, _, err := s.PutDocument(ctx, Document{Key: design.Key, Kind: "design", Body: "# changed\n\nbody", CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	due, err = s.DueYouTrackDocs(ctx, patterns, "handoffkeep", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 2 {
		t.Fatalf("a changed sha must be due again, got %+v", keysOf(due))
	}
}

func TestYouTrackDocArticleLedgerRoundTripAndOrphans(t *testing.T) {
	s := schemaStore(t, "youtrack_docs_ledger")
	ctx := context.Background()
	at := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	if _, ok, err := s.GetYouTrackDocArticle(ctx, "design/none"); err != nil || ok {
		t.Fatalf("missing row: ok=%v err=%v", ok, err)
	}
	if err := s.UpsertYouTrackDocArticle(ctx, YouTrackDocArticle{DocKey: "index:design:other", HKProject: "other", ArticleID: "A-0", SyncedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertYouTrackDocArticle(ctx, YouTrackDocArticle{DocKey: "review/gone", HKProject: "other", ArticleID: "A-9", SourceSHA: "x", Bytes: 3, SyncedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertYouTrackDocArticle(ctx, YouTrackDocArticle{DocKey: "review/gone", HKProject: "other", ArticleID: "A-9", SourceSHA: "y", Bytes: 4, SyncedAt: at}); err != nil {
		t.Fatal(err)
	}
	row, ok, err := s.GetYouTrackDocArticle(ctx, "review/gone")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if row.SourceSHA != "y" || row.Bytes != 4 || row.ArticleID != "A-9" {
		t.Fatalf("upsert did not replace the row: %+v", row)
	}
	orphans, err := s.OrphanYouTrackDocArticles(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0].DocKey != "review/gone" {
		t.Fatalf("orphans = %+v, want only review/gone (index rows are not documents)", orphans)
	}
	if err := s.UpsertYouTrackDocArticle(ctx, YouTrackDocArticle{DocKey: "review/x", ArticleID: "A-2"}); err == nil {
		t.Fatal("a row without a project must be refused")
	}
}

func keysOf(docs []Document) []string {
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.Key)
	}
	return out
}
