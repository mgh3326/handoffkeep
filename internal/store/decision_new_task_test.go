package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/guard"
)

// Invariant: a secret-like ground is refused the same way a secret-like
// reason is, so a credential cannot reach task refs through grounds.
func TestDecisionRequestGroundSecretRefused(t *testing.T) {
	in := decisionInput()
	in.Grounds = []string{"the key is " + "AKIA" + strings.Repeat("X", 16)}
	if err := CheckDecisionRequestInput(in); err != nil {
		t.Fatalf("shape check refused it first: %v", err)
	}
	err := rejectDecisionRequestSecrets(in)
	if pattern, ok := guard.SecretPattern(err); !ok || pattern != "aws_access_key" {
		t.Fatalf("error=%v, secret pattern=%q ok=%t", err, pattern, ok)
	}
}

// Invariant: a new task is refused, with nothing written, before the request
// write can refuse it: a secret-like ground is caught before the pool is
// reached. The MySQL path runs in TestMySQLNewTaskDecisionFilesTaskAndRequest.
func TestNewTaskRefusedBeforeAnyWrite(t *testing.T) {
	ctx := context.Background()
	in := NewTaskDecisionInput{Project: "p", Lane: "mac-lane", Request: decisionInput()}
	secret := in
	secret.Request.Grounds = []string{"AKIA" + strings.Repeat("X", 16)}
	_, err := (&Store{}).RecordDecisionRequestNewTask(ctx, "tester", secret)
	if pattern, ok := guard.SecretPattern(err); !ok || pattern != "aws_access_key" {
		t.Fatalf("secret ground: err=%v, pattern=%q ok=%t", err, pattern, ok)
	}
}

// Invariant: an option attachment that no attachments row holds refuses the
// new-task call before the task row is written, so the refusal files nothing.
func TestNewTaskAttachmentRefusalFilesNothing(t *testing.T) {
	s := schemaStore(t, "newtask")
	ctx := context.Background()
	if _, err := s.AddTaskProject(ctx, "newtask-project", "tester"); err != nil {
		t.Fatal(err)
	}
	question := "newtask attachment refusal question"
	in := decisionInput()
	in.Question = question
	in.Options.Options[0].Attachments = []DecisionOptionAttachment{{SHA256: strings.Repeat("a", 64)}}
	_, err := s.RecordDecisionRequestNewTask(ctx, "tester", NewTaskDecisionInput{Project: "newtask-project", Lane: "mac-lane", Request: in})
	if !errors.Is(err, ErrDecisionAttachmentNotFound) {
		t.Fatalf("err=%v, want ErrDecisionAttachmentNotFound", err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE title=$1`, question).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("refused new-task call left %d task row(s)", n)
	}
}
