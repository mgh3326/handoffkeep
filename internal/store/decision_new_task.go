package store

import (
	"context"
	"errors"
)

// NewTaskDecisionInput is `decisions ask --new-task`: a question with no task
// yet. The task is filed in Project on Lane, titled with the question, and
// the request is recorded on it.
type NewTaskDecisionInput struct {
	Project string               `json:"project"`
	Lane    string               `json:"lane"`
	Request DecisionRequestInput `json:"request"`
}

// ErrDecisionNewTaskProject refuses a new-task request that names no project.
var ErrDecisionNewTaskProject = errors.New("decision_new_task_project")

// RecordDecisionRequestNewTask files a kind=decide task and records its
// decision request on it, in one call. Every refusal the request write can
// raise runs before the task row is created, so a refused request files
// nothing: the request's shape, its secret scan and its option attachments
// are checked first. The task is then created through
// CreateTaskChecked, which refuses a similar open task, so a retry after an
// unknown outcome gets that refusal instead of a second task. What remains
// after the create is the request write's database failures; if one of
// those fails, the task stays in backlog with no request and the caller
// re-sends with the request on that task.
func (s *Store) RecordDecisionRequestNewTask(ctx context.Context, by string, in NewTaskDecisionInput) (DecisionRequestResult, error) {
	if in.Project == "" {
		return DecisionRequestResult{}, ErrDecisionNewTaskProject
	}
	if !validName(in.Lane) {
		return DecisionRequestResult{}, invalidDecisionRequest("lane is required for a new task")
	}
	in.Request.RequestLane = in.Lane
	// taskID 1 stands in for the task that does not exist yet: checkDecisionRequestWrite
	// uses it only for its positive-id check, and runs the shape and secret checks.
	if err := checkDecisionRequestWrite(1, by, &in.Request); err != nil {
		return DecisionRequestResult{}, err
	}
	if err := s.checkNewTaskAttachments(ctx, in.Request.Options); err != nil {
		return DecisionRequestResult{}, err
	}
	project := in.Project
	task, err := s.CreateTaskChecked(ctx, Task{
		Lane:      in.Lane,
		Title:     in.Request.Question,
		Kind:      "decide",
		Project:   &project,
		CreatedBy: by,
		// The task exists only to carry this question: when the request is
		// answered, withdrawn or default-applied it closes to completed
		// (operator decision 417, hk 5201).
		Refs: TaskRefs{QuestionTask: true},
	}, DuplicateCheck{})
	if err != nil {
		return DecisionRequestResult{}, err
	}
	return s.RecordDecisionRequest(ctx, task.ID, by, in.Request)
}

// checkNewTaskAttachments runs the option attachment existence check in a
// read-only transaction, before the new task's row is written.
func (s *Store) checkNewTaskAttachments(ctx context.Context, options DecisionOptions) error {
	tx, release, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	defer tx.Rollback(ctx)
	return checkDecisionAttachmentsExistTx(ctx, tx, options)
}
