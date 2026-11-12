package ytwrite_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

// A document body that quotes the reserved op marker (hk 4295: the hk 4206
// brief and reports) is sent with the quote escaped, so the op's own marker
// stays the only real one, and its evidence compares against that same
// rendering.

const (
	quotedOne = "The marker looks like `<!-- hk-op:123 -->`.\n"
	quotedTwo = "First <!-- hk-op:1 --> and\n```\n<!-- hk-op:\n```\nend <!-- hk-op:"
	// escapedPrefix is the escaped form: a zero-width space after "<".
	escapedPrefix = "<​!-- hk-op:"
)

func TestEscapeArticleBody(t *testing.T) {
	for _, c := range []struct {
		body, want string
	}{
		{quotedOne, "The marker looks like `" + escapedPrefix + "123 -->`.\n"},
		{quotedTwo, "First " + escapedPrefix + "1 --> and\n```\n" + escapedPrefix + "\n```\nend " + escapedPrefix},
		// Only the op marker is reserved; the legacy marker and the escaped
		// form itself are left as they are (the escape is one-way).
		{"<!-- hk-marker:x --> " + escapedPrefix, "<!-- hk-marker:x --> " + escapedPrefix},
	} {
		got := ytwrite.EscapeArticleBody(c.body)
		if got != c.want || ytwrite.HasOpMarker(got) {
			t.Errorf("EscapeArticleBody(%q) = %q, want %q", c.body, got, c.want)
		}
	}
}

// A body without the marker renders byte for byte as before the escape.
func TestArticleRenderWithoutMarkerIsUnchanged(t *testing.T) {
	at := time.Date(2026, 11, 12, 13, 10, 0, 0, time.UTC)
	body := "# 문서\n\n본문 그대로. <!-- note --> `<!-- hk-marker:abc -->` &lt;!-- hk-op:\n"
	if got := ytwrite.EscapeArticleBody(body); got != body {
		t.Fatalf("escaped %q", got)
	}
	const golden = "작성: hk:documents · 2026-11-12T13:10:00Z · hk op 77\n\n" +
		"# 문서\n\n본문 그대로. <!-- note --> `<!-- hk-marker:abc -->` &lt;!-- hk-op:\n" +
		"\n\n<!-- hk-op:77 -->"
	if got := ytwrite.RenderArticleCreate(artActor, at, 77, body); got != golden {
		t.Fatalf("render %q\nwant   %q", got, golden)
	}
}

func TestArticleCreateEscapesQuotedMarkers(t *testing.T) {
	for name, body := range map[string]string{"one": quotedOne, "two": quotedTwo} {
		t.Run(name, func(t *testing.T) {
			e := newArticleEnv(t)
			req := articleReq()
			req.Body = body
			if err := ytwrite.CheckArticle(req); err != nil {
				t.Fatalf("refused: %v", err)
			}
			n, err := ytwrite.ArticleOp(req)
			if err != nil {
				t.Fatal(err)
			}
			// A lost reply: the evidence search must apply it from the
			// escaped rendering, with no second POST.
			e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.CommitDropReply})
			op, _, err := e.ledger.Prepare(context.Background(), e.clock.Now(), n)
			if err != nil {
				t.Fatal(err)
			}
			e.drive()
			e.want(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
			e.clock.Advance(time.Hour)
			e.drive()
			got := e.want(op.ID, ytwrite.StateApplied, "")
			arts := e.yt.Articles(artProject)
			if len(arts) != 1 || arts[0].ID != got.YTResultID || len(e.yt.ArticleWrites()) != 1 {
				t.Fatalf("articles %+v writes %d", arts, len(e.yt.ArticleWrites()))
			}
			content := arts[0].Content
			if want := ytwrite.RenderArticleCreate(artActor, op.CreatedAt, op.ID, body); content != want {
				t.Fatalf("content %q\nwant %q", content, want)
			}
			if c := strings.Count(content, "<!-- hk-op:"); c != 1 || !strings.HasSuffix(content, ytwrite.OpMarker(op.ID)) {
				t.Fatalf("content has %d real markers: %q", c, content)
			}
			id, gotBody, ok := ytwrite.ParseArticleContent(content)
			if !ok || id != op.ID || gotBody != ytwrite.EscapeArticleBody(body) {
				t.Fatalf("parsed %d %q %v", id, gotBody, ok)
			}
			if strings.ReplaceAll(gotBody, escapedPrefix, "<!-- hk-op:") != body {
				t.Fatalf("parsed body %q does not show as %q", gotBody, body)
			}
			e.wantMap(got, got.YTResultID, false)
		})
	}
}

// article_update escapes the body for both binding origins: the raw body of
// an imported article and hk's format of an article hk created.
func TestArticleUpdateEscapesQuotedMarkers(t *testing.T) {
	for _, provenance := range []bool{false, true} {
		e := newArticleEnv(t)
		art := e.yt.InjectArticle(artProject, artSummary, "old")
		n, err := ytwrite.ArticleUpdateOp(ytwrite.ArticleUpdateRequest{Principal: artActor, Actor: artActor, Tenant: decTenant,
			ArticleID: art, DocumentID: artDocID, Summary: artSummary, Content: quotedTwo, Provenance: provenance})
		if err != nil {
			t.Fatalf("provenance %v: refused: %v", provenance, err)
		}
		n.Key = "article_update:4206:1:a"
		e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.CommitDropReply})
		op, _, err := e.ledger.Prepare(context.Background(), e.clock.Now(), n)
		if err != nil {
			t.Fatal(err)
		}
		e.drive()
		e.want(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
		e.clock.Advance(time.Hour)
		e.drive()
		e.want(op.ID, ytwrite.StateApplied, "")
		want, real := ytwrite.EscapeArticleBody(quotedTwo), 0
		if provenance {
			want, real = ytwrite.RenderArticleCreate(artActor, op.CreatedAt, op.ID, quotedTwo), 1
		}
		got, _ := e.yt.Article(art)
		if got.Content != want || strings.Count(got.Content, "<!-- hk-op:") != real || len(e.yt.ArticleWrites()) != 1 {
			t.Fatalf("provenance %v: content %q\nwant %q, %d writes", provenance, got.Content, want, len(e.yt.ArticleWrites()))
		}
	}
}

func TestParseArticleContentRefusesOtherText(t *testing.T) {
	at := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	good := ytwrite.RenderArticleCreate(artActor, at, 5, "body")
	if id, body, ok := ytwrite.ParseArticleContent(good); !ok || id != 5 || body != "body" {
		t.Fatalf("parsed %d %q %v", id, body, ok)
	}
	for name, text := range map[string]string{
		"raw":          "body",
		"other_marker": ytwrite.ProvenanceLine(artActor, at, 5) + "\n\nbody\n\n" + ytwrite.OpMarker(6),
		"quoted":       ytwrite.ProvenanceLine(artActor, at, 5) + "\n\n<!-- hk-op:1 -->\n\n" + ytwrite.OpMarker(5),
	} {
		if _, _, ok := ytwrite.ParseArticleContent(text); ok {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestArticleRetryKey(t *testing.T) {
	req := articleReq()
	first, err := ytwrite.ArticleOp(req)
	if err != nil {
		t.Fatal(err)
	}
	req.RetryOf = 67208
	retry, err := ytwrite.ArticleOp(req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Key != "document-article:4206" || retry.Key != "document-article:4206:retry-67208" || retry.Key != ytwrite.ArticleRetryKey(artDocID, 67208) ||
		retry.PayloadSHA != first.PayloadSHA {
		t.Fatalf("first %q retry %q", first.Key, retry.Key)
	}
}
