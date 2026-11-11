package youtrack

import (
	"strconv"
	"strings"
)

// The comment audit (hk 3899) reads the legacy mirror's evidence with the
// sync's own rules.

// CommentLinkIsID reports whether a youtrack_comment_links value is a
// YouTrack comment id, not one of the sync's synthetic markers: a machinery
// note it chose not to mirror (noiseLink) or a comment the Python mirror
// posted before the Go sync took over (adoptedLink).
func CommentLinkIsID(link string) bool {
	return link != "" && !strings.HasPrefix(link, "noise:") && !strings.HasPrefix(link, "adopted:")
}

// CommentLinkIsAdopted reports whether a link is the adoption's marker: the
// Python mirror posted the comment, in a form hk cannot recognise by id.
func CommentLinkIsAdopted(link string) bool { return strings.HasPrefix(link, "adopted:") }

// LegacyCommentID reads the hk comment id from a mirrored comment's
// <!-- hk-comment:N --> marker, the evidence commentMarks links by.
func LegacyCommentID(text string) (int64, bool) {
	m := commentMarkerRE.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	return id, err == nil && id > 0
}
