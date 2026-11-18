package store

import (
	"context"
	"testing"
)

// newQuestionTask files a kind=decide task through `decisions ask --new-task`
// (operator decision 417, hk 5201).
func newQuestionTask(t *testing.T, s *Store, project, lane string) DecisionRequestResult {
	t.Helper()
	ctx := context.Background()
	if _, err := s.AddTaskProject(ctx, project, "tester"); err != nil {
		t.Fatal(err)
	}
	in := decisionInput()
	in.Question = "question task for " + project
	res, err := s.RecordDecisionRequestNewTask(ctx, "tester", NewTaskDecisionInput{Project: project, Lane: lane, Request: in})
	if err != nil {
		t.Fatal(err)
	}
	if res.Task.Kind != "decide" || !res.Task.Refs.QuestionTask {
		t.Fatalf("new task = kind %q question_task %t, want decide true", res.Task.Kind, res.Task.Refs.QuestionTask)
	}
	return res
}

// answerDecision resolves one request on the verified cli path.
func answerDecision(t *testing.T, s *Store, taskID int64, requestID, kind, option, receipt string) ResolveDecisionOutcome {
	t.Helper()
	in := ResolveDecisionInput{
		TaskID: taskID, RequestID: requestID, Kind: kind, Option: option, Receipt: receipt,
		Responder: "operator", Channel: DecisionChannelCLI, Actor: "operator", Verified: true,
	}
	if kind == DecisionRequestWithdrawn {
		in.Text = "withdrawn in the test"
	}
	out, err := s.ResolveDecision(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// testQuestionTaskCloses pins the three closes decision 417 names (answered,
// withdrawn, default_applied) and the evidence the close records.
func testQuestionTaskCloses(t *testing.T, kind, option, receipt string) {
	t.Helper()
	ctx := context.Background()
	s := schemaStore(t, "qtask-"+kind)
	res := newQuestionTask(t, s, "question-project-"+kind, "qlane-"+kind)
	answerDecision(t, s, res.Task.ID, res.Request.ID, kind, option, receipt)
	got, found, err := s.GetTask(ctx, res.Task.ID)
	if err != nil || !found {
		t.Fatalf("GetTask: found=%t err=%v", found, err)
	}
	if got.State != StateCompleted {
		t.Fatalf("state = %s after %s; want completed", got.State, kind)
	}
	if got.CompletedAt == nil {
		t.Fatal("completed_at is not set")
	}
	if got.Refs.Closure == nil || got.Refs.Closure.Evidence != res.Request.ID || got.Refs.Closure.Type != "resolved" {
		t.Fatalf("closure = %+v, want resolved evidence %s", got.Refs.Closure, res.Request.ID)
	}
	if !got.Refs.QuestionTask {
		t.Fatal("question_task marker was dropped by the close")
	}
}

func TestQuestionTaskClosesWithAnswer(t *testing.T) {
	testQuestionTaskCloses(t, DecisionRequestAnswered, "A", "")
}

func TestQuestionTaskClosesWithWithdraw(t *testing.T) {
	testQuestionTaskCloses(t, DecisionRequestWithdrawn, "", "")
}

func TestQuestionTaskClosesWithDefaultApplied(t *testing.T) {
	testQuestionTaskCloses(t, DecisionRequestDefaultApplied, "", "default applied: held")
}

// TestDecideTaskThatCarriesWorkStaysOpen pins the boundary of 417: a decide
// task the session filed itself (no question_task marker) keeps its state.
func TestDecideTaskThatCarriesWorkStaysOpen(t *testing.T) {
	ctx := context.Background()
	s := schemaStore(t, "qtask-plain")
	if _, err := s.AddTaskProject(ctx, "plain-project", "tester"); err != nil {
		t.Fatal(err)
	}
	project := "plain-project"
	task, err := s.CreateTask(ctx, Task{Lane: "plain-lane", Title: "decide with work", Kind: "decide", Project: &project, CreatedBy: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := s.RecordDecisionRequest(ctx, task.ID, "tester", decisionInput())
	if err != nil {
		t.Fatal(err)
	}
	answerDecision(t, s, task.ID, req.Request.ID, DecisionRequestAnswered, "A", "")
	got, found, err := s.GetTask(ctx, task.ID)
	if err != nil || !found {
		t.Fatalf("GetTask: found=%t err=%v", found, err)
	}
	if got.State != StateBacklog {
		t.Fatalf("state = %s; want backlog (a decide task without the marker stays open)", got.State)
	}
	if got.CompletedAt != nil || got.Refs.Closure != nil {
		t.Fatalf("unexpected close: state=%s closure=%+v", got.State, got.Refs.Closure)
	}
}

// TestQuestionTaskWithChildStaysOpen pins the second boundary: a question task
// that somehow has a child is carrying work and stays open.
func TestQuestionTaskWithChildStaysOpen(t *testing.T) {
	ctx := context.Background()
	s := schemaStore(t, "qtask-child")
	res := newQuestionTask(t, s, "child-project", "child-lane")
	project := "child-project"
	child, err := s.CreateTask(ctx, Task{Lane: "child-lane", Title: "child of a question", Kind: "ops", Project: &project, ParentID: &res.Task.ID, CreatedBy: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	_ = child
	answerDecision(t, s, res.Task.ID, res.Request.ID, DecisionRequestAnswered, "A", "")
	got, found, err := s.GetTask(ctx, res.Task.ID)
	if err != nil || !found {
		t.Fatalf("GetTask: found=%t err=%v", found, err)
	}
	if got.State == StateCompleted {
		t.Fatalf("a question task with a child closed to %s", got.State)
	}
}
