package ytwrite

import (
	"sort"
	"strings"
)

// The comment audit (hk 3899, `handoffkeep youtrack audit`) compares a bound
// task's hk comments with the comments on its YouTrack issue. It decides
// nothing about the op log: it reads two sets and says which hk comment has
// which YouTrack copies.

// AuditComment is one hk comment the audit expects on the issue.
type AuditComment struct {
	ID   int64
	Body string
	// OpID is the comment's own comment_append op on this issue (0: none).
	// A YouTrack comment in that op's rendered form (RenderedOpID) is its
	// copy. An unlinked imported comment may also match its body after
	// strings.TrimSpace, because YouTrack trims comment text.
	OpID int64
	// LinkedYTIDs are the YouTrack comments a recorded link names as its
	// copies (the legacy mirror push's youtrack_comment_links id): each is
	// its copy whatever its text.
	LinkedYTIDs []string
}

// AuditCopies is one hk comment and the YouTrack comments that copy it.
type AuditCopies struct {
	CommentID int64
	OpID      int64
	YTIDs     []string
}

// AuditAmbiguous is a YouTrack comment whose evidence names two hk comments:
// its text is one's exact body and ends in another's legacy trailer.
type AuditAmbiguous struct {
	YTID       string
	CommentIDs []int64
}

// CommentAudit is one issue's comparison. Every live YouTrack comment is a
// copy of one hk comment, in AmbiguousCopy (assigned to neither), or in
// YTOnlyBot (written by the bot) or YTOnlyHuman (anyone else). Every hk
// comment is in exactly one of Present (one copy), Duplicate (two or more)
// and Missing (none), except that one with no copy named by an
// AmbiguousCopy entry is listed there only: its copy may exist. Deleted
// comments are left out.
type CommentAudit struct {
	Present       []AuditCopies
	Missing       []AuditCopies
	Duplicate     []AuditCopies
	AmbiguousCopy []AuditAmbiguous
	YTOnlyBot     []string
	YTOnlyHuman   []string
}

