package store

import (
	"strconv"
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
	// The feed sets the key's family on every create; the first op must carry
	// it too, or the retry's payload hash differs.
	n, err := ytwrite.ArticleOp(ytwrite.ArticleRequest{Principal: YTDocsPrincipal, Actor: YTDocsPrincipal, YTProjectID: ytartShared, YTProjectShort: "SHRDOC",
		Tenant: ytcTenant, DocumentID: id, Summary: YTArticleSummary(key, "note"), Body: body, ParentFamily: ytArticleFamily(id, key)})
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

func TestYTArticleCreateClaimRechecksDocumentScope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove func(t *testing.T, s *Store, id int64, key string)
	}{
		{
			name: "deleted",
			remove: func(t *testing.T, s *Store, id int64, _ string) {
				t.Helper()
				if _, err := s.pool.Exec(t.Context(), `DELETE FROM documents WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "task report",
			remove: func(t *testing.T, s *Store, _ int64, key string) {
				t.Helper()
				task, err := s.CreateTask(t.Context(), Task{Lane: "ytart-lane", Title: "article scope", Kind: "implement", CreatedBy: "test"})
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := s.PutDocument(t.Context(), Document{Key: key, Kind: "report", Body: "body", CreatedBy: "test", TaskID: &task.ID}); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := ytartSetup(t, "ytart_claim_scope_"+strings.ReplaceAll(tc.name, " ", "_"), true)
			key := "reports/job/report.md"
			id := ytartPut(t, s, key, "body", nil)
			ytartPass(t, s)
			ops := ytartOps(t, s, id)
			if len(ops) != 1 || ops[0].State != ytwrite.StatePrepared {
				t.Fatalf("prepared ops %+v", ops)
			}

			tc.remove(t, s, id, key)
			yt := ytwritetest.NewFake()
			var offset atomic.Int64
			ytartDrive(t, s, yt, &offset)

			got := ytartOps(t, s, id)
			if len(got) != 1 || got[0].State != ytwrite.StateRejected || got[0].Reason != ytwrite.ReasonOutOfScope || got[0].Attempts != 0 {
				t.Fatalf("out-of-scope create %+v", got)
			}
			if len(yt.ArticleWrites()) != 0 {
				t.Fatalf("article writes after scope changed: %+v", yt.ArticleWrites())
			}
			if yt.Calls() != 0 {
				t.Fatalf("remote calls after scope changed: %d", yt.Calls())
			}
		})
	}
}

// hk 4357: at the deploy that prepared the retries, the previous colour,
// still serving, claimed them and its render refused the quoted marker
// (unsupported_kind, attempts 1: the claim counts the attempt before the
// render). Such a retry sent nothing, so it gets one more retry, keyed off
// it; that one is the last.

// ytartRefuse records op id rejected with reason after attempts claims, as
// a worker's refusal does.
func ytartRefuse(t *testing.T, s *Store, id int64, reason string, attempts int) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), `UPDATE yt_write_ops SET state='rejected', reason=$2, attempts=$3, resolved_at=now() WHERE id=$1`,
		id, reason, attempts); err != nil {
		t.Fatal(err)
	}
}

// ytartRescan runs a touch, a feed pass and a rescan for document id.
func ytartRescan(t *testing.T, s *Store, id int64) {
	t.Helper()
	ytartTouch(t, s, id)
	ytartPass(t, s)
	ytartRescanDue(s)
	if _, err := s.RunYTFeedConsumers(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
}

func TestYTArticlesRetryRefusedByOlderColourIsRetriedOnce(t *testing.T) {
	s := ytartSetup(t, "ytart_marker_again", true)
	yt := ytwritetest.NewFake()
	var offset atomic.Int64
	const key = "briefs/ytart/again.md"
	id := ytartPut(t, s, key, ytartQuoted, nil)
	first := ytartRejectedCreate(t, s, id, key, ytartQuoted, ytwrite.ReasonInvalidIssue)
	ytartPass(t, s)
	ops := ytartOps(t, s, id)
	if len(ops) != 2 || ops[1].Key != ytwrite.ArticleRetryKey(id, first.ID) {
		t.Fatalf("ops %+v", ops)
	}
	retry1 := ops[1]
	ytartRefuse(t, s, retry1.ID, ytwrite.ReasonUnsupportedKind, 1)
	if n := ytartUnwritten(t, s); n != 1 {
		t.Fatalf("%d documents unwritten after the refusal", n)
	}

	ytartRescan(t, s, id)
	ops = ytartOps(t, s, id)
	want := retry1.Key + ":retry-" + strconv.FormatInt(retry1.ID, 10)
	if len(ops) != 3 || ops[2].State != ytwrite.StatePrepared || ops[2].Key != want || ops[2].PayloadSHA != first.PayloadSHA {
		t.Fatalf("ops %+v, want a third keyed %q", ops, want)
	}
	if n := ytartUnwritten(t, s); n != 0 {
		t.Fatalf("%d documents unwritten with retry 2 prepared", n)
	}
	// Retry 2 holds the create: another pass prepares nothing.
	ytartRescan(t, s, id)
	if n := len(ytartOps(t, s, id)); n != 3 {
		t.Fatalf("%d ops after a touch and a rescan", n)
	}

	ytartDrive(t, s, yt, &offset)
	retry2 := ytartOps(t, s, id)[2]
	if retry2.State != ytwrite.StateApplied {
		t.Fatalf("retry 2 %+v", retry2)
	}
	if b := ytartBinding(t, s, id); b.OpID != retry2.ID || b.ContentSHA != ytwrite.ContentSHA(ytartQuoted) {
		t.Fatalf("binding %+v", b)
	}
}

// Retry 2 refused again, or a retry refused after an earlier attempt (which
// may have written) or any other way, stays the operator's.
func TestYTArticlesRetryRefusedOtherwiseStays(t *testing.T) {
	s := ytartSetup(t, "ytart_marker_stays", true)
	cases := []struct {
		name, reason string
		attempts     int
		levels       int
	}{
		{"again", ytwrite.ReasonUnsupportedKind, 1, 2},
		{"sent", ytwrite.ReasonUnsupportedKind, 2, 1},
		{"other", ytwrite.ReasonInvalidIssue, 1, 1},
		{"ambiguous", ytwrite.ReasonAmbiguousIssue, 1, 1},
	}
	ids := make([]int64, len(cases))
	for i, c := range cases {
		key := "briefs/ytart/stays-" + c.name + ".md"
		ids[i] = ytartPut(t, s, key, ytartQuoted, nil)
		ytartRejectedCreate(t, s, ids[i], key, ytartQuoted, ytwrite.ReasonInvalidIssue)
	}
	ytartPass(t, s)
	for i, c := range cases {
		for level := 1; level <= c.levels; level++ {
			ops := ytartOps(t, s, ids[i])
			if len(ops) != level+1 || ops[level].State != ytwrite.StatePrepared {
				t.Fatalf("%s level %d: ops %+v", c.name, level, ops)
			}
			reason, attempts := ytwrite.ReasonUnsupportedKind, 1
			if level == c.levels {
				reason, attempts = c.reason, c.attempts
			}
			ytartRefuse(t, s, ops[level].ID, reason, attempts)
			ytartRescan(t, s, ids[i])
		}
	}
	for i, c := range cases {
		if ops := ytartOps(t, s, ids[i]); len(ops) != c.levels+1 || ops[c.levels].State != ytwrite.StateRejected {
			t.Fatalf("%s: ops %+v", c.name, ops)
		}
	}
	if n := ytartUnwritten(t, s); n != len(cases) {
		t.Fatalf("%d documents unwritten, want the operator's %d", n, len(cases))
	}
}
