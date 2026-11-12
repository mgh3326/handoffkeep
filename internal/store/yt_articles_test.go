package store

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

// Document articles (hk 4206): the document_articles feed consumer prepares
// a bound document's article_update when its body is not the article's, an
// article_create for a document created after the import, and nothing for
// an excluded, oversize or pre-import unbound document.

const (
	ytartShared     = "0-40"
	ytartUnassigned = "0-41"
)

func ytartBuckets() map[string]YTArticleProject {
	return map[string]YTArticleProject{
		YTArticleBucketShared:     {ID: ytartShared, ShortName: "SHRDOC"},
		YTArticleBucketUnassigned: {ID: ytartUnassigned, ShortName: "HK"},
	}
}

func ytartPass(t *testing.T, s *Store) YTFeedPass {
	t.Helper()
	return ytFeedPassOK(t, s, ytFeedConsumers[ytDocsConsumer])
}

// ytartSetup is a throwaway store in docs mode on (on false: off) with the
// consumer's cursor already created.
func ytartSetup(t *testing.T, prefix string, on bool) *Store {
	t.Helper()
	s := schemaStore(t, prefix)
	if on {
		s.SetYTDocsMode(YTDocsOn, ytcTenant, ytartBuckets())
	} else {
		s.SetYTDocsMode(YTDocsOff, "", nil)
	}
	if pass := ytartPass(t, s); !pass.Created {
		t.Fatalf("first pass %+v", pass)
	}
	return s
}

// ytartPut writes a document (a new one is created now, after the import
// ended) and returns its id.
func ytartPut(t *testing.T, s *Store, key, body string, taskID *int64) int64 {
	t.Helper()
	d, _, err := s.PutDocument(t.Context(), Document{Key: key, Kind: "note", Body: body, CreatedBy: "test", TaskID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	return d.ID
}

// ytartImported moves a document's creation to before the import ended (the
// update is itself a feed row).
func ytartImported(t *testing.T, s *Store, id int64) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), `UPDATE documents SET created_at=$2 WHERE id=$1`, id, YTImportEndedAt.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
}