// AuditComments matches hk comments to YouTrack comments, by the strongest
// evidence first: a YouTrack comment whose id is in an hk comment's
// LinkedYTIDs copies that comment; else one in the rendered form of op N
// (RenderedOpID) copies the hk comment whose OpID is N; else one whose
// legacy marker names hk comment N copies comment N, unless its text is also
// another comment's exact body (AmbiguousCopy); else exact bodies match. For
// unlinked imported comments only, surrounding whitespace is ignored because
// YouTrack trims comment text. If that makes one remote body match distinct
// hk bodies, the copy is ambiguous. Equal bodies are shared in comment id
// order; extra copies make the last comment a duplicate. botID is the bot's
// YouTrack user id.
func AuditComments(hk []AuditComment, yt []RemoteComment, botID string, legacy func(text string) (int64, bool)) CommentAudit {
	hk = append([]AuditComment(nil), hk...)
	sort.Slice(hk, func(i, j int) bool { return hk[i].ID < hk[j].ID })
	live := make([]RemoteComment, 0, len(yt))
	for _, c := range yt {
		if !c.Deleted {
			live = append(live, c)
		}
	}
	// A listing has no order (EnumerateStableComments reads a set); sort by
	// id, shorter first, so "4-9" comes before "4-10" and a run is
	// repeatable.
	sort.Slice(live, func(i, j int) bool {
		a, b := live[i].ID, live[j].ID
		if len(a) != len(b) {
			return len(a) < len(b)
		}
		return a < b
	})
	copies := make([][]string, len(hk))
	byOp := map[int64]int{}
	byLink := map[string]int{}
	byID := map[int64]int{}
	byBody := map[string][]int{}
	byTrimmedBody := map[string][]int{}
	byTrimmedImportedBody := map[string][]int{}
	for i, c := range hk {
		byID[c.ID] = i
		if c.OpID > 0 {
			byOp[c.OpID] = i
		}
		for _, id := range c.LinkedYTIDs {
			byLink[id] = i
		}
		byBody[c.Body] = append(byBody[c.Body], i)
		byTrimmedBody[strings.TrimSpace(c.Body)] = append(byTrimmedBody[strings.TrimSpace(c.Body)], i)
		if c.OpID == 0 && len(c.LinkedYTIDs) == 0 {
			byTrimmedImportedBody[strings.TrimSpace(c.Body)] = append(byTrimmedImportedBody[strings.TrimSpace(c.Body)], i)
		}
	}
	var out CommentAudit
	unsettled := map[int64]bool{}
	var byText []RemoteComment
	for _, c := range live {
		if i, ok := byLink[c.ID]; ok {
			copies[i] = append(copies[i], c.ID)
			continue
		}
		if op, ok := RenderedOpID(c.Text); ok {
			if i, ok := byOp[op]; ok {
				copies[i] = append(copies[i], c.ID)
				continue
			}
		}
		if legacy != nil {
			if n, ok := legacy(c.Text); ok {
				if i, ok := byID[n]; ok {
					// The text may also be another comment's exact body
					// (a body can end in a quoted trailer): then the
					// evidence conflicts and the copy is no one's.
					others := []int64{}
					for _, j := range byBody[c.Text] {
						if j != i {
							others = append(others, hk[j].ID)
						}
					}
					if len(others) > 0 {
						ids := append([]int64{hk[i].ID}, others...)
						for _, id := range ids {
							unsettled[id] = true
						}
						out.AmbiguousCopy = append(out.AmbiguousCopy, AuditAmbiguous{YTID: c.ID, CommentIDs: ids})
						continue
					}
					copies[i] = append(copies[i], c.ID)
					continue
				}
			}
		}
		byText = append(byText, c)
	}
	for _, c := range byText {
		group := byBody[c.Text]
		trimmed := strings.TrimSpace(c.Text)
		ambiguityGroup := byTrimmedBody[trimmed]
		// A trimmed collision is ambiguous across all hk comments, even if
		// one candidate has op or mirror evidence. Trimming itself remains
		// attribution evidence only for unlinked imported comments.
		if len(ambiguityGroup) > 1 && hasDistinctBodies(hk, ambiguityGroup) {
			ids := make([]int64, 0, len(ambiguityGroup))
			for _, i := range ambiguityGroup {
				ids = append(ids, hk[i].ID)
				unsettled[hk[i].ID] = true
			}
			out.AmbiguousCopy = append(out.AmbiguousCopy, AuditAmbiguous{YTID: c.ID, CommentIDs: ids})
			continue
		}
		if len(group) == 0 {
			group = byTrimmedImportedBody[trimmed]
		}
		if len(group) == 0 {
			if botID != "" && c.AuthorID == botID {
				out.YTOnlyBot = append(out.YTOnlyBot, c.ID)
			} else {
				out.YTOnlyHuman = append(out.YTOnlyHuman, c.ID)
			}
			continue
		}
		target := group[len(group)-1]
		for _, i := range group {
			if len(copies[i]) == 0 {
				target = i
				break
			}
		}
		copies[target] = append(copies[target], c.ID)
	}
	for i, c := range hk {
		got := AuditCopies{CommentID: c.ID, OpID: c.OpID, YTIDs: copies[i]}
		switch len(copies[i]) {
		case 0:
			if !unsettled[c.ID] {
				out.Missing = append(out.Missing, got)
			}
		case 1:
			out.Present = append(out.Present, got)
		default:
			out.Duplicate = append(out.Duplicate, got)
		}
	}
	return out
}

func hasDistinctBodies(hk []AuditComment, group []int) bool {
	if len(group) < 2 {
		return false
	}
	body := hk[group[0]].Body
	for _, i := range group[1:] {
		if hk[i].Body != body {
			return true
		}
	}
	return false
}
