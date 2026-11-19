//go:build mysql

package store

import (
	"strconv"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// TestMySQLYouTrackDocHitsReadsBindings maps a YouTrack article hit to its
// document on MySQL through yt_article_bindings. Before hk 5621 the call
// refused with ErrDialectUnsupported, so every document hit of the YouTrack
// search was lost once hk served from MySQL.
func TestMySQLYouTrackDocHitsReadsBindings(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	n := strconv.FormatInt(time.Now().UnixNano(), 10)
	doc, _, err := s.PutDocument(ctx, Document{Key: "hk5621/doc-" + n, Kind: "note", Session: "s-" + n, Body: "body", CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	article := "3-" + n
	if _, err := s.db().Exec(ctx, `INSERT INTO yt_article_bindings(document_id,tenant,article_id,project_id,origin,bound_by,bound_at) VALUES(?,'tenant',?,'PROJ','import_verified','test',?)`, doc.ID, article, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	hits, err := s.YouTrackDocHits(ctx, []string{article, "3-none-" + n}, "")
	if err != nil {
		t.Fatalf("YouTrackDocHits on MySQL: %v", err)
	}
	got, ok := hits[article]
	if !ok || got.Key != doc.Key || got.Scope != "docs" || len(hits) != 1 {
		t.Fatalf("hits = %+v, want only %s -> %s", hits, article, doc.Key)
	}
	if other, err := s.YouTrackDocHits(ctx, []string{article}, "other-session"); err != nil || len(other) != 0 {
		t.Fatalf("session filter: hits = %+v, err = %v, want none", other, err)
	}
}
