package ytwrite_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

// The document kinds (hk 4206): article_create makes a document's article
// once or adopts the one carrying its marker; article_update writes the
// whole summary and content to the bound article.

const (
	artProject = "0-30"
	artDocID   = int64(4206)
	artActor   = "hk:documents"
	artSummary = "[design] design/yt/articles.md [note]"
)

type articleEnv struct{ *kindEnv }

func newArticleEnv(t *testing.T) articleEnv { return articleEnv{newKindEnv(t)} }

func articleReq() ytwrite.ArticleRequest {
	return ytwrite.ArticleRequest{Principal: artActor, Actor: artActor, YTProjectID: artProject, YTProjectShort: "SHRDOC", Tenant: decTenant,
		DocumentID: artDocID, Summary: artSummary, Body: "# 문서\n\n본문 그대로.\n"}
}

func (e articleEnv) prepareCreate() ytwrite.Op {
	e.t.Helper()
	n, err := ytwrite.ArticleOp(articleReq())
	if err != nil {
		e.t.Fatal(err)
	}
	op, _, err := e.ledger.Prepare(context.Background(), e.clock.Now(), n)
	if err != nil {
		e.t.Fatal(err)
	}
	return op
}

func (e articleEnv) wantMap(op ytwrite.Op, articleID string, adopted bool) {
	e.t.Helper()
	m, ok, err := e.ledger.MapGet(context.Background(), ytwrite.ArticleEntity, "4206")
	if err != nil || !ok || m.YTID != articleID || m.YTParentID != artProject || m.OpID != op.ID || m.Adopted != adopted {
		e.t.Fatalf("document map row %+v %v %v, want article %s op %d adopted %v", m, ok, err, articleID, op.ID, adopted)
	}
}

func TestArticleCreateCreatesOneWithProvenanceAndMarker(t *testing.T) {
	e := newArticleEnv(t)
	op := e.prepareCreate()
	if op.Key != "document-article:4206" || op.EntityKind != "document" || op.EntityID != "4206" || op.YTTargetID != artProject {
		t.Fatalf("op %+v", op)
	}
	e.drive()
	got := e.want(op.ID, ytwrite.StateApplied, "")
	arts := e.yt.Articles(artProject)
	writes := e.yt.ArticleWrites()
	if len(arts) != 1 || arts[0].ID != got.YTResultID || len(writes) != 1 || e.yt.Finds() != 1 {
		t.Fatalf("articles %+v writes %+v finds %d", arts, writes, e.yt.Finds())
	}
	want := ytwrite.ProvenanceLine(artActor, op.CreatedAt, op.ID) + "\n\n" + articleReq().Body + "\n\n" + ytwrite.OpMarker(op.ID)
	if arts[0].Summary != artSummary || arts[0].Content != want {
		t.Fatalf("article %+v\nwant content %q", arts[0], want)
	}
	e.wantMap(got, got.YTResultID, false)
}

// An article that already carries the op's marker is adopted, with no POST;
// an article with the summary but no marker (another document's, or a
// person's) is not.
func TestArticleCreateAdoptsTheMarkedArticle(t *testing.T) {
	e := newArticleEnv(t)
	op := e.prepareCreate()
	e.yt.InjectArticle(artProject, artSummary, "someone else's text")
	marked := e.yt.InjectArticle(artProject, artSummary, "edited by hand\n\n"+ytwrite.OpMarker(op.ID))
	e.drive()
	got := e.want(op.ID, ytwrite.StateApplied, "")
	if got.YTResultID != marked || e.yt.Calls() != 0 {
		t.Fatalf("op %+v writes %d", got, e.yt.Calls())
	}
	e.wantMap(got, marked, true)
}

func TestArticleCreateTwoMarkedSendsNothing(t *testing.T) {
	e := newArticleEnv(t)
	op := e.prepareCreate()
	e.yt.InjectArticle(artProject, artSummary, ytwrite.OpMarker(op.ID))
	e.yt.InjectArticle(artProject, artSummary, "x "+ytwrite.OpMarker(op.ID))
	e.drive()
	e.want(op.ID, ytwrite.StateRejected, ytwrite.ReasonAmbiguousIssue)
	if e.yt.Calls() != 0 {
		t.Fatalf("%d writes", e.yt.Calls())
	}
}

