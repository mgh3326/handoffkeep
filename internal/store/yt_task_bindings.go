package store

import (
	"context"
	"errors"
	"time"
)

// YTTaskBinding is a task's verified YouTrack issue for comment mode on.
type YTTaskBinding struct {
	TaskID        int64     `json:"task_id"`
	Tenant        string    `json:"tenant"`
	IssueID       string    `json:"issue_id"`
	IssueReadable string    `json:"issue_readable"`
	ProjectID     string    `json:"project_id"`
	BoundBy       string    `json:"bound_by"`
	BoundAt       time.Time `json:"bound_at"`
}

// ErrYTBindingConflict is a binding that disagrees with what hk keeps.
var ErrYTBindingConflict = errors.New("yt_binding_conflict")

// AddYTTaskBinding is not implemented yet.
func (s *Store) AddYTTaskBinding(ctx context.Context, b YTTaskBinding, verified YTCanaryCandidate) (YTTaskBinding, error) {
	return YTTaskBinding{}, errors.New("AddYTTaskBinding: not implemented")
}
