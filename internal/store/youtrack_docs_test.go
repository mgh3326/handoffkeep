package store

import (
	"context"
	"strings"
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
	if err := s.UpsertYouTrackDocArticle(ctx, YouTrackDocArticle{DocKey: design.Key, HKProject: "handoffkeep", ArticleID: "A-1", SourceSHA: design.SHA256, PartSHA: "v2:design", Bytes: int64(len(design.Body) + 100), SyncedAt: time.Now().UTC()}); err != nil {
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
	if err := s.UpsertYouTrackDocArticle(ctx, YouTrackDocArticle{DocKey: "/hk-part/gone/2", DocumentKey: "review/gone", PartIndex: 2, PartCount: 2, HKProject: "other", ArticleID: "A-10", SourceSHA: "y", PartSHA: "v2:child", Bytes: 3, SyncedAt: at}); err != nil {
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
	if len(orphans) != 2 || orphans[0].DocKey != "review/gone" || orphans[1].DocKey != "review/gone" {
		t.Fatalf("orphans = %+v, want both article parts for review/gone", orphans)
	}
	if err := s.UpsertYouTrackDocArticle(ctx, YouTrackDocArticle{DocKey: "review/x", ArticleID: "A-2"}); err == nil {
		t.Fatal("a row without a project must be refused")
	}
}

func TestLegacyOversizedStubIsDueForBackfill(t *testing.T) {
	s := schemaStore(t, "youtrack_docs_legacy_stub")
	ctx := context.Background()
	doc, _, err := s.PutDocument(ctx, Document{Key: "research/legacy-stub", Kind: "research", Body: "# Legacy\n\n" + strings.Repeat("x", 300<<10), CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutYouTrackProject(ctx, YouTrackProject{HKProject: "handoffkeep", ShortName: "HK"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertYouTrackDocArticle(ctx, YouTrackDocArticle{DocKey: doc.Key, HKProject: "handoffkeep", ArticleID: "A-stub", SourceSHA: doc.SHA256, Bytes: 120, SyncedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	due, err := s.DueYouTrackDocs(ctx, []string{"%"}, "handoffkeep", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Key != doc.Key {
		t.Fatalf("legacy stub was not queued for backfill: %+v", due)
	}
}

func TestLegacyRowWithoutPartSHAAndStaleChildAreDue(t *testing.T) {
	s := schemaStore(t, "youtrack_docs_legacy_parts")
	ctx := context.Background()
	doc, _, err := s.PutDocument(ctx, Document{Key: "research/legacy-parts", Kind: "research", Body: "# Legacy\n\nbody", CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.PutYouTrackProject(ctx, YouTrackProject{HKProject: "handoffkeep", ShortName: "HK"}); err != nil {
		t.Fatal(err)
	}
	if err = s.UpsertYouTrackDocArticle(ctx, YouTrackDocArticle{DocKey: doc.Key, HKProject: "handoffkeep", ArticleID: "A-1", SourceSHA: doc.SHA256, Bytes: int64(len(doc.Body) + 100), SyncedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	due, err := s.DueYouTrackDocs(ctx, []string{"%"}, "handoffkeep", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("row without part SHA was not due: %+v", due)
	}
	if err = s.UpsertYouTrackDocArticle(ctx, YouTrackDocArticle{DocKey: doc.Key, HKProject: "handoffkeep", ArticleID: "A-1", PartCount: 2, SourceSHA: doc.SHA256, PartSHA: "v2:primary", Bytes: int64(len(doc.Body) + 100), SyncedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err = s.UpsertYouTrackDocArticle(ctx, YouTrackDocArticle{DocKey: "/hk-part/child/2", DocumentKey: doc.Key, PartIndex: 2, PartCount: 2, HKProject: "handoffkeep", ArticleID: "A-2", SourceSHA: "old-source", PartSHA: "v2:child", Bytes: 10, SyncedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	due, err = s.DueYouTrackDocs(ctx, []string{"%"}, "handoffkeep", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("stale child was not due: %+v", due)
	}
}

func TestPreviousReleaseLedgerWriteCanStillBeReadAndIsNotOrphaned(t *testing.T) {
	s := schemaStore(t, "youtrack_docs_legacy_write")
	ctx := context.Background()
	doc, _, err := s.PutDocument(ctx, Document{Key: "review/legacy-write", Kind: "other", Body: "# Legacy write\n\nbody", CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO youtrack_doc_articles(doc_key,hk_project,article_id,source_sha,bytes,synced_at) VALUES($1,'handoffkeep','A-old',$2,200,now())`, doc.Key, doc.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.YouTrackDocArticles(ctx, doc.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].DocumentKey != "" {
		t.Fatalf("legacy row read = %+v", rows)
	}
	orphans, err := s.OrphanYouTrackDocArticles(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Fatalf("live document reported orphaned: %+v", orphans)
	}
}

func TestPreviousReleaseUpdateInvalidatesPartSHA(t *testing.T) {
	s := schemaStore(t, "youtrack_docs_legacy_update")
	ctx := context.Background()
	doc, _, err := s.PutDocument(ctx, Document{Key: "design/legacy-update", Kind: "design", Body: "# Before\n\nbody", CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.PutYouTrackProject(ctx, YouTrackProject{HKProject: "handoffkeep", ShortName: "HK"}); err != nil {
		t.Fatal(err)
	}
	if err = s.UpsertYouTrackDocArticle(ctx, YouTrackDocArticle{DocKey: doc.Key, HKProject: "handoffkeep", ArticleID: "A-old", SourceSHA: doc.SHA256, PartSHA: "v2:previous", Bytes: 200, SyncedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	updated, _, err := s.PutDocument(ctx, Document{Key: doc.Key, Kind: "design", Body: "# After\n\nchanged", CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO youtrack_doc_articles(doc_key,hk_project,article_id,source_sha,bytes,synced_at) VALUES($1,'handoffkeep','A-old',$2,200,now()) ON CONFLICT(doc_key) DO UPDATE SET hk_project=EXCLUDED.hk_project,article_id=EXCLUDED.article_id,source_sha=EXCLUDED.source_sha,bytes=EXCLUDED.bytes,synced_at=EXCLUDED.synced_at`, doc.Key, updated.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	row, ok, err := s.GetYouTrackDocArticle(ctx, doc.Key)
	if err != nil || !ok {
		t.Fatalf("get old-release row: ok=%v err=%v", ok, err)
	}
	if row.PartSHA != "" {
		t.Fatalf("legacy write retained stale part SHA %q", row.PartSHA)
	}
	due, err := s.DueYouTrackDocs(ctx, []string{"%"}, "handoffkeep", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Key != doc.Key {
		t.Fatalf("legacy-updated row was not due: %+v", due)
	}
}

func TestPreviousReleaseProjectMoveInvalidatesMultipartSHA(t *testing.T) {
	s := schemaStore(t, "youtrack_docs_legacy_move")
	ctx := context.Background()
	oldProject, newProject := "legacy-old", "legacy-new"
	for _, project := range []string{oldProject, newProject} {
		if _, err := s.AddTaskProject(ctx, project, "test"); err != nil {
			t.Fatal(err)
		}
		if err := s.PutYouTrackProject(ctx, YouTrackProject{HKProject: project, ShortName: project}); err != nil {
			t.Fatal(err)
		}
	}
	task, err := s.CreateTask(ctx, Task{Lane: relaneLane(t, "youtrack-doc-move"), Title: "move document", Kind: "implement", CreatedBy: "test", Project: &oldProject})
	if err != nil {
		t.Fatal(err)
	}
	doc, _, err := s.PutDocument(ctx, Document{Key: "design/legacy-move", Kind: "design", Body: "# Before\n\nbody", CreatedBy: "test", TaskID: &task.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertYouTrackDocArticle(ctx, YouTrackDocArticle{DocKey: doc.Key, HKProject: oldProject, ArticleID: "A-old", SourceSHA: doc.SHA256, PartIndex: 1, PartCount: 2, PartSHA: "v2:primary", Bytes: 100, SyncedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertYouTrackDocArticle(ctx, YouTrackDocArticle{DocKey: "/hk-part/legacy-move/2", DocumentKey: doc.Key, HKProject: oldProject, ArticleID: "A-child", SourceSHA: doc.SHA256, PartIndex: 2, PartCount: 2, PartSHA: "v2:child", Bytes: 20, SyncedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET project=$2 WHERE id=$1`, task.ID, newProject); err != nil {
		t.Fatal(err)
	}
	// This is the previous release's upsert: it replaces the primary article and
	// project, but cannot update the part SHA or child row.
	if _, err := s.pool.Exec(ctx, `UPDATE youtrack_doc_articles SET hk_project=$2,article_id='A-new',synced_at=now() WHERE doc_key=$1`, doc.Key, newProject); err != nil {
		t.Fatal(err)
	}
	row, ok, err := s.GetYouTrackDocArticle(ctx, doc.Key)
	if err != nil || !ok {
		t.Fatalf("get moved primary row: ok=%v err=%v", ok, err)
	}
	if row.PartSHA != "" {
		t.Fatalf("legacy project move retained stale part SHA %q", row.PartSHA)
	}
	due, err := s.DueYouTrackDocs(ctx, []string{"design/%"}, oldProject, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Key != doc.Key {
		t.Fatalf("legacy project move was not due for multipart repair: %+v", keysOf(due))
	}
}

func keysOf(docs []Document) []string {
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.Key)
	}
	return out
}
