package ytwrite

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

func init() {
	registerKind(KindCommentAppend, func(cfg Config) Kind { return commentAppend{pageSize: cfg.PageSize} })
}

// commentAppend is the comment_append kind: one comment appended to the
// target issue. Its evidence is the issue's comments, listed with pageSize.
type commentAppend struct{ pageSize int }

func (commentAppend) Render(op Op) (string, error) { return RenderComment(op) }

// Send refuses an op with no target. The store's claim rejects a comment
// queued behind its task's issue_create (hk 4263) that has no binding from
// that create before it dispatches it; this is the backstop that never calls
// YouTrack with an empty issue id.
func (commentAppend) Send(ctx context.Context, remote Remote, op Op, text string) (string, error) {
	if !issueIDRE.MatchString(op.YTTargetID) {
		return "", &RefusedError{Reason: ReasonIssueNotBound, Detail: "task " + op.EntityID + " has no binding from its issue_create op; the comment was queued behind it"}
	}
	return remote.CreateComment(ctx, op.YTTargetID, text)
}

func (k commentAppend) Evidence(ctx context.Context, remote Remote, op Op, text string) (Evidence, error) {
	var ev Evidence
	if !issueIDRE.MatchString(op.YTTargetID) {
		// Send refuses such an op before it writes, so it is never uncertain.
		return ev, fmt.Errorf("%w: comment op %d has no target issue", ErrInvalid, op.ID)
	}
	comments, err := remote.ListComments(ctx, op.YTTargetID, k.pageSize)
	if err != nil {
		// A rate-limited search waits at least as long as the server asked.
		var re *RemoteError
		if errors.As(err, &re) && re.Status == http.StatusTooManyRequests {
			ev.RetryAfter = re.RetryAfter
		}
		return ev, err
	}
	marker := OpMarker(op.ID)
	for _, c := range comments {
		if !strings.Contains(c.Text, marker) {
			continue
		}
		switch {
		case c.Text != text:
			ev.Mismatched++
		case c.Deleted:
			ev.Deleted++
		default:
			ev.Matches = append(ev.Matches, c.ID)
		}
	}
	return ev, nil
}

func (commentAppend) MapRows(op Op, ytID string) []MapRow { return []MapRow{commentMap(op, ytID)} }
