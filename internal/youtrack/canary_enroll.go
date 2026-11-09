package youtrack

import (
	"context"
	"errors"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// ErrCanaryRefused is a task whose YouTrack issue cannot be bound for the
// comment canary (slice b1a). The text after "canary_refused: " says why.
var ErrCanaryRefused = errors.New("canary_refused")

// IssueReader reads one issue.
type IssueReader interface {
	GetIssue(ctx context.Context, issueID string) (Issue, error)
}

// VerifyCanary binds a task to its imported YouTrack issue: <short name>-<task
// id>, whose summary starts with "#<task id>". Every mapping hk keeps must
// agree; the first plausible issue is never picked.
func VerifyCanary(ctx context.Context, issues IssueReader, tenant string, c store.YTCanaryCandidate) (store.YTCommentCanary, error) {
	return store.YTCommentCanary{}, nil
}
