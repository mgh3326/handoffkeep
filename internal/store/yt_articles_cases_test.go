package store

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The document-articles cases (hk 4206, hk 5077): they run on PostgreSQL and
// MySQL alike (yt_area_cases_test.go) and use the store's public methods
// only, so the article bindings and the create chain are exercised on both
// dialects.

const ytartCaseTenant = "ytcase.example"

func ytartCaseBuckets() map[string]YTArticleProject {
	return map[string]YTArticleProject{
		YTArticleBucketShared:     {ID: "0-40", ShortName: "SHRDOC"},
		YTArticleBucketUnassigned: {ID: "0-41", ShortName: "HK"},
	}
}

// ytartCaseDoc writes a document created now (after the import ended) and
// returns its id and the candidate YTArticleDocs lists for it.
func ytartCaseDoc(t *testing.T, s *Store, key, body string) (int64, YTArticleCandidate) {
	t.Helper()
	d, _, err := s.PutDocument(t.Context(), Document{Key: key, Kind: "note", Body: body, CreatedBy: "ytcase"})
	if err != nil {
		t.Fatal(err)
	}
	docs, err := s.YTArticleDocs(t.Context(), "", 0, YTArticleDocPageMax)
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range docs {
		if doc.DocumentID == d.ID {
			return d.ID, doc.YTArticleCandidate
		}
	}
	t.Fatalf("document %d is not listed by YTArticleDocs", d.ID)
	return 0, YTArticleCandidate{}
}

func ytartCaseOps(t *testing.T, s *Store, id int64) []ytwrite.Op {
	t.Helper()
	ops, err := s.YTWriteLedger().ListState(t.Context(), ytwrite.StatePrepared, YTArticleDocPageMax)
	if err != nil {
		t.Fatal(err)
	}
	out := []ytwrite.Op{}
	for _, op := range ops {
		if op.Kind == ytwrite.KindArticleCreate && op.EntityKind == ytwrite.ArticleEntity && op.EntityID == strconv.FormatInt(id, 10) {
			out = append(out, op)
		}
	}
	return out
}

// A verified binding round trips: AddYTArticleBinding stores it,
// YTArticleBinding and YTArticleDocs read it, adding it again is a no-op, and
// a different article for the same document is refused.
func ytArticlesBindingRoundTripCase(t *testing.T, s *Store) {
	ctx := t.Context()
	id, cand := ytartCaseDoc(t, s, "briefs/ytcase/round-trip.md", "the binding body")
	// A content sha marks the binding as the document's own article, not a
	// split document's index (content_sha NULL, which YTArticleDocs reports
	// as BoundSplitIndex).
	b := YTArticleBinding{DocumentID: id, Tenant: ytartCaseTenant, ArticleID: "0-40-A1", ProjectID: "0-40",
		ContentSHA: strings.Repeat("a", 64), BoundBy: "ytcase"}
	got, err := s.AddYTArticleBinding(ctx, b, cand)
	if err != nil {
		t.Fatal(err)
	}
	if got.DocumentID != id || got.Tenant != ytartCaseTenant || got.ArticleID != "0-40-A1" || got.Origin != YTBindingImportVerified {
		t.Fatalf("stored binding %+v", got)
	}
	read, found, err := s.YTArticleBinding(ctx, id)
	if err != nil || !found {
		t.Fatalf("read binding: found=%v err=%v", found, err)
	}
	if read.ArticleID != got.ArticleID || read.Tenant != got.Tenant || read.ProjectID != got.ProjectID {
		t.Fatalf("read %+v, stored %+v", read, got)
	}
	again, err := s.AddYTArticleBinding(ctx, b, cand)
	if err != nil {
		t.Fatal(err)
	}
	if again.ArticleID != b.ArticleID || again.BoundAt.IsZero() {
		t.Fatalf("second add %+v", again)
	}
	docs, err := s.YTArticleDocs(ctx, "", 0, YTArticleDocPageMax)
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, doc := range docs {
		if doc.DocumentID == id {
			listed = doc.BoundTenant == ytartCaseTenant && !doc.BoundSplitIndex
		}
	}
	if !listed {
		t.Fatalf("document %d is not listed as bound on %s", id, ytartCaseTenant)
	}
	other := b
	other.ArticleID = "0-40-A2"
	if _, err := s.AddYTArticleBinding(ctx, other, cand); !errors.Is(err, ErrYTArticleBindingConflict) {
		t.Fatalf("second article for the document: %v", err)
	}
}

