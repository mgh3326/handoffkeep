//go:build mysql

package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// TestMySQLNewTaskDecisionFilesTaskAndRequest runs `decisions ask --new-task`
// end to end on MySQL: the task is filed with the question as its title and
// the request is recorded on it; an option attachment no attachments row holds
// is refused before the task row is written, so nothing is filed.
func TestMySQLNewTaskDecisionFilesTaskAndRequest(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	n := strconv.FormatInt(time.Now().UnixNano(), 10)
	project := "newtask-mysql-" + n
	lane := "newtask-mysql-lane-" + n
	if _, err := s.AddTaskProject(ctx, project, "tester"); err != nil {
		t.Fatal(err)
	}

	question := "mysql new-task question " + n
	in := decisionInput()
	in.Question = question
	got, err := s.RecordDecisionRequestNewTask(ctx, "tester", NewTaskDecisionInput{Project: project, Lane: lane, Request: in})
	if err != nil {
		t.Fatalf("new task on mysql: %v", err)
	}
	if got.Duplicate || got.Task.ID < 1 {
		t.Fatalf("result=%+v, want a new task with an id", got)
	}
	if got.Task.Kind != "decide" || got.Task.Title != question || got.Task.Lane != lane {
		t.Fatalf("task=%+v, want kind decide titled with the question on lane %s", got.Task, lane)
	}
	stored, found, err := s.GetTask(ctx, got.Task.ID)
	if err != nil || !found {
		t.Fatalf("get task %d: found=%t err=%v", got.Task.ID, found, err)
	}
	if stored.Title != question || stored.Project == nil || *stored.Project != project {
		t.Fatalf("stored task=%+v, want the question on project %s", stored, project)
	}

	projectPtr := project
	refused := decisionInput()
	refused.Question = "mysql refused new-task " + n
	refused.Options.Options[0].Attachments = []DecisionOptionAttachment{{SHA256: strings.Repeat("b", 64)}}
	_, err = s.RecordDecisionRequestNewTask(ctx, "tester", NewTaskDecisionInput{Project: project, Lane: lane, Request: refused})
	if !errors.Is(err, ErrDecisionAttachmentNotFound) {
		t.Fatalf("err=%v, want ErrDecisionAttachmentNotFound", err)
	}
	filed, err := s.ListTasks(ctx, lane, "", "", &projectPtr, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(filed) != 1 {
		t.Fatalf("refused new-task call left %d task row(s) on lane %s, want the one filed above", len(filed), lane)
	}
}
