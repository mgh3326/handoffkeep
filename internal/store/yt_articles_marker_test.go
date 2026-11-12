package store

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

// hk 4295: before the render escaped a quoted op marker, the article_create
// of a document whose body quoted it was rejected invalid_issue as it was
// prepared (15 documents on production, 2026-11-12). Such a create made no
// article, so the feed and the rescan replace it with one retry, which is
// sent with the quote escaped and binds the document.

const ytartQuoted = "the marker is `<!-- hk-op:123 -->`, twice: <!-- hk-op:"

// ytartRejectedCreate stores document id's first article_create rejected
// with reason before any claim, as the prepare-time refusal did.
func ytartRejectedCreate(t *testing.T, s *Store, id int64, key, body, reason string) ytwrite.Op {
	t.Helper()
	ctx := t.Context()
	n, err := ytwrite.ArticleOp(ytwrite.ArticleRequest{Principal: YTDocsPrincipal, Actor: YTDocsPrincipal, YTProjectID: ytartShared, YTProjectShort: "SHRDOC",
		Tenant: ytcTenant, DocumentID: id, Summary: YTArticleSummary(key, "note"), Body: body})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	now := time.Now().UTC()
	op, created, err := prepareYTOpTx(ctx, tx, now, n)
	if err != nil || !created {
		t.Fatalf("prepare: %v %v", created, err)
	}
	if op, err = rejectYTOpTx(ctx, tx, now, op.ID, reason, "ytwrite_invalid: document body contains the reserved hk op marker"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return op
}

func TestYTArticlesMarkerRejectedCreateIsRetried(t *testing.T) {
	s := ytartSetup(t, "ytart_marker", true)
	ctx := t.Context()
	yt := ytwritetest.NewFake()
	var offset atomic.Int64
	const key = "briefs/ytart/marker.md"
	id := ytartPut(t, s, key, ytartQuoted, nil)
	first := ytartRejectedCreate(t, s, id, key, ytartQuoted, ytwrite.ReasonInvalidIssue)
	if first.Key != ytwrite.ArticleKey(id) || first.Attempts != 0 {
		t.Fatalf("first %+v", first)
	}

	ytartPass(t, s)
	ops := ytartOps(t, s, id)
	if len(ops) != 2 || ops[1].Kind != ytwrite.KindArticleCreate || ops[1].State != ytwrite.StatePrepared ||
		ops[1].Key != ytwrite.ArticleRetryKey(id, first.ID) || ops[1].PayloadSHA != first.PayloadSHA {
		t.Fatalf("ops %+v", ops)
	}
	// The retry holds the create: another pass or a rescan prepares nothing.
	ytartTouch(t, s, id)
	ytartPass(t, s)
	ytartRescanDue(s)
	if _, err := s.RunYTFeedConsumers(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if n := len(ytartOps(t, s, id)); n != 2 {
		t.Fatalf("%d ops after a touch and a rescan", n)
	}

	ytartDrive(t, s, yt, &offset)
	retry := ytartOps(t, s, id)[1]
	if retry.State != ytwrite.StateApplied {
		t.Fatalf("retry %+v", retry)
	}
	a, _ := yt.Article(retry.YTResultID)
	if want := ytwrite.RenderArticleCreate(YTDocsPrincipal, retry.CreatedAt, retry.ID, ytartQuoted); a.Content != want ||
		strings.Count(a.Content, "<!-- hk-op:") != 1 {
		t.Fatalf("article %q\nwant %q", a.Content, want)
	}
	b := ytartBinding(t, s, id)
	if b.OpID != retry.ID || b.Origin != YTBindingOpApplied || b.ContentSHA != ytwrite.ContentSHA(ytartQuoted) || b.NeedsUpdate {
		t.Fatalf("binding %+v", b)
	}
	if n := ytartUnwritten(t, s); n != 0 {
		t.Fatalf("%d documents unwritten", n)
	}
}

// The rescan finds such a document too (a pre-deploy rejection has no new
// feed row), and a create rejected any other way stays the operator's.
func TestYTArticlesRescanRetriesOnlyPrepareTimeInvalid(t *testing.T) {
	s := ytartSetup(t, "ytart_marker_scan", true)
	ctx := t.Context()
	quoted := ytartPut(t, s, "briefs/ytart/quoted.md", ytartQuoted, nil)
	other := ytartPut(t, s, "briefs/ytart/other.md", "body", nil)
	ytartRejectedCreate(t, s, quoted, "briefs/ytart/quoted.md", ytartQuoted, ytwrite.ReasonInvalidIssue)
	ytartRejectedCreate(t, s, other, "briefs/ytart/other.md", "body", ytwrite.ReasonAmbiguousIssue)
	// Read their feed rows with docs off, as a pre-deploy pass did.
	s.SetYTDocsMode(YTDocsOff, "", nil)
	ytartPass(t, s)
	s.SetYTDocsMode(YTDocsOn, ytcTenant, ytartBuckets())
	if n := ytartUnwritten(t, s); n != 2 {
		t.Fatalf("%d documents unwritten before the rescan", n)
	}
	if _, err := s.RunYTFeedConsumers(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if ops := ytartOps(t, s, quoted); len(ops) != 2 || ops[1].State != ytwrite.StatePrepared || ops[1].Key != ytwrite.ArticleRetryKey(quoted, ops[0].ID) {
		t.Fatalf("quoted ops %+v", ops)
	}
	if ops := ytartOps(t, s, other); len(ops) != 1 {
		t.Fatalf("other ops %+v", ops)
	}
	if n := ytartUnwritten(t, s); n != 1 {
		t.Fatalf("%d documents unwritten, want the operator's one", n)
	}
}