// An article_create op prepared and applied through the ledger publishes the
// document's binding (origin op_applied, the op, content_sha of the body the
// op wrote); a binding published by hand under that unresolved op is refused.
func ytArticlesCreateApplyCase(t *testing.T, s *Store) {
	ctx := t.Context()
	now := time.Now().UTC()
	const body = "the article body"
	id, cand := ytartCaseDoc(t, s, "briefs/ytcase/apply.md", body)
	n, err := ytwrite.ArticleOp(ytwrite.ArticleRequest{Principal: YTDocsPrincipal, Actor: YTDocsPrincipal,
		YTProjectID: "0-40", YTProjectShort: "SHRDOC", Tenant: ytartCaseTenant, DocumentID: id,
		Summary: YTArticleSummary(cand.Key, cand.Kind), Body: body})
	if err != nil {
		t.Fatal(err)
	}
	op, created, err := s.YTWriteLedger().Prepare(ctx, now, n)
	if err != nil || !created {
		t.Fatalf("prepare: created=%v err=%v", created, err)
	}
	// A binding under an unresolved create would hold the op for good.
	if _, err := s.AddYTArticleBinding(ctx, YTArticleBinding{DocumentID: id, Tenant: ytartCaseTenant,
		ArticleID: "0-40-A9", ProjectID: "0-40", BoundBy: "ytcase"}, cand); !errors.Is(err, ErrYTArticleBindingConflict) {
		t.Fatalf("binding under an unresolved create: %v", err)
	}
	claimed, outcome, err := s.YTWriteLedger().Claim(ctx, now, op.ID)
	if err != nil || outcome != ytwrite.ClaimClaimed {
		t.Fatalf("claim: outcome=%s err=%v", outcome, err)
	}
	if _, err := s.YTWriteLedger().Apply(ctx, now, claimed.ID, "0-40-A7", nil); err != nil {
		t.Fatal(err)
	}
	b, found, err := s.YTArticleBinding(ctx, id)
	if err != nil || !found {
		t.Fatalf("binding: found=%v err=%v", found, err)
	}
	if b.ArticleID != "0-40-A7" || b.Tenant != ytartCaseTenant || b.Origin != YTBindingOpApplied || b.OpID != op.ID {
		t.Fatalf("published binding %+v", b)
	}
	if b.ContentSHA != ytwrite.ContentSHA(body) || b.NeedsUpdate {
		t.Fatalf("published content %+v", b)
	}
}

// The document_articles consumer, in docs mode on, prepares one create for a
// document created after the import ended, none for a document out of scope,
// and none on the next pass.
func ytArticlesDocsConsumerCase(t *testing.T, s *Store) {
	ctx := t.Context()
	s.SetYTDocsMode(YTDocsOn, ytartCaseTenant, ytartCaseBuckets())
	id, _ := ytartCaseDoc(t, s, "briefs/ytcase/consumer.md", "the consumer body")
	excluded, _ := ytartCaseDoc(t, s, "screens/ytcase/excluded.md", "the excluded body")
	if _, err := s.RunYTFeedConsumers(ctx, 100); err != nil {
		t.Fatal(err)
	}
	ops := ytartCaseOps(t, s, id)
	if len(ops) != 1 {
		t.Fatalf("%d prepared creates for document %d", len(ops), id)
	}
	if ops[0].Tenant != ytartCaseTenant || ops[0].YTTargetID != "0-40" {
		t.Fatalf("prepared create %+v", ops[0])
	}
	if n := len(ytartCaseOps(t, s, excluded)); n != 0 {
		t.Fatalf("%d prepared creates for an out-of-scope document", n)
	}
	if _, found, err := s.YTArticleBinding(ctx, id); err != nil || found {
		t.Fatalf("a prepared create published a binding: found=%v err=%v", found, err)
	}
	if _, err := s.RunYTFeedConsumers(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if n := len(ytartCaseOps(t, s, id)); n != 1 {
		t.Fatalf("%d prepared creates after a second pass", n)
	}
}

func init() {
	registerYTAreaCase("yt_articles_binding_round_trip", ytArticlesBindingRoundTripCase)
	registerYTAreaCase("yt_articles_create_apply", ytArticlesCreateApplyCase)
	registerYTAreaCase("yt_articles_docs_consumer", ytArticlesDocsConsumerCase)
}
