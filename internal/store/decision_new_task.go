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
// decision request on it, in one call. The request is checked first, so a
// refused request files nothing. The task is created through
// CreateTaskChecked, which refuses a similar open task: a retry after an
// unknown outcome gets that refusal instead of a second task. The task and
// the request are two writes; if the request write fails after the task
// exists, the task stays in backlog with no request and the caller re-sends
// with the request on that task.
func (s *Store) RecordDecisionRequestNewTask(ctx context.Context, by string, in NewTaskDecisionInput) (DecisionRequestResult, error) {
	if in.Project == "" {
		return DecisionRequestResult{}, ErrDecisionNewTaskProject
	}
	if !validName(in.Lane) {
		return DecisionRequestResult{}, invalidDecisionRequest("lane is required for a new task")
	}
	in.Request.RequestLane = in.Lane
	if err := CheckDecisionRequestInput(in.Request); err != nil {
		return DecisionRequestResult{}, err
	}
	project := in.Project
	task, err := s.CreateTaskChecked(ctx, Task{
		Lane:      in.Lane,
		Title:     in.Request.Question,
		Kind:      "decide",
		Project:   &project,
		CreatedBy: by,
	}, DuplicateCheck{})
	if err != nil {
		return DecisionRequestResult{}, err
	}
	return s.RecordDecisionRequest(ctx, task.ID, by, in.Request)
}
