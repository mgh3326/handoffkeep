package youtrack

import (
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// A document longer than one article part is split over more parts, and every
// part, marker included, stays under the measured article part bound.
func TestDocArticlePartsStayUnderTheMeasuredBound(t *testing.T) {
	content := strings.Repeat("문서 본문 ", 1<<17) // about 1.5 MB of UTF-8
	parts := docArticleParts(content, "handoffkeep-design-limits")
	if len(parts) < 2 {
		t.Fatalf("a %d-byte document was not split: %d part(s)", len(content), len(parts))
	}
	for i, p := range parts {
		if len(p) > ytwrite.ArticlePartMaxBytes {
			t.Fatalf("part %d is %d bytes, over the %d-byte bound", i+1, len(p), ytwrite.ArticlePartMaxBytes)
		}
	}
	if docArticleMaxBytes != ytwrite.ArticlePartMaxBytes {
		t.Fatalf("the mirror cuts article parts at %d, not the measured %d", docArticleMaxBytes, ytwrite.ArticlePartMaxBytes)
	}
}