// A lost reply leaves the create remote_unknown; the exact content with the
// marker is the evidence that applies it, with no second POST.
func TestArticleCreateLostReplyEvidenceApplies(t *testing.T) {
	e := newArticleEnv(t)
	e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.CommitDropReply})
	op := e.prepareCreate()
	e.drive()
	e.want(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
	e.clock.Advance(time.Hour)
	e.drive()
	got := e.want(op.ID, ytwrite.StateApplied, "")
	arts := e.yt.Articles(artProject)
	if len(arts) != 1 || got.YTResultID != arts[0].ID || e.yt.Calls() != 1 {
		t.Fatalf("op %+v articles %+v writes %d", got, arts, e.yt.Calls())
	}
	e.wantMap(got, arts[0].ID, false)
}

// A 5xx whose create did not commit, then an article that only quotes the
// marker: no exact match, the op stays remote_unknown and nothing is resent.
func TestArticleCreateLostReplyWithoutEvidenceStaysUnknown(t *testing.T) {
	e := newArticleEnv(t)
	e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.Refuse, Status: http.StatusBadGateway})
	op := e.prepareCreate()
	e.drive()
	e.want(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonServerError)
	e.clock.Advance(time.Hour)
	e.drive()
	e.want(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonNoEvidence)
	e.yt.InjectArticle(artProject, artSummary, "copied\n\n"+ytwrite.OpMarker(op.ID))
	e.clock.Advance(time.Hour)
	e.drive()
	e.want(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonMarkerMismatch)
	if e.yt.Calls() != 1 {
		t.Fatalf("%d writes", e.yt.Calls())
	}
}

func TestCheckArticle(t *testing.T) {
	if err := ytwrite.CheckArticle(articleReq()); err != nil {
		t.Fatal(err)
	}
	empty := articleReq()
	empty.Body = ""
	if err := ytwrite.CheckArticle(empty); err != nil {
		t.Fatalf("an empty document was refused: %v", err)
	}
	for name, edit := range map[string]func(*ytwrite.ArticleRequest){
		"marker":         func(r *ytwrite.ArticleRequest) { r.Body = "quoting " + ytwrite.OpMarker(1) },
		"nul":            func(r *ytwrite.ArticleRequest) { r.Body = "a\x00b" },
		"oversize":       func(r *ytwrite.ArticleRequest) { r.Body = strings.Repeat("a", ytwrite.MaxArticleBytes+1) },
		"empty_summary":  func(r *ytwrite.ArticleRequest) { r.Summary = "" },
		"long_summary":   func(r *ytwrite.ArticleRequest) { r.Summary = strings.Repeat("가", ytwrite.ArticleSummaryLimit+1) },
		"no_project":     func(r *ytwrite.ArticleRequest) { r.YTProjectID = "" },
		"no_tenant":      func(r *ytwrite.ArticleRequest) { r.Tenant = "" },
		"newline_header": func(r *ytwrite.ArticleRequest) { r.Summary = "a\nb" },
	} {
		r := articleReq()
		edit(&r)
		if err := ytwrite.CheckArticle(r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	at := strings.Repeat("a", ytwrite.MaxArticleBytes)
	full := articleReq()
	full.Body = at
	if err := ytwrite.CheckArticle(full); err != nil {
		t.Fatalf("a body of exactly MaxArticleBytes was refused: %v", err)
	}
}

func (e articleEnv) prepareUpdate(articleID, key, content string) ytwrite.Op {
	e.t.Helper()
	n, err := ytwrite.ArticleUpdateOp(ytwrite.ArticleUpdateRequest{Principal: artActor, Actor: artActor, Tenant: decTenant,
		ArticleID: articleID, DocumentID: artDocID, Summary: artSummary, Content: content})
	if err != nil {
		e.t.Fatal(err)
	}
	n.Key = key
	op, _, err := e.ledger.Prepare(context.Background(), e.clock.Now(), n)
	if err != nil {
		e.t.Fatal(err)
	}
	return op
}

func TestArticleUpdateWritesAndEvidence(t *testing.T) {
	e := newArticleEnv(t)
	art := e.yt.InjectArticle(artProject, "[design] old", "old body")
	op := e.prepareUpdate(art, "article_update:4206:1:a", "new body v1")
	if op.EntityKind != "document" || op.YTTargetID != art {
		t.Fatalf("op %+v", op)
	}
	e.drive()
	e.want(op.ID, ytwrite.StateApplied, "")
	if got, _ := e.yt.Article(art); got.Summary != artSummary || got.Content != "new body v1" {
		t.Fatalf("article %+v", got)
	}

	// Lost reply: the GET that compares the content's sha applies it.
	e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.CommitDropReply})
	lost := e.prepareUpdate(art, "article_update:4206:2:b", "new body v2")
	e.drive()
	e.want(lost.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
	e.clock.Advance(time.Hour)
	e.drive()
	e.want(lost.ID, ytwrite.StateApplied, "")

	// A lost reply whose write did not land: the content differs, unknown.
	e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.Refuse, Status: http.StatusBadGateway})
	miss := e.prepareUpdate(art, "article_update:4206:3:c", "new body v3")
	e.drive()
	e.want(miss.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonServerError)
	e.clock.Advance(time.Hour)
	e.drive()
	e.want(miss.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonMarkerMismatch)
	if n := len(e.yt.ArticleWrites()); n != 3 {
		t.Fatalf("%d writes, want 3 (no resend)", n)
	}
}

