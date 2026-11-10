package ytwrite

import (
	"context"
	"errors"
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

func (commentAppend) Send(ctx context.Context, remote Remote, op Op, text string) (string, error) {
	return remote.CreateComment(ctx, op.YTTargetID, text)
}

func (k commentAppend) Evidence(ctx context.Context, remote Remote, op Op, text string) (Evidence, error) {
	var ev Evidence
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
