package youtrack

import (
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// The audit's reading of the legacy evidence follows the sync's own
// markers: the synthetic links are not ids, and a mirrored comment's text
// names its hk comment.
func TestAuditReadsLegacyCommentEvidence(t *testing.T) {
	for _, tc := range []struct {
		link        string
		id, adopted bool
	}{
		{"4-101", true, false},
		{noiseLink(5), false, false},
		{adoptedLink(5), false, true},
		{"", false, false},
	} {
		if CommentLinkIsID(tc.link) != tc.id || CommentLinkIsAdopted(tc.link) != tc.adopted {
			t.Errorf("link %q: id %v adopted %v, want %v %v", tc.link, CommentLinkIsID(tc.link), CommentLinkIsAdopted(tc.link), tc.id, tc.adopted)
		}
	}
	text := commentText(store.TaskComment{ID: 42, Body: "본문", Author: "worker", CreatedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)})
	if id, ok := LegacyCommentID(text); !ok || id != 42 {
		t.Fatalf("LegacyCommentID(%q) = %d %v, want 42", text, id, ok)
	}
	if _, ok := LegacyCommentID("no marker here"); ok {
		t.Fatal("a text without the marker names a comment")
	}
}