// An update of an article that is gone is a 404: rejected, never retried.
func TestArticleUpdateMissingArticleRejected(t *testing.T) {
	e := newArticleEnv(t)
	op := e.prepareUpdate("3-999", "article_update:4206:1:a", "body")
	e.drive()
	e.want(op.ID, ytwrite.StateRejected, "http_404")
}

func TestArticleKindsRefuseATaskLane(t *testing.T) {
	e := newArticleEnv(t)
	n, err := ytwrite.ArticleOp(articleReq())
	if err != nil {
		t.Fatal(err)
	}
	n.EntityKind = "task"
	op, _, err := e.ledger.Prepare(context.Background(), e.clock.Now(), n)
	if err != nil {
		t.Fatal(err)
	}
	e.drive()
	e.want(op.ID, ytwrite.StateRejected, ytwrite.ReasonUnsupportedKind)
	if e.yt.Calls() != 0 || e.yt.Finds() != 0 {
		t.Fatalf("%d writes %d finds", e.yt.Calls(), e.yt.Finds())
	}
}

// An operator closing an unknown article_create must say whether the article
// was created by the op or adopted: the binding's needs_update depends on it.
func TestArticleCreateResolveNeedsProvenance(t *testing.T) {
	e := newArticleEnv(t)
	e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.Refuse, Status: http.StatusBadGateway})
	op := e.prepareCreate()
	e.drive()
	e.want(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonServerError)
	if _, err := e.p.Resolve(context.Background(), op.ID, ytwrite.Resolution{Applied: true, YTID: "3-9", By: "operator"}); err == nil {
		t.Fatal("resolved without provenance")
	}
	adopted := true
	if _, err := e.p.Resolve(context.Background(), op.ID, ytwrite.Resolution{Applied: true, YTID: "3-9", Adopted: &adopted, By: "operator"}); err != nil {
		t.Fatal(err)
	}
	e.wantMap(e.want(op.ID, ytwrite.StateApplied, ""), "3-9", true)
}

// An article hk created (binding origin op_applied) keeps hk's format on
// update: the update op's provenance line, the body and its marker, sent and
// compared as such; a lost reply is applied by that exact rendering. An
// imported article gets the raw body (TestArticleUpdateWritesAndEvidence).
func TestArticleUpdateProvenanceKeepsHKFormat(t *testing.T) {
	e := newArticleEnv(t)
	art := e.yt.InjectArticle(artProject, artSummary, "created by an earlier op")
	prepare := func(key, body string) ytwrite.Op {
		t.Helper()
		n, err := ytwrite.ArticleUpdateOp(ytwrite.ArticleUpdateRequest{Principal: artActor, Actor: artActor, Tenant: decTenant,
			ArticleID: art, DocumentID: artDocID, Summary: artSummary, Content: body, Provenance: true})
		if err != nil {
			t.Fatal(err)
		}
		n.Key = key
		op, _, err := e.ledger.Prepare(context.Background(), e.clock.Now(), n)
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	op := prepare("article_update:4206:1:a", "edited body")
	e.drive()
	e.want(op.ID, ytwrite.StateApplied, "")
	want := ytwrite.RenderArticleCreate(artActor, op.CreatedAt, op.ID, "edited body")
	if got, _ := e.yt.Article(art); got.Content != want || got.Summary != artSummary {
		t.Fatalf("article %+v\nwant %q", got, want)
	}

	e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.CommitDropReply})
	lost := prepare("article_update:4206:2:b", "edited again")
	e.drive()
	e.want(lost.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
	e.clock.Advance(time.Hour)
	e.drive()
	e.want(lost.ID, ytwrite.StateApplied, "")
	if got, _ := e.yt.Article(art); got.Content != ytwrite.RenderArticleCreate(artActor, lost.CreatedAt, lost.ID, "edited again") {
		t.Fatalf("article %+v", got)
	}

	// The raw body is no evidence for a provenance update.
	e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.Refuse, Status: http.StatusBadGateway})
	miss := prepare("article_update:4206:3:c", "third")
	e.drive()
	if _, err := e.yt.UpdateArticle(context.Background(), art, artSummary, "third"); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Hour)
	e.drive()
	e.want(miss.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonMarkerMismatch)

	if _, err := ytwrite.ArticleUpdateOp(ytwrite.ArticleUpdateRequest{Principal: artActor, Actor: artActor, Tenant: decTenant,
		ArticleID: art, DocumentID: artDocID, Summary: artSummary, Content: "quoting " + ytwrite.OpMarker(1), Provenance: true}); err == nil {
		t.Fatal("a provenance update quoting the marker was accepted")
	}
}