// ytartTouch writes the document row without changing its body.
func ytartTouch(t *testing.T, s *Store, id int64) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), `UPDATE documents SET updated_at=updated_at + interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
}

// ytartBind publishes an import_verified binding of the document to article
// in the shared bucket's project, holding contentSHA ("" : the document's
// own sha).
func ytartBind(t *testing.T, s *Store, id int64, article, contentSHA string, needsUpdate bool) {
	t.Helper()
	d := ytArticleDoc(t, s, id)
	if contentSHA == "" {
		contentSHA = d.SHA256
	}
	b := YTArticleBinding{DocumentID: id, Tenant: ytcTenant, ArticleID: article, ProjectID: ytartShared, ContentSHA: contentSHA, NeedsUpdate: needsUpdate, BoundBy: "test"}
	if _, err := s.AddYTArticleBinding(t.Context(), b, d.YTArticleCandidate); err != nil {
		t.Fatal(err)
	}
}

func ytartOps(t *testing.T, s *Store, id int64) []ytwrite.Op {
	t.Helper()
	ops, err := s.YTWriteLedger().list(t.Context(), `SELECT `+ytOpColumns+` FROM yt_write_ops WHERE entity_kind='document' AND entity_id=$1 ORDER BY id`,
		strconv.FormatInt(id, 10))
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

func ytartDrive(t *testing.T, s *Store, yt *ytwritetest.FakeYouTrack, offset *atomic.Int64) {
	t.Helper()
	if _, err := ytcProtocol(t, s, yt, offset).Drive(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
}

func ytartBinding(t *testing.T, s *Store, id int64) YTArticleBinding {
	t.Helper()
	b, found, err := s.YTArticleBinding(t.Context(), id)
	if err != nil || !found {
		t.Fatalf("binding of %d: %v %v", id, found, err)
	}
	return b
}

// A bound document whose body changes gets one article_update, which the
// protocol applies to the bound article; its binding then records the new
// sha, so a write that leaves the body as it was prepares nothing. A
// document whose body is the article's prepares nothing either.
func TestYTArticlesBoundChangedUpdatesOnce(t *testing.T) {
	s := ytartSetup(t, "ytart_upd", true)
	yt := ytwritetest.NewFake()
	var offset atomic.Int64
	id := ytartPut(t, s, "note/ytart/upd", "imported body", nil)
	ytartImported(t, s, id)
	summary := YTArticleSummary("note/ytart/upd", "note")
	article := yt.InjectArticle(ytartShared, summary, "imported body")
	ytartBind(t, s, id, article, "", false)

	ytartPass(t, s)
	ytartTouch(t, s, id)
	ytartPass(t, s)
	if ops := ytartOps(t, s, id); len(ops) != 0 {
		t.Fatalf("unchanged document made ops %+v", ops)
	}

	ytartPut(t, s, "note/ytart/upd", "edited body", nil)
	ytartPass(t, s)
	ytartPass(t, s)
	ops := ytartOps(t, s, id)
	if len(ops) != 1 || ops[0].Kind != ytwrite.KindArticleUpdate || ops[0].YTTargetID != article || ops[0].Principal != YTDocsPrincipal ||
		ops[0].Tenant != ytcTenant || ops[0].State != ytwrite.StatePrepared {
		t.Fatalf("ops %+v", ops)
	}
	var p ytwrite.ArticleUpdatePayload
	if err := json.Unmarshal(ops[0].Payload, &p); err != nil || p.Summary != summary || p.Content != "edited body" || p.Provenance {
		t.Fatalf("payload %+v %v", p, err)
	}

	ytartDrive(t, s, yt, &offset)
	if got := ytartOps(t, s, id); got[0].State != ytwrite.StateApplied {
		t.Fatalf("op %+v", got[0])
	}
	if a, _ := yt.Article(article); a.Content != "edited body" || a.Summary != summary {
		t.Fatalf("article %+v", a)
	}
	b := ytartBinding(t, s, id)
	if b.ContentSHA != ytwrite.ContentSHA("edited body") || b.NeedsUpdate || b.Origin != YTBindingImportVerified {
		t.Fatalf("binding %+v", b)
	}
	ytartTouch(t, s, id)
	ytartPass(t, s)
	if n := len(ytartOps(t, s, id)); n != 1 {
		t.Fatalf("%d ops after a touch", n)
	}
}

// A needs_update binding (the backfill's sha_differs) is updated the same
// way on the document's next change, even when its body did not change.
func TestYTArticlesNeedsUpdateIsUpdated(t *testing.T) {
	s := ytartSetup(t, "ytart_need", true)
	id := ytartPut(t, s, "note/ytart/need", "hk body", nil)
	ytartImported(t, s, id)
	ytartBind(t, s, id, "3-500", ytwrite.ContentSHA("what the import wrote"), true)
	ytartTouch(t, s, id)
	ytartPass(t, s)
	ops := ytartOps(t, s, id)
	if len(ops) != 1 || ops[0].Kind != ytwrite.KindArticleUpdate || ops[0].YTTargetID != "3-500" {
		t.Fatalf("ops %+v", ops)
	}
}

// A document created after the import gets one article_create in its
// bucket's project; applying it publishes the binding (origin op_applied) in
// the apply's transaction and prepares no update for an unchanged body.
func TestYTArticlesPostImportCreateBinds(t *testing.T) {
	s := ytartSetup(t, "ytart_new", true)
	yt := ytwritetest.NewFake()
	var offset atomic.Int64
	id := ytartPut(t, s, "design/ytart/new.md", "# new\n\nbody", nil)
	ytartPass(t, s)
	ytartPass(t, s)
	ops := ytartOps(t, s, id)
	if len(ops) != 1 || ops[0].Kind != ytwrite.KindArticleCreate || ops[0].Key != ytwrite.ArticleKey(id) || ops[0].YTTargetID != ytartShared {
		t.Fatalf("ops %+v", ops)
	}
	var p ytwrite.ArticleCreatePayload
	if err := json.Unmarshal(ops[0].Payload, &p); err != nil || p.Summary != YTArticleSummary("design/ytart/new.md", "note") || p.Body != "# new\n\nbody" || p.YTProjectShort != "SHRDOC" {
		t.Fatalf("payload %+v %v", p, err)
	}
	if _, found, _ := s.YTArticleBinding(t.Context(), id); found {
		t.Fatal("bound before the apply")
	}

	ytartDrive(t, s, yt, &offset)
	op := ytartOps(t, s, id)[0]
	if op.State != ytwrite.StateApplied || op.Reason != "" {
		t.Fatalf("op %+v", op)
	}
	b := ytartBinding(t, s, id)
	if b.ArticleID != op.YTResultID || b.Origin != YTBindingOpApplied || b.OpID != op.ID || b.Tenant != ytcTenant || b.ProjectID != ytartShared ||
		b.ContentSHA != ytwrite.ContentSHA("# new\n\nbody") || b.NeedsUpdate {
		t.Fatalf("binding %+v", b)
	}
	if a, _ := yt.Article(op.YTResultID); !strings.HasSuffix(a.Content, "\n\n# new\n\nbody\n\n"+ytwrite.OpMarker(op.ID)) {
		t.Fatalf("article %+v", a)
	}
	if n := len(ytartOps(t, s, id)); n != 1 {
		t.Fatalf("%d ops after the bind", n)
	}
	// Its next edit is an update of the article it made, which keeps hk's
	// format: the update op's provenance line, the body and its marker. The
	// binding records the body's sha, so an unchanged body prepares nothing.
	ytartPut(t, s, "design/ytart/new.md", "# new\n\nedited", nil)
	ytartPass(t, s)
	ops = ytartOps(t, s, id)
	if len(ops) != 2 || ops[1].Kind != ytwrite.KindArticleUpdate || ops[1].YTTargetID != b.ArticleID {
		t.Fatalf("ops %+v", ops)
	}
	var up ytwrite.ArticleUpdatePayload
	if err := json.Unmarshal(ops[1].Payload, &up); err != nil || !up.Provenance || up.Content != "# new\n\nedited" {
		t.Fatalf("update payload %+v %v", up, err)
	}
	ytartDrive(t, s, yt, &offset)
	upd := ytartOps(t, s, id)[1]
	if upd.State != ytwrite.StateApplied {
		t.Fatalf("update %+v", upd)
	}
	if a, _ := yt.Article(b.ArticleID); a.Content != ytwrite.RenderArticleCreate(YTDocsPrincipal, upd.CreatedAt, upd.ID, "# new\n\nedited") {
		t.Fatalf("article after update %+v", a)
	}
	if b := ytartBinding(t, s, id); b.ContentSHA != ytwrite.ContentSHA("# new\n\nedited") || b.NeedsUpdate || b.Origin != YTBindingOpApplied {
		t.Fatalf("binding after update %+v", b)
	}
	ytartTouch(t, s, id)
	ytartPass(t, s)
	if n := len(ytartOps(t, s, id)); n != 2 {
		t.Fatalf("%d ops after a touch", n)
	}
}

// A create whose reply was lost is applied by its evidence (the article with
// its marker), not sent twice, and binds the document then.
func TestYTArticlesCreateAdoptedOnLostReply(t *testing.T) {
	s := ytartSetup(t, "ytart_lost", true)
	yt := ytwritetest.NewFake()
	var offset atomic.Int64
	id := ytartPut(t, s, "design/ytart/lost.md", "lost body", nil)
	ytartPass(t, s)
	yt.Queue(ytwritetest.Fault{Kind: ytwritetest.CommitDropReply})
	ytartDrive(t, s, yt, &offset)
	if op := ytartOps(t, s, id)[0]; op.State != ytwrite.StateRemoteUnknown {
		t.Fatalf("op %+v", op)
	}
	if _, found, _ := s.YTArticleBinding(t.Context(), id); found {
		t.Fatal("bound while unknown")
	}
	offset.Add(int64(time.Hour))
	ytartDrive(t, s, yt, &offset)
	op := ytartOps(t, s, id)[0]
	arts := yt.Articles(ytartShared)
	if op.State != ytwrite.StateApplied || len(arts) != 1 || op.YTResultID != arts[0].ID || yt.Calls() != 1 {
		t.Fatalf("op %+v articles %+v calls %d", op, arts, yt.Calls())
	}
	if b := ytartBinding(t, s, id); b.ArticleID != arts[0].ID || b.Origin != YTBindingOpApplied || b.OpID != op.ID {
		t.Fatalf("binding %+v", b)
	}
}

// No op for: an unbound document created before the import ended
// (exception_unbound_imported), a body over the single-article limit, bound
// or not (exception_not_updated), and the excluded prefixes. A task's brief
// is in scope (handoffkeep-design, hk 4206) and gets its create in its
// task's project's YouTrack project.
func TestYTArticlesExceptionsAndScope(t *testing.T) {
	s := ytartSetup(t, "ytart_scope", true)
	ctx := t.Context()
	// New tasks need a project; this one maps to its own YouTrack project.
	if _, err := s.AddTaskProject(ctx, "ytart", "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutYouTrackProject(ctx, YouTrackProject{HKProject: "ytart", ProjectID: "0-42", ShortName: "YTART"}); err != nil {
		t.Fatal(err)
	}
	project := "ytart"
	task, err := s.CreateTask(ctx, Task{Lane: "ytart-lane", Title: "articles", Kind: "implement", CreatedBy: "test", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("a", YTArticleSafePartBytes+1)
	imported := ytartPut(t, s, "note/ytart/imported", "old", nil)
	ytartImported(t, s, imported)
	oversize := ytartPut(t, s, "note/ytart/oversize", big, nil)
	oversizeBound := ytartPut(t, s, "note/ytart/oversize-bound", "small", nil)
	ytartImported(t, s, oversizeBound)
	ytartBind(t, s, oversizeBound, "3-600", "", false)
	ytartPut(t, s, "note/ytart/oversize-bound", big, nil)
	excluded := []int64{
		ytartPut(t, s, "screens/ytart/pane", "screen", nil),
		ytartPut(t, s, "review/mwr-ytart/prompt", "prompt", nil),
		ytartPut(t, s, "reports/ytart-job/report.md", "report", &task.ID),
		ytartPut(t, s, "reports/mwr-ytart/r1.md", "review report", &task.ID),
	}
	excluded = append(excluded, ytartPut(t, s, "briefs/mwr-ytart-r1.md", "reviewer brief", nil))
	brief := ytartPut(t, s, "brief/ytart-job/brief.md", "the brief", &task.ID)
	ytartPass(t, s)
	for _, id := range append([]int64{imported, oversize, oversizeBound}, excluded...) {
		if ops := ytartOps(t, s, id); len(ops) != 0 {
			t.Errorf("document %d made ops %+v", id, ops)
		}
	}
	if ops := ytartOps(t, s, brief); len(ops) != 1 || ops[0].Kind != ytwrite.KindArticleCreate || ops[0].YTTargetID != "0-42" {
		t.Fatalf("brief ops %+v", ops)
	}
	for id, want := range map[int64]string{imported: YTArticleExceptionUnboundImported, oversize: YTArticleExceptionNotUpdated,
		oversizeBound: YTArticleExceptionNotUpdated, brief: "", excluded[0]: "", excluded[2]: "", excluded[4]: ""} {
		if got := YTArticleExceptionOf(ytArticleDoc(t, s, id), ytcTenant); got != want {
			t.Errorf("document %d exception %q, want %q", id, got, want)
		}
	}
}

// Mode off prepares nothing, and its cursor still advances; on later, the
// document is written with its next change only.
func TestYTArticlesModeOffDoesNothing(t *testing.T) {
	s := ytartSetup(t, "ytart_off", false)
	id := ytartPut(t, s, "design/ytart/off.md", "body", nil)
	pass := ytartPass(t, s)
	if ops := ytartOps(t, s, id); len(ops) != 0 || pass.Read == 0 {
		t.Fatalf("pass %+v ops %+v", pass, ops)
	}
	if got := ytArticleCreateGateSQL(s); got != `FALSE` {
		t.Fatalf("create gate %s", got)
	}
	if got := ytArticleUpdateGateSQL(s); got != `FALSE` {
		t.Fatalf("update gate %s", got)
	}
	s.SetYTDocsMode(YTDocsOn, ytcTenant, ytartBuckets())
	ytartPass(t, s)
	if ops := ytartOps(t, s, id); len(ops) != 0 {
		t.Fatalf("ops without a change %+v", ops)
	}
	ytartTouch(t, s, id)
	ytartPass(t, s)
	if ops := ytartOps(t, s, id); len(ops) != 1 {
		t.Fatalf("ops after the change %+v", ops)
	}
}

// On without both bucket projects is off.
func TestYTDocsModeNeedsBuckets(t *testing.T) {
	s := &Store{}
	s.SetYTDocsMode(YTDocsOn, ytcTenant, map[string]YTArticleProject{YTArticleBucketShared: {ID: "0-1", ShortName: "S"}})
	if _, on := s.ytDocsNow(); on {
		t.Fatal("on with one bucket")
	}
	s.SetYTDocsMode(YTDocsOn, ytcTenant, ytartBuckets())
	if _, on := s.ytDocsNow(); !on {
		t.Fatal("off with both buckets")
	}
}

func TestYTArticleException(t *testing.T) {
	after, before := YTImportEndedAt.Add(time.Second), YTImportEndedAt.Add(-time.Second)
	for _, tc := range []struct {
		bytes             int
		bound, splitIndex bool
		at                time.Time
		want              string
	}{
		{10, false, false, after, ""},
		{10, false, false, YTImportEndedAt, ""},
		{10, false, false, before, YTArticleExceptionUnboundImported},
		{10, true, false, before, ""},
		{10, true, true, before, YTArticleExceptionNotUpdated},
		{YTArticleSafePartBytes, false, false, after, ""},
		{YTArticleSafePartBytes + 1, false, false, after, YTArticleExceptionNotUpdated},
		{YTArticleSafePartBytes + 1, true, false, before, YTArticleExceptionNotUpdated},
	} {
		if got := ytArticleException(tc.bytes, tc.bound, tc.splitIndex, tc.at); got != tc.want {
			t.Errorf("%+v: %q", tc, got)
		}
	}
	if YTImportEndedAt != time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC) || YTArticleSafePartBytes != 240*1024 {
		t.Fatal("constants moved")
	}
}

// r4545936506/r4545936516: an update queued for B applies after the document
// went back to A, which the feed found unchanged (the binding still held A).
// The apply re-checks the document and queues A's update behind it, so the
// article ends at A.
func TestYTArticlesQueuedUpdateThenRevert(t *testing.T) {
	s := ytartSetup(t, "ytart_revert", true)
	yt := ytwritetest.NewFake()
	var offset atomic.Int64
	id := ytartPut(t, s, "note/ytart/revert", "body A", nil)
	ytartImported(t, s, id)
	article := yt.InjectArticle(ytartShared, YTArticleSummary("note/ytart/revert", "note"), "body A")
	ytartBind(t, s, id, article, "", false)
	ytartPass(t, s)

	ytartPut(t, s, "note/ytart/revert", "body B", nil)
	ytartPass(t, s)
	ytartPut(t, s, "note/ytart/revert", "body A", nil)
	ytartPass(t, s)
	if ops := ytartOps(t, s, id); len(ops) != 1 {
		t.Fatalf("ops before the drive %+v", ops)
	}
	ytartDrive(t, s, yt, &offset)
	ops := ytartOps(t, s, id)
	if len(ops) != 2 || ops[0].State != ytwrite.StateApplied || ops[1].Kind != ytwrite.KindArticleUpdate {
		t.Fatalf("ops after B applied %+v", ops)
	}
	ytartDrive(t, s, yt, &offset)
	if a, _ := yt.Article(article); a.Content != "body A" {
		t.Fatalf("article %+v", a)
	}
	if b := ytartBinding(t, s, id); b.ContentSHA != ytwrite.ContentSHA("body A") {
		t.Fatalf("binding %+v", b)
	}
	if n := len(ytartOps(t, s, id)); n != 2 {
		t.Fatalf("%d ops at the end", n)
	}
}

// r4545936512: the consumer's cursor starts at the feed's end, so documents
// written before its first pass have no row it reads. The catch-up after the
// cursor exists prepares their ops once.
func TestYTArticlesCatchUpCoversTheFirstPassGap(t *testing.T) {
	s := schemaStore(t, "ytart_catch")
	s.SetYTDocsMode(YTDocsOn, ytcTenant, ytartBuckets())
	fresh := ytartPut(t, s, "design/ytart/fresh.md", "fresh", nil)
	bound := ytartPut(t, s, "note/ytart/bound", "old", nil)
	ytartImported(t, s, bound)
	ytartBind(t, s, bound, "3-700", "", false)
	ytartPut(t, s, "note/ytart/bound", "changed before the first pass", nil)
	same := ytartPut(t, s, "note/ytart/same", "same", nil)
	ytartImported(t, s, same)
	ytartBind(t, s, same, "3-701", "", false)
	if pass := ytartPass(t, s); !pass.Created {
		t.Fatalf("first pass %+v", pass)
	}
	if len(ytartOps(t, s, fresh))+len(ytartOps(t, s, bound)) != 0 {
		t.Fatal("the first pass read rows from before its cursor")
	}
	if _, err := s.RunYTFeedConsumers(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	if ops := ytartOps(t, s, fresh); len(ops) != 1 || ops[0].Kind != ytwrite.KindArticleCreate {
		t.Fatalf("fresh ops %+v", ops)
	}
	if ops := ytartOps(t, s, bound); len(ops) != 1 || ops[0].Kind != ytwrite.KindArticleUpdate || ops[0].YTTargetID != "3-700" {
		t.Fatalf("bound ops %+v", ops)
	}
	if ops := ytartOps(t, s, same); len(ops) != 0 {
		t.Fatalf("unchanged ops %+v", ops)
	}
	var marker bool
	if err := s.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM yt_feed_cursors WHERE consumer=$1)`, ytDocsCatchUpMarker).Scan(&marker); err != nil || !marker {
		t.Fatalf("catch-up marker %v %v", marker, err)
	}
	// Again, directly: idempotent.
	if _, _, err := s.catchUpYTArticles(t.Context(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if len(ytartOps(t, s, fresh)) != 1 || len(ytartOps(t, s, bound)) != 1 {
		t.Fatal("a second catch-up prepared ops again")
	}
}

// r4546214644: a backfill binding is refused while the document's
// article_create is unresolved; it would hold the create, and every update
// behind it on the lane, for good.
func TestYTArticleBindingRefusedWhileCreatePending(t *testing.T) {
	s := ytartSetup(t, "ytart_pend", true)
	id := ytartPut(t, s, "design/ytart/pending.md", "body", nil)
	ytartPass(t, s)
	if ops := ytartOps(t, s, id); len(ops) != 1 || ops[0].State != ytwrite.StatePrepared {
		t.Fatalf("ops %+v", ops)
	}
	d := ytArticleDoc(t, s, id)
	b := YTArticleBinding{DocumentID: id, Tenant: ytcTenant, ArticleID: "3-800", ProjectID: ytartShared, ContentSHA: d.SHA256, BoundBy: "test"}
	if _, err := s.AddYTArticleBinding(t.Context(), b, d.YTArticleCandidate); !errors.Is(err, ErrYTArticleBindingConflict) || !strings.Contains(err.Error(), "unresolved article_create") {
		t.Fatalf("binding under a pending create: %v", err)
	}
	if _, found, _ := s.YTArticleBinding(t.Context(), id); found {
		t.Fatal("bound")
	}
	if _, err := s.YTWriteLedger().Reject(t.Context(), time.Now().UTC(), ytartOps(t, s, id)[0].ID, "operator_not_applied", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddYTArticleBinding(t.Context(), b, d.YTArticleCandidate); err != nil {
		t.Fatalf("binding after the create was rejected: %v", err)
	}
}

// r4546390459: the documents authority switch also waits for the document
// articles: HANDOFFKEEP_YT_DOCS_MODE on, no unresolved hk:documents op, and
// document_articles caught up with the feed.
func TestYTAuthorityDocumentsWaitsForArticles(t *testing.T) {
	s := ytartSetup(t, "ytart_auth", false)
	ctx := t.Context()
	s.SetYTDocCommentsMode(YTDocCommentsOn)
	docComments := ytFeedConsumers[ytDocCommentsConsumer]
	ytFeedPassOK(t, s, docComments)
	set := func() error {
		_, _, err := s.SetYTAuthority(ctx, []YTAuthorityKind{YTAuthorityDocuments}, YTAuthorityYouTrack, "operator", "drill")
		return err
	}
	refused := func(err error, want string) {
		t.Helper()
		if !errors.Is(err, ErrYTAuthorityRefused) || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want a refusal naming %q", err, want)
		}
	}
	refused(set(), "HANDOFFKEEP_YT_DOCS_MODE")

	s.SetYTDocsMode(YTDocsOn, ytcTenant, ytartBuckets())
	id := ytartPut(t, s, "design/ytart/auth.md", "body", nil)
	ytFeedPassOK(t, s, docComments)
	refused(set(), "feed consumer document_articles has 1 changes")
	ytartPass(t, s)
	ops := ytartOps(t, s, id)
	if len(ops) != 1 || ops[0].Principal != YTDocsPrincipal {
		t.Fatalf("ops %+v", ops)
	}
	refused(set(), "documents: 1 ops")

	if _, err := s.YTWriteLedger().Reject(ctx, time.Now().UTC(), ops[0].ID, "operator_not_applied", "test"); err != nil {
		t.Fatal(err)
	}
	// r4546651995/r4546652097: a caught-up cursor proves nothing until the
	// first catch-up has covered the documents changed before it.
	refused(set(), "first catch-up has not completed")
	if _, err := s.RunYTFeedConsumers(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if err := set(); err != nil {
		t.Fatalf("switch with every precondition met: %v", err)
	}
}

// The document kinds' scope: the import's filter, less the mergewake
// reviewer briefs (handoffkeep-design 2026-11-12).
func TestYTArticleInScope(t *testing.T) {
	for _, tc := range []struct {
		key  string
		task int64
		want bool
	}{
		{"design/x.md", 0, true},
		{"deploy/2026-11-12.md", 0, true},
		{"brief/job/brief.md", 7, true},
		{"briefs/job.md", 0, true},
		{"briefs/mwr-338-r1.md", 0, false},
		{"briefs/mwr-338/round1.md", 7, false},
		{"briefs/mwr", 0, true},
		{"review/mwr-338/prompt.md", 0, false},
		{"screens/pane.txt", 0, false},
		{"reports/job/report.md", 7, false},
		{"reports/job/report.md", 0, true},
	} {
		if got := ytArticleInScope(tc.key, tc.task); got != tc.want {
			t.Errorf("ytArticleInScope(%q, %d) = %v, want %v", tc.key, tc.task, got, tc.want)
		}
	}
}

// ytartLegacy records an article of the legacy `youtrack sync` for a
// document key, split into parts articles.
func ytartLegacy(t *testing.T, s *Store, key, article string, parts int) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO youtrack_doc_articles(doc_key, hk_project, article_id, synced_at, document_key, part_index, part_count)
		VALUES ($1,'',$2,now(),$1,1,$3)`, key, article, parts); err != nil {
		t.Fatal(err)
	}
}

// r4546690175 (handoffkeep-design decision-3 B): a post-import document the
// legacy sync wrote never gets a second article. Its legacy article, once
// verified, is adopted and bound import_verified, and the body is rewritten
// raw by an article_update; one that does not verify, or a split one, is
// rejected legacy_synced_unverified with nothing created or bound.
func TestYTArticlesLegacySyncedDocument(t *testing.T) {
	s := ytartSetup(t, "ytart_legacy", true)
	yt := ytwritetest.NewFake()
	var offset atomic.Int64
	const key = "design/ytart/legacy.md"
	legacy := yt.InjectArticle(ytartShared, key, ytwrite.LegacyDocMarker(key)+"\n> hk 문서\n\nold body")
	ytartLegacy(t, s, key, legacy, 1)
	id := ytartPut(t, s, key, "current body", nil)
	ytartPass(t, s)
	ops := ytartOps(t, s, id)
	var p ytwrite.ArticleCreatePayload
	if len(ops) != 1 || json.Unmarshal(ops[0].Payload, &p) != nil || p.LegacyArticleID != legacy || p.LegacyMarker != ytwrite.LegacyDocMarker(key) {
		t.Fatalf("ops %+v payload %+v", ops, p)
	}
	// One drive applies the adoption and the update it queued (the
	// binding's needs_update is what queues it, and the update clears it).
	ytartDrive(t, s, yt, &offset)
	ytartDrive(t, s, yt, &offset)
	ops = ytartOps(t, s, id)
	if ops[0].State != ytwrite.StateApplied || ops[0].YTResultID != legacy || len(yt.Articles(ytartShared)) != 1 {
		t.Fatalf("ops %+v articles %+v", ops, yt.Articles(ytartShared))
	}
	var up ytwrite.ArticleUpdatePayload
	if len(ops) != 2 || ops[1].Kind != ytwrite.KindArticleUpdate || ops[1].State != ytwrite.StateApplied ||
		json.Unmarshal(ops[1].Payload, &up) != nil || up.Provenance || up.Content != "current body" {
		t.Fatalf("ops after the adoption %+v", ops)
	}
	if b := ytartBinding(t, s, id); b.ArticleID != legacy || b.Origin != YTBindingImportVerified || b.OpID != ops[0].ID ||
		b.NeedsUpdate || b.ContentSHA != ytwrite.ContentSHA("current body") {
		t.Fatalf("binding %+v", b)
	}
	if a, _ := yt.Article(legacy); a.Content != "current body" {
		t.Fatalf("article %+v", a)
	}

	for name, setup := range map[string]func() string{
		"unverified": func() string { return yt.InjectArticle("0-99", "x", "no marker") },
		"split":      func() string { return yt.InjectArticle(ytartShared, "y", "part one") },
	} {
		k := "design/ytart/legacy-" + name + ".md"
		art := setup()
		parts := 1
		if name == "split" {
			parts = 2
		}
		ytartLegacy(t, s, k, art, parts)
		doc := ytartPut(t, s, k, "body", nil)
		ytartPass(t, s)
		ytartDrive(t, s, yt, &offset)
		ops := ytartOps(t, s, doc)
		if len(ops) != 1 || ops[0].State != ytwrite.StateRejected || ops[0].Reason != ytwrite.ReasonLegacySyncedUnverified {
			t.Fatalf("%s: ops %+v", name, ops)
		}
		if _, found, _ := s.YTArticleBinding(t.Context(), doc); found {
			t.Fatalf("%s: bound", name)
		}
	}
	if n := len(yt.ArticleWrites()); n != 1 {
		t.Fatalf("%d article writes, want only the one update", n)
	}
}

// r4546690164: a document the catch-up skipped as no_project (its task's
// project has no YouTrack project yet) is retried on every later pass, and
// the catch-up is not done (no marker, the documents switch refused) until
// its mapping exists and its create is prepared.
func TestYTArticlesCatchUpRetriesNoProject(t *testing.T) {
	s := schemaStore(t, "ytart_noproj")
	ctx := t.Context()
	s.SetYTDocsMode(YTDocsOn, ytcTenant, ytartBuckets())
	if _, err := s.AddTaskProject(ctx, "ytart-np", "test"); err != nil {
		t.Fatal(err)
	}
	project := "ytart-np"
	task, err := s.CreateTask(ctx, Task{Lane: "ytart-lane", Title: "articles", Kind: "implement", CreatedBy: "test", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	id := ytartPut(t, s, "design/ytart/noproj.md", "body", &task.ID)
	if pass := ytartPass(t, s); !pass.Created {
		t.Fatalf("first pass %+v", pass)
	}
	marker := func() bool {
		t.Helper()
		var ok bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM yt_feed_cursors WHERE consumer=$1)`, ytDocsCatchUpMarker).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	for range 2 {
		if _, err := s.RunYTFeedConsumers(ctx, 100); err != nil {
			t.Fatal(err)
		}
		if ops := ytartOps(t, s, id); len(ops) != 0 || marker() || s.ytDocsCaughtUp.Load() {
			t.Fatalf("no_project: ops %+v marker %v", ops, marker())
		}
	}
	if err := s.PutYouTrackProject(ctx, YouTrackProject{HKProject: "ytart-np", ProjectID: "0-43", ShortName: "YTNP"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunYTFeedConsumers(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if ops := ytartOps(t, s, id); len(ops) != 1 || ops[0].Kind != ytwrite.KindArticleCreate || ops[0].YTTargetID != "0-43" {
		t.Fatalf("ops after the mapping %+v", ops)
	}
	if !marker() || !s.ytDocsCaughtUp.Load() {
		t.Fatal("catch-up not done once nothing waits")
	}
}
