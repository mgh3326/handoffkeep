package store

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// The YouTrack search hits are SQL, so these run against PostgreSQL in CI. The
// YouTrack search backend's own behaviour is pinned by fake-YouTrack tests in
// internal/youtrack/search_test.go.

func TestYouTrackTaskHitsMapIssuesAndFilterByLane(t *testing.T) {
	s := schemaStore(t, "youtrack_search_tasks")
	ctx := context.Background()
	now := time.Now().UTC()
	laneA := "search-lane-a"
	laneB := "search-lane-b"
	ta := youtrackTask(t, s, laneA, "alpha search task")
	tb := youtrackTask(t, s, laneB, "beta search task")
	if _, err := s.UpsertYouTrackMirror(ctx, youtrackMirrorInput(ta.ID, "9-1", "HK-1", "tenant", "mirror", now)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertYouTrackMirror(ctx, youtrackMirrorInput(tb.ID, "9-2", "HK-2", "tenant", "mirror", now)); err != nil {
		t.Fatal(err)
	}

	hits, err := s.YouTrackTaskHits(ctx, "tenant", []string{"9-1", "9-2", "9-404"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %+v, want the two mirrored issues (9-404 unmapped)", hits)
	}
	if x := hits["9-1"]; x.Scope != "tasks" || x.Key != strconv.FormatInt(ta.ID, 10) || x.Session != laneA || x.Kind != "implement" {
		t.Fatalf("9-1 hit = %+v", x)
	}

	// A second tenant's mapping of the same issue id is a different row, and it
	// answers only for that tenant.
	tc := youtrackTask(t, s, laneA, "tenant-b task")
	if _, err := s.UpsertYouTrackMirror(ctx, youtrackMirrorInput(tc.ID, "9-1", "HK-9", "tenant-b", "mirror", now)); err != nil {
		t.Fatal(err)
	}
	mine, err := s.YouTrackTaskHits(ctx, "tenant", []string{"9-1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if mine["9-1"].Key != strconv.FormatInt(ta.ID, 10) {
		t.Fatalf("tenant hit = %+v, want task %d", mine["9-1"], ta.ID)
	}
	theirs, err := s.YouTrackTaskHits(ctx, "tenant-b", []string{"9-1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if theirs["9-1"].Key != strconv.FormatInt(tc.ID, 10) {
		t.Fatalf("tenant-b hit = %+v, want task %d", theirs["9-1"], tc.ID)
	}
	onlyB, err := s.YouTrackTaskHits(ctx, "tenant", []string{"9-1", "9-2"}, laneB)
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyB) != 1 || onlyB["9-2"].Key != strconv.FormatInt(tb.ID, 10) {
		t.Fatalf("lane filter hits = %+v, want only 9-2", onlyB)
	}
}

func TestYouTrackTaskHitsUseBindingWithoutOrigin(t *testing.T) {
	s := schemaStore(t, "youtrack_search_binding")
	ctx := context.Background()
	task := youtrackTask(t, s, "binding-search-lane", "binding only task")
	issueID := "9-binding-" + strconv.FormatInt(task.ID, 10)
	ytFeedExec(t, s, `INSERT INTO yt_task_bindings(task_id,tenant,issue_id,project_id,bound_by,bound_at) VALUES($1,$2,$3,'0-99','test',now())`, task.ID, "tenant", issueID)
	if n := ytcCount(t, s, `SELECT count(*) FROM task_origins WHERE task_id=$1`, task.ID); n != 0 {
		t.Fatalf("task_origins rows = %d, want none", n)
	}

	hits, err := s.YouTrackTaskHits(ctx, "tenant", []string{issueID}, "binding-search-lane")
	if err != nil {
		t.Fatal(err)
	}
	if hit, ok := hits[issueID]; !ok || hit.Key != strconv.FormatInt(task.ID, 10) || hit.Session != task.Lane {
		t.Fatalf("binding hit = %+v, present=%v; want task %d", hit, ok, task.ID)
	}
	otherTenant, err := s.YouTrackTaskHits(ctx, "other-tenant", []string{issueID}, "")
	if err != nil || len(otherTenant) != 0 {
		t.Fatalf("other tenant hits=%+v err=%v; want none", otherTenant, err)
	}
}

// A legacy task_origins mirror is suppressed only by a binding of the searched
// tenant: another tenant's binding on the same task must not hide this tenant's
// origin hit.
func TestYouTrackTaskHitsOriginIgnoresOtherTenantBinding(t *testing.T) {
	s := schemaStore(t, "youtrack_search_origin_tenant")
	ctx := context.Background()
	task := youtrackTask(t, s, "origin-tenant-lane", "origin tenant task")
	issueID := "9-origin-" + strconv.FormatInt(task.ID, 10)
	ytFeedExec(t, s, `INSERT INTO yt_task_bindings(task_id,tenant,issue_id,project_id,bound_by,bound_at) VALUES($1,$2,$3,'0-99','test',now())`, task.ID, "other-tenant", "9-other-"+strconv.FormatInt(task.ID, 10))
	ytFeedExec(t, s, `INSERT INTO task_origins(task_id,provider,namespace,external_id,snapshot_at,created_by,created_at) VALUES($1,'youtrack',$2,$3,now(),'test',now())`, task.ID, "tenant", issueID)

	hits, err := s.YouTrackTaskHits(ctx, "tenant", []string{issueID}, "origin-tenant-lane")
	if err != nil {
		t.Fatal(err)
	}
	if hit, ok := hits[issueID]; !ok || hit.Key != strconv.FormatInt(task.ID, 10) {
		t.Fatalf("origin hit = %+v, present=%v; want task %d (other tenant's binding must not suppress it)", hit, ok, task.ID)
	}
}

func TestYouTrackDocHitsMapArticlesAndFilterBySession(t *testing.T) {
	s := schemaStore(t, "youtrack_search_docs")
	ctx := context.Background()
	if _, _, err := s.PutDocument(ctx, Document{Key: "design/hits-a", Kind: "design", Session: "sess-hits", Body: "# A\n\nbody", CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	seedYouTrackDocArticle(t, s, YouTrackDocArticle{DocKey: "design/hits-a", HKProject: "handoffkeep", ArticleID: "55-1"})
	seedYouTrackDocArticle(t, s, YouTrackDocArticle{DocKey: "/hk-part/search-test/2", DocumentKey: "design/hits-a", PartIndex: 2, PartCount: 2, HKProject: "handoffkeep", ArticleID: "55-3"})
	// An index article has no document row, so it must not be a hit.
	seedYouTrackDocArticle(t, s, YouTrackDocArticle{DocKey: "index:hk:handoffkeep", HKProject: "handoffkeep", ArticleID: "55-2"})

	hits, err := s.YouTrackDocHits(ctx, []string{"55-1", "55-2", "55-3"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %+v, want primary and child document articles (index article dropped)", hits)
	}
	if x := hits["55-1"]; x.Scope != "docs" || x.Key != "design/hits-a" || x.Title != "design/hits-a" || x.Session != "sess-hits" || x.Kind != "design" {
		t.Fatalf("55-1 hit = %+v", x)
	}
	if x := hits["55-3"]; x.Key != "design/hits-a" {
		t.Fatalf("child part hit = %+v", x)
	}

	other, err := s.YouTrackDocHits(ctx, []string{"55-1"}, "other-session")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("session filter hits = %+v, want none", other)
	}

	exists, err := s.DocumentKeyExists(ctx, "design/hits-a")
	if err != nil || !exists {
		t.Fatalf("DocumentKeyExists(design/hits-a) = %v, %v; want true", exists, err)
	}
	missing, err := s.DocumentKeyExists(ctx, "design/none")
	if err != nil || missing {
		t.Fatalf("DocumentKeyExists(design/none) = %v, %v; want false", missing, err)
	}
}

func TestNumericIDQueryMatchesOnlyBareIDs(t *testing.T) {
	for q, want := range map[string]bool{"42": true, "#42": true, " #7 ": true, "4a": false, "#": false, "": false, "design/x": false} {
		if got := NumericIDQuery(q); got != want {
			t.Fatalf("NumericIDQuery(%q) = %v, want %v", q, got, want)
		}
	}
}

// CheckSearch moved out of search() unchanged. This pins what it refuses, so a
// later edit to either backend cannot quietly widen the accepted inputs.
func TestCheckSearchRefusesWhatSearchRefused(t *testing.T) {
	long := make([]byte, 513)
	for i := range long {
		long[i] = 'a'
	}
	longSession := make([]byte, 129)
	for i := range longSession {
		longSession[i] = 'a'
	}
	refused := []struct{ q, scope, session string }{
		{"", "all", ""},
		{"   ", "all", ""},
		{string(long), "all", ""},
		{"x", "all", string(longSession)},
		{"x", "bogus", ""},
		{"x", "", ""},
		{"x\x00y", "all", ""},
	}
	for _, c := range refused {
		if err := CheckSearch(c.q, c.scope, c.session); err == nil || err.Error() != "invalid search" {
			t.Fatalf("CheckSearch(%q, %q, %d-byte session) = %v, want invalid search", c.q, c.scope, len(c.session), err)
		}
	}
	for _, scope := range []string{"all", "docs", "tasks"} {
		if err := CheckSearch("x", scope, ""); err != nil {
			t.Fatalf("CheckSearch scope %s = %v, want accepted", scope, err)
		}
	}
}

// seedYouTrackDocArticle writes one article ledger row. The store has no
// writer for the ledger any more (the sync that filled it is gone), so the
// search tests seed it directly.
func seedYouTrackDocArticle(t *testing.T, s *Store, x YouTrackDocArticle) {
	t.Helper()
	if x.DocumentKey == "" {
		x.DocumentKey = x.DocKey
	}
	if x.PartIndex == 0 {
		x.PartIndex = 1
	}
	if x.PartCount == 0 {
		x.PartCount = 1
	}
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO youtrack_doc_articles(doc_key,document_key,part_index,part_count,part_sha,hk_project,article_id,source_sha,bytes,synced_at)
		VALUES($1,$2,$3,$4,'',$5,$6,'',0,now())`, x.DocKey, x.DocumentKey, x.PartIndex, x.PartCount, x.HKProject, x.ArticleID); err != nil {
		t.Fatal(err)
	}
}
