package ytwrite

import "unicode/utf8"

// YouTrack's bounds, measured on the operator's instance on 2026-11-13 (the
// measurement comment on hk 4402). They are the only size numbers hk refuses
// or splits text by for task titles, task text, comments and documents: text
// YouTrack would accept is accepted here, and text it would refuse is refused
// with a code that names the bound. A request-body fuse is a server-memory
// bound, not a text limit, and lives with its route.
const (
	// TextMaxChars bounds one YouTrack text field, a comment body or an issue
	// description: one JSON string of at most this many characters (code
	// points, not bytes).
	TextMaxChars = 20_000_000
	// SummaryMaxChars bounds an issue summary. A longer title is cut to it,
	// the summary rule, and is never refused.
	SummaryMaxChars = 250
	// ArticlePartMaxBytes bounds one article part, in UTF-8 bytes. A longer
	// document is split over more parts instead of being refused.
	ArticlePartMaxBytes = 240 << 10
)

// WithinTextMax reports whether s fits one YouTrack text field.
func WithinTextMax(s string) bool { return utf8.RuneCountInString(s) <= TextMaxChars }
