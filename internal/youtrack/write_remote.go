package youtrack

import (
	"context"
	"errors"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// WriteRemote is the ytwrite.Remote over YouTrack's REST API (slice b1a).
type WriteRemote struct{ c *Client }

var _ ytwrite.Remote = (*WriteRemote)(nil)

// NewWriteRemote wraps a configured client.
func NewWriteRemote(c *Client) *WriteRemote { return &WriteRemote{c: c} }

// CreateComment makes one POST /api/issues/{id}/comments.
func (r *WriteRemote) CreateComment(ctx context.Context, issueID, text string) (string, error) {
	return "", errors.New("not implemented")
}

// ListComments reads every comment of the issue.
func (r *WriteRemote) ListComments(ctx context.Context, issueID string, pageSize int) ([]ytwrite.RemoteComment, error) {
	return nil, errors.New("not implemented")
}

// GetIssue reads one issue for target verification.
func (r *WriteRemote) GetIssue(ctx context.Context, issueID string) (Issue, error) {
	return Issue{}, errors.New("not implemented")
}
