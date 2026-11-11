package ytwrite

import "sort"

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
	// copy; any comment whose text is exactly Body is one too (the import
	// wrote the body as it is).
	OpID int64
	// MirrorYTID is the YouTrack comment the legacy mirror push posted for
	// it and recorded (youtrack_comment_links; "": none). That comment is
	// its copy whatever its text.
	MirrorYTID string
}

// AuditCopies is one hk comment and the YouTrack comments that copy it.
type AuditCopies struct {
	CommentID int64
	OpID      int64
	YTIDs     []string
}

// CommentAudit is one issue's comparison. Every hk comment is in exactly one
// of Present (one copy), Missing (none) and Duplicate (two or more); every
// live YouTrack comment is a copy of one hk comment or in YTOnlyBot (written
// by the bot) or YTOnlyHuman (anyone else). Deleted comments are left out.
type CommentAudit struct {
	Present     []AuditCopies
	Missing     []AuditCopies
	Duplicate   []AuditCopies
	YTOnlyBot   []string
	YTOnlyHuman []string
}

// AuditComments matches hk comments to YouTrack comments. A YouTrack comment
// whose id is an hk comment's MirrorYTID copies that comment; one in the
// rendered form of op N (RenderedOpID) copies the hk comment whose OpID is
// N. Any other one copies an hk comment whose body equals its text
// exactly, not trimmed or normalised: equal bodies are shared out one copy
// each in comment id order, first to the comments with no copy yet, and a
// copy left over goes to the last of them, which makes that one a
// duplicate. botID is the bot's YouTrack user id.
func AuditComments(hk []AuditComment, yt []RemoteComment, botID string) CommentAudit {
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
	byMirror := map[string]int{}
	byBody := map[string][]int{}
	for i, c := range hk {
		if c.OpID > 0 {
			byOp[c.OpID] = i
		}
		if c.MirrorYTID != "" {
			byMirror[c.MirrorYTID] = i
		}
		byBody[c.Body] = append(byBody[c.Body], i)
	}
	var byText []RemoteComment
	for _, c := range live {
		if i, ok := byMirror[c.ID]; ok {
			copies[i] = append(copies[i], c.ID)
			continue
		}
		if op, ok := RenderedOpID(c.Text); ok {
			if i, ok := byOp[op]; ok {
				copies[i] = append(copies[i], c.ID)
				continue
			}
		}
		byText = append(byText, c)
	}
	var out CommentAudit
	for _, c := range byText {
		group := byBody[c.Text]
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
			out.Missing = append(out.Missing, got)
		case 1:
			out.Present = append(out.Present, got)
		default:
			out.Duplicate = append(out.Duplicate, got)
		}
	}
	return out
}
