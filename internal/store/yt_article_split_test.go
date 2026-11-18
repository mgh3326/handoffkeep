package store

import (
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

// The store side of article_split (hk 5085, design/2026-11-18/hk5085-yt-split).

// TestYTArticleSplitProtocolOverPostgres runs the article_split contract
// (S1-S9) over the Postgres ledger. One schema serves every subtest:
// yt_link_plans is append-only, so op ids keep counting up and an old plan
// row never names a new op.
func TestYTArticleSplitProtocolOverPostgres(t *testing.T) {
	s := schemaStore(t, "ytsplit")
	n := 0
	ytwritetest.RunSplitProtocol(t, func(t *testing.T) (ytwrite.Ledger, int64) {
		t.Helper()
		if _, err := s.pool.Exec(t.Context(), `TRUNCATE yt_link_owned, yt_link_unowned, yt_link_steps, yt_comment_write_links, yt_entity_map, yt_family_roots, yt_article_bindings, yt_write_ops, yt_write_op_adoptions, yt_issue_comment_bounds`); err != nil {
			t.Fatal(err)
		}
		n++
		return s.YTWriteLedger(), ytartPut(t, s, "note/ytsplit/doc-"+strconv.Itoa(n), "body", nil)
	})
}

// ytsplitBody is a document body that needs two parts.
func ytsplitBody(fill string) string {
	return strings.Repeat(fill+" line of a long document\n", YTArticleSafePartBytes/20)
}

// S11: with the flag off (the default) a new long document stays
// exception_not_updated and prepares no op, exactly as before.
func TestYTArticleSplitOffKeepsTheException(t *testing.T) {
	s := ytartSetup(t, "ytsplit_off", true)
	id := ytartPut(t, s, "note/ytsplit/off", ytsplitBody("a"), nil)
	ytartPass(t, s)
	if ops := ytartOps(t, s, id); len(ops) != 0 {
		t.Fatalf("flag off prepared %+v", ops)
	}
	if got := YTArticleExceptionOf(ytArticleDoc(t, s, id), ytcTenant); got != YTArticleExceptionNotUpdated {
		t.Fatalf("exception %q", got)
	}
	if !strings.Contains(ytArticleSplitGateSQL(s), `o.attempts > 0`) {
		t.Fatalf("flag off admits new split ops: %s", ytArticleSplitGateSQL(s))
	}
}

// S10, S1, S12: with the flag on a new long document gets one article_split
// op; its apply binds the document to the index with the full body's sha, so
// search maps a part (a child of the index) to the document; another pass
// prepares nothing; and an edit of the split document prepares no op.
func TestYTArticleSplitOnPreparesOneOpAndBindsTheIndex(t *testing.T) {
	s := ytartSetup(t, "ytsplit_on", true)
	s.SetYTArticleSplit(true)
	yt := ytwritetest.NewFake()
	var offset atomic.Int64
	body := ytsplitBody("a")
	// A split creates no family root: the family already has one.
	yt.InjectArticle(ytartShared, "note", ytwrite.FamilyRootContent("note"))
	id := ytartPut(t, s, "note/ytsplit/on", body, nil)
	ytartPass(t, s)
	ops := ytartOps(t, s, id)
	if len(ops) != 1 || ops[0].Kind != ytwrite.KindArticleSplit || ops[0].Key != ytwrite.ArticleKey(id) || ops[0].YTTargetID != ytartShared {
		t.Fatalf("ops %+v", ops)
	}
	ytartTouch(t, s, id)
	ytartPass(t, s)
	if got := ytartOps(t, s, id); len(got) != 1 {
		t.Fatalf("a second pass prepared %+v", got)
	}
	ytartDrive(t, s, yt, &offset)
	op, err := s.YTWriteLedger().ByID(t.Context(), ops[0].ID)
	if err != nil || op.State != ytwrite.StateApplied {
		t.Fatalf("op %+v %v", op, err)
	}
	b := ytartBinding(t, s, id)
	if b.ArticleID != op.YTResultID || b.ContentSHA != ytwrite.ContentSHA(body) || b.Origin != YTBindingOpApplied || b.OpID != op.ID || b.NeedsUpdate {
		t.Fatalf("binding %+v, want the index %s with the full body's sha", b, op.YTResultID)
	}
	index, _ := yt.Article(op.YTResultID)
	parts := 0
	for _, a := range yt.Articles(ytartShared) {
		if a.ParentID == index.ID {
			parts++
		}
	}
	if parts != 2 {
		t.Fatalf("%d parts under the index", parts)
	}
	hits, err := s.YouTrackDocHits(t.Context(), []string{index.ID}, "")
	if err != nil || hits[index.ID].Key != "note/ytsplit/on" {
		t.Fatalf("search hit of the index (a part's parent) %+v %v", hits, err)
	}

	// S12: the split document changes (still long, or now short): no op.
	for _, edit := range []string{ytsplitBody("b"), "now short"} {
		ytartPut(t, s, "note/ytsplit/on", edit, nil)
		ytartPass(t, s)
		if got := ytartOps(t, s, id); len(got) != 1 {
			t.Fatalf("an edit of a split document prepared %+v", got[1:])
		}
		if got := YTArticleExceptionOf(ytArticleDoc(t, s, id), ytcTenant); got != YTArticleExceptionNotUpdated {
			t.Fatalf("exception %q", got)
		}
	}
}

// S1, S9 at the store: once a document has an article_split op, whatever its
// state (here rejected partial_write with its index recorded), no other
// create is prepared for it, even when its body becomes short, and the
// backfill does not bind beside it.
func TestYTArticleSplitOpIsTheOnlyCreate(t *testing.T) {
	s := ytartSetup(t, "ytsplit_only", true)
	s.SetYTArticleSplit(true)
	id := ytartPut(t, s, "note/ytsplit/only", ytsplitBody("a"), nil)
	ytartPass(t, s)
	ops := ytartOps(t, s, id)
	if len(ops) != 1 {
		t.Fatalf("ops %+v", ops)
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE yt_write_ops SET state='rejected', reason=$2, yt_result_id='3-900', resolved_at=$3 WHERE id=$1`,
		ops[0].ID, ytwrite.ReasonPartialWrite, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	ytartPut(t, s, "note/ytsplit/only", "short now", nil)
	ytartPass(t, s)
	if got := ytartOps(t, s, id); len(got) != 1 {
		t.Fatalf("a document with a split op got another create: %+v", got[1:])
	}
	if _, err := s.catchUpYTArticle(t.Context(), time.Now().UTC(), mustYTDocsGate(t, s), id); err != nil {
		t.Fatal(err)
	}
	if got := ytartOps(t, s, id); len(got) != 1 {
		t.Fatalf("the catch-up prepared another create: %+v", got[1:])
	}
	d := ytArticleDoc(t, s, id)
	b := YTArticleBinding{DocumentID: id, Tenant: ytcTenant, ArticleID: "3-901", ProjectID: ytartShared, ContentSHA: d.SHA256, BoundBy: "test"}
	if _, err := s.AddYTArticleBinding(t.Context(), b, d.YTArticleCandidate); !errors.Is(err, ErrYTArticleBindingConflict) || !strings.Contains(err.Error(), "article_split") {
		t.Fatalf("the backfill beside a split op: %v", err)
	}
}

// S11: turning the flag off stops new split ops but admits one already
// dispatched (attempts > 0), so it still settles.
func TestYTArticleSplitGateAdmitsDispatchedOpsWhenOff(t *testing.T) {
	s := ytartSetup(t, "ytsplit_gate", true)
	s.SetYTArticleSplit(true)
	id := ytartPut(t, s, "note/ytsplit/gate", ytsplitBody("a"), nil)
	ytartPass(t, s)
	ops := ytartOps(t, s, id)
	if len(ops) != 1 {
		t.Fatalf("ops %+v", ops)
	}
	s.SetYTArticleSplit(false)
	l := s.YTTenantLedger(ytcTenant)
	if _, outcome, err := l.Claim(t.Context(), time.Now().UTC(), ops[0].ID); err != nil || outcome != ytwrite.ClaimHeld {
		t.Fatalf("a never-dispatched split op with the flag off: %v %v", outcome, err)
	}
	// Dispatched once with the flag on, then deferred (a 429).
	s.SetYTArticleSplit(true)
	if _, outcome, err := l.Claim(t.Context(), time.Now().UTC(), ops[0].ID); err != nil || outcome != ytwrite.ClaimClaimed {
		t.Fatalf("claim with the flag on: %v %v", outcome, err)
	}
	if _, err := l.Defer(t.Context(), time.Now().UTC(), ops[0].ID, time.Now().UTC().Add(-time.Second), ytwrite.ReasonRateLimited, "429"); err != nil {
		t.Fatal(err)
	}
	s.SetYTArticleSplit(false)
	if _, outcome, err := l.Claim(t.Context(), time.Now().UTC(), ops[0].ID); err != nil || outcome != ytwrite.ClaimClaimed {
		t.Fatalf("a deferred split op with the flag off: %v %v", outcome, err)
	}
}

func mustYTDocsGate(t *testing.T, s *Store) ytDocsGate {
	t.Helper()
	g, on := s.ytDocsNow()
	if !on {
		t.Fatal("docs mode is off")
	}
	return g
}
