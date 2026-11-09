package store

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// YTCommentsMode gates slice b1a: whether a new hk comment on a canary task
// also gets a protocol-tracked copy on its YouTrack issue
// (HANDOFFKEEP_YT_COMMENTS_MODE).
type YTCommentsMode string

const (
	// YTCommentsOff writes no op row (the default).
	YTCommentsOff YTCommentsMode = "off"
	// YTCommentsCanary prepares one comment_append op for each new comment
	// on a task in yt_comment_canary, in the comment's own transaction.
	YTCommentsCanary YTCommentsMode = "canary"
)

// YTCommentPrincipal owns the comment-copy ops; their idempotency key is
// YTCommentKey(comment id), so one comment can have at most one op.
const YTCommentPrincipal = "hk:task-comments"

// YTCommentKey is the idempotency key of the op that copies one comment.
func YTCommentKey(commentID int64) string { return "task-comment:" + strconv.FormatInt(commentID, 10) }

// SetYTCommentsMode sets the gate for this process. serve calls it once at
// start, before any request.
func (s *Store) SetYTCommentsMode(m YTCommentsMode) {}

// YTCommentCanary is one task in the canary set with the issue it was bound
// to at enrollment: the tenant and the stable internal issue id (not the
// readable id, which changes when an issue moves project).
type YTCommentCanary struct {
	TaskID        int64     `json:"task_id"`
	Tenant        string    `json:"tenant"`
	IssueID       string    `json:"issue_id"`
	IssueReadable string    `json:"issue_readable"`
	ProjectID     string    `json:"project_id"`
	AddedBy       string    `json:"added_by"`
	AddedAt       time.Time `json:"added_at"`
}

// YTCanaryCandidate is what hk already keeps about a task's YouTrack issue,
// read in one statement for a canary enrollment.
type YTCanaryCandidate struct {
	TaskID int64
	// Project is the task's hk project ("" when it has none); ShortName and
	// ProjectID its exact youtrack_projects row ("" when unmapped: there is no
	// OTHER fallback here).
	Project   string
	ShortName string
	ProjectID string
	// MirrorIssueID and MirrorTenant are the old mirror's task_origins link.
	MirrorIssueID string
	MirrorTenant  string
	// MappedIssueID is the yt_entity_map task row.
	MappedIssueID string
}

// YTCanaryCandidate reads a task's mappings. A missing task is ErrTaskNotFound.
func (s *Store) YTCanaryCandidate(ctx context.Context, taskID int64) (YTCanaryCandidate, error) {
	return YTCanaryCandidate{}, errors.New("not implemented")
}

// AddYTCommentCanary puts a verified binding in the canary set. Adding the
// same binding again is a no-op; another binding for an enrolled task, or an
// issue another task is mapped to, is ErrYTCanaryConflict. Nothing else is
// written: an enrollment creates no op row.
func (s *Store) AddYTCommentCanary(ctx context.Context, c YTCommentCanary) (YTCommentCanary, error) {
	return YTCommentCanary{}, errors.New("not implemented")
}

// ErrYTCanaryConflict is an enrollment that disagrees with a binding or map
// row already stored.
var ErrYTCanaryConflict = errors.New("yt_canary_conflict")

// YTCommentAdmitted reports whether the worker may send an op for a task to
// this issue on this tenant: the task is still enrolled with that binding.
func (s *Store) YTCommentAdmitted(ctx context.Context, taskID int64, issueID, tenant string) (bool, error) {
	return false, errors.New("not implemented")
}

// RemoveYTCommentCanary takes a task out of the canary set; removed is false
// when it was not in it. Ops already prepared are not touched.
func (s *Store) RemoveYTCommentCanary(ctx context.Context, taskID int64) (bool, error) {
	return false, errors.New("not implemented")
}

// ListYTCommentCanary lists the canary set by task id.
func (s *Store) ListYTCommentCanary(ctx context.Context) ([]YTCommentCanary, error) {
	return nil, errors.New("not implemented")
}

// YTOpFilter selects ops for the operator's view. State is a lifecycle state,
// "pending" (prepared, dispatched or remote_unknown; the default) or "all".
type YTOpFilter struct {
	State   string
	TaskID  int64
	AfterID int64
	Limit   int
}

// YTOpListMax bounds one page of the op view.
const YTOpListMax = 500

// ListYTWriteOps lists ops in id order for the operator's view.
func (s *Store) ListYTWriteOps(ctx context.Context, f YTOpFilter) ([]ytwrite.Op, error) {
	return nil, errors.New("not implemented")
}