func (e articleEnv) prepareLegacyCreate(legacyID string) ytwrite.Op {
	e.t.Helper()
	req := articleReq()
	req.LegacyArticleID, req.LegacyMarker = legacyID, ytwrite.LegacyDocMarker("design/yt/articles.md")
	n, err := ytwrite.ArticleOp(req)
	if err != nil {
		e.t.Fatal(err)
	}
	op, _, err := e.ledger.Prepare(context.Background(), e.clock.Now(), n)
	if err != nil {
		e.t.Fatal(err)
	}
	return op
}

// A document the legacy sync wrote: its verified article (top level, in the
// op's project, carrying the document's legacy marker) is adopted with no
// POST and no search; one that does not verify, or is gone, rejects the op
// legacy_synced_unverified, never a second article; a failed read retries.
func TestArticleCreateLegacySyncedArticle(t *testing.T) {
	legacyBody := ytwrite.LegacyDocMarker("design/yt/articles.md") + "\n> hk 문서 `design/yt/articles.md`\n\nold body"
	t.Run("verified", func(t *testing.T) {
		e := newArticleEnv(t)
		legacy := e.yt.InjectArticle(artProject, "design/yt/articles.md", legacyBody)
		op := e.prepareLegacyCreate(legacy)
		e.drive()
		got := e.want(op.ID, ytwrite.StateApplied, "")
		if got.YTResultID != legacy || e.yt.Calls() != 0 || e.yt.Finds() != 0 {
			t.Fatalf("op %+v writes %d finds %d", got, e.yt.Calls(), e.yt.Finds())
		}
		e.wantMap(got, legacy, true)
	})
	for name, setup := range map[string]func(e articleEnv) string{
		"no_marker":     func(e articleEnv) string { return e.yt.InjectArticle(artProject, "x", "old body without the marker") },
		"other_project": func(e articleEnv) string { return e.yt.InjectArticle("0-99", "x", legacyBody) },
		"other_marker":  func(e articleEnv) string { return e.yt.InjectArticle(artProject, "x", ytwrite.LegacyDocMarker("other/key")+"\nbody") },
		"gone":          func(articleEnv) string { return "3-404" },
	} {
		t.Run(name, func(t *testing.T) {
			e := newArticleEnv(t)
			op := e.prepareLegacyCreate(setup(e))
			e.drive()
			e.want(op.ID, ytwrite.StateRejected, ytwrite.ReasonLegacySyncedUnverified)
			if e.yt.Calls() != 0 || len(e.yt.ArticleWrites()) != 0 {
				t.Fatalf("%d writes", e.yt.Calls())
			}
		})
	}
	t.Run("read_failed", func(t *testing.T) {
		e := newArticleEnv(t)
		legacy := e.yt.InjectArticle(artProject, "x", legacyBody)
		e.yt.QueueRead(&ytwrite.RemoteError{Status: http.StatusBadGateway, Message: "down"})
		op := e.prepareLegacyCreate(legacy)
		e.drive()
		e.want(op.ID, ytwrite.StatePrepared, ytwrite.ReasonNotSent)
		if e.yt.Calls() != 0 {
			t.Fatalf("%d writes", e.yt.Calls())
		}
	})
}

// The legacy marker is the one the legacy sync writes (internal/youtrack
// docMarker delegates to it).
func TestLegacyDocMarker(t *testing.T) {
	if got := ytwrite.LegacyDocMarker("design/x.md"); got != "<!-- hk-marker:"+ytwrite.LegacyDocToken("design/x.md")+" -->" ||
		len(ytwrite.LegacyDocToken("design/x.md")) != len("hkdoc")+24 || !strings.HasPrefix(got, "<!-- hk-marker:hkdoc") {
		t.Fatalf("marker %q", got)
	}
}
