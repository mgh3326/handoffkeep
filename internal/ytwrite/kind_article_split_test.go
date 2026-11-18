package ytwrite_test

import (
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// S4 (4603799062): a 250-code-point base keeps every part's suffix, every
// part summary is within the limit, and no two are equal.
func TestSplitPartSummaryKeepsTheSuffix(t *testing.T) {
	base := strings.Repeat("가", ytwrite.ArticleSummaryLimit)
	const n = 12
	seen := map[string]bool{}
	for i := 1; i <= n; i++ {
		s := ytwrite.SplitPartSummary(base, i, n)
		suffix := " / part " + strconv.Itoa(i) + " of " + strconv.Itoa(n)
		if !strings.HasSuffix(s, suffix) || utf8.RuneCountInString(s) > ytwrite.ArticleSummaryLimit || seen[s] {
			t.Fatalf("part %d summary %q (%d code points)", i, s, utf8.RuneCountInString(s))
		}
		seen[s] = true
	}
	if got := ytwrite.SplitPartSummary("short", 1, 2); got != "short / part 1 of 2" {
		t.Fatalf("short summary %q", got)
	}
}

// Every rendered part stays within ArticlePartMaxBytes even with the longest
// actor, the chunks hold the escaped body in order, a quoted marker in the
// body never becomes a real one, and a body within the bound is not split.
func TestSplitArticleBodyBounds(t *testing.T) {
	actor := strings.Repeat("a", 200)
	at := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	var b strings.Builder
	for b.Len() < 3*ytwrite.ArticlePartMaxBytes {
		b.WriteString("한글 줄과 <!-- hk-op:7 --> 인용 ")
		b.WriteString(strings.Repeat("x", 500))
		b.WriteString("\n")
	}
	b.WriteString(strings.Repeat("y", ytwrite.ArticlePartMaxBytes)) // one line longer than a chunk
	body := b.String()
	chunks := ytwrite.SplitArticleBody(body)
	if strings.Join(chunks, "") != ytwrite.EscapeArticleBody(body) || len(chunks) < 4 {
		t.Fatalf("%d chunks do not hold the escaped body", len(chunks))
	}
	for i, c := range chunks {
		part := ytwrite.RenderSplitPart(actor, at, 999999999, i+1, len(chunks), c)
		if len(part) > ytwrite.ArticlePartMaxBytes || !utf8.ValidString(c) || c == "" {
			t.Fatalf("part %d: %d bytes, valid %v", i+1, len(part), utf8.ValidString(c))
		}
		if strings.Count(part, "<!-- hk-op:") != 1 {
			t.Fatalf("part %d carries %d real markers", i+1, strings.Count(part, "<!-- hk-op:"))
		}
	}
	if ytwrite.NeedsSplit(strings.Repeat("z", ytwrite.MaxArticleBytes)) || !ytwrite.NeedsSplit(strings.Repeat("z", ytwrite.MaxArticleBytes+1)) {
		t.Fatal("NeedsSplit is not len > MaxArticleBytes")
	}
}

// A part's marker is not its index's marker with a suffix: a marker search for
// the op never finds a part, and part i of n differs from part j.
func TestPartMarkerIsNotTheIndexMarker(t *testing.T) {
	if strings.Contains(ytwrite.PartMarker(12, 1, 2), ytwrite.OpMarker(12)) || strings.Contains(ytwrite.PartMarker(12, 1, 2), ytwrite.OpMarker(1)) {
		t.Fatal("a part marker contains an index marker")
	}
	if ytwrite.PartMarker(12, 1, 2) == ytwrite.PartMarker(12, 2, 2) {
		t.Fatal("two parts share a marker")
	}
}

// The split request: only a body over the bound, never a legacy article, and
// one op per document under its own kind.
func TestArticleSplitOpAndCheck(t *testing.T) {
	req := articleReq()
	req.Body = strings.Repeat("b", ytwrite.MaxArticleBytes+1)
	if err := ytwrite.CheckArticleSplit(req); err != nil {
		t.Fatal(err)
	}
	n, err := ytwrite.ArticleSplitOp(req)
	if err != nil || n.Kind != ytwrite.KindArticleSplit || n.Key != ytwrite.ArticleKey(artDocID) || n.EntityKind != ytwrite.ArticleEntity || n.EntityID != "4206" {
		t.Fatalf("op %+v %v", n, err)
	}
	short := articleReq()
	if err := ytwrite.CheckArticleSplit(short); err == nil {
		t.Fatal("a short body was accepted for a split")
	}
	legacy := req
	legacy.LegacyArticleID = "3-1"
	if err := ytwrite.CheckArticleSplit(legacy); err == nil {
		t.Fatal("a legacy article was accepted for a split")
	}
}
