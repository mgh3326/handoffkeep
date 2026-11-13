package youtrack

import "testing"

// The audit's reading of the legacy evidence follows the sync's own
// markers: the synthetic links are not ids, and a mirrored comment's text
// names its hk comment.
func TestAuditReadsLegacyCommentEvidence(t *testing.T) {
	for _, tc := range []struct {
		link        string
		id, adopted bool
	}{
		{"4-101", true, false},
		{"noise:5", false, false},
		{"adopted:5", false, true},
		{"", false, false},
	} {
		if CommentLinkIsID(tc.link) != tc.id || CommentLinkIsAdopted(tc.link) != tc.adopted {
			t.Errorf("link %q: id %v adopted %v, want %v %v", tc.link, CommentLinkIsID(tc.link), CommentLinkIsAdopted(tc.link), tc.id, tc.adopted)
		}
	}
	// The legacy mirror ended each comment with this trailer (hk comment 42).
	text := "**worker** · 2026-11-01 00:00 UTC\n\n본문\n\n<!-- hk-comment:42 -->"
	if id, ok := LegacyCommentID(text); !ok || id != 42 {
		t.Fatalf("LegacyCommentID(%q) = %d %v, want 42", text, id, ok)
	}
	for _, quoted := range []string{"no marker here", "quotes <!-- hk-comment:7 --> inline", "<!-- hk-comment:7 -->", text + "\n"} {
		if id, ok := LegacyCommentID(quoted); ok {
			t.Errorf("LegacyCommentID(%q) = %d: only the trailing marker counts", quoted, id)
		}
	}
	// A body quoting another marker before its own: the trailing one wins.
	if id, ok := LegacyCommentID("quotes <!-- hk-comment:7 -->\n\n<!-- hk-comment:8 -->"); !ok || id != 8 {
		t.Errorf("trailing marker = %d %v, want 8", id, ok)
	}
}
