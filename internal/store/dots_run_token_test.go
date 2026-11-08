package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// startDotsRun starts a request with a fresh revision and returns the START
// result together with the revision it sent, so a test can replay that START.
func startDotsRun(t *testing.T, s *Store, taskID int64, key string) (DotsReplyResult, time.Time) {
	t.Helper()
	current, found, err := s.GetTask(t.Context(), taskID)
	if err != nil || !found {
		t.Fatalf("task found=%v err=%v", found, err)
	}
	revision := current.UpdatedAt
	got, err := s.ReplyDots(t.Context(), "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: taskID, Kind: "START", Body: "Starting", IdempotencyKey: key, ExpectedUpdatedAt: &revision})
	if err != nil || got.RunToken == "" {
		t.Fatalf("start=%+v err=%v", got, err)
	}
	return got, revision
}

func TestDotsRunTokenShape(t *testing.T) {
	a, err := newDotsRunToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newDotsRunToken()
	if err != nil {
		t.Fatal(err)
	}
	if !validDotsRunToken(a) || !validDotsRunToken(b) || a == b {
		t.Fatalf("tokens %q %q", a, b)
	}
	for _, bad := range []string{"", "not-a-token", strings.Repeat("A", 64), strings.Repeat("a", 63), strings.Repeat("a", 65)} {
		if validDotsRunToken(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
}

// A zero Store has no pool. These refusals must come before any state is
// read, so they hold without a database.
func TestDotsReplyRunTokenRefusedBeforeState(t *testing.T) {
	var s Store
	ctx := t.Context()
	in := DotsReplyInput{TaskID: 1, Kind: "PROGRESS", Body: "Found a lead", IdempotencyKey: "progress"}
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", in); !errors.Is(err, ErrDotsRunTokenRequired) {
		t.Fatalf("no token: %v", err)
	}
	in.RunToken = "not-a-token"
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", in); !errors.Is(err, ErrDotsRunTokenMismatch) {
		t.Fatalf("malformed token: %v", err)
	}
	takeover := DotsReplyInput{TaskID: 1, Kind: "PROGRESS", Body: "Found a lead", IdempotencyKey: "progress", RunToken: strings.Repeat("a", 64), Takeover: true}
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", takeover); err == nil || err.Error() != "invalid dots reply" {
		t.Fatalf("takeover on a non-START reply: %v", err)
	}
}

func TestDotsRunTokenGatesLaterReplies(t *testing.T) {
	s := schemaStore(t, "dots_run_gate")
	ctx := t.Context()
	task, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate"})
	if err != nil {
		t.Fatal(err)
	}
	if issued, err := s.DotsRunIssuedAt(ctx, task.ID); err != nil || issued != nil {
		t.Fatalf("issued before start=%v err=%v", issued, err)
	}
	start, _ := startDotsRun(t, s, task.ID, "start")
	if issued, err := s.DotsRunIssuedAt(ctx, task.ID); err != nil || issued == nil {
		t.Fatalf("issued after start=%v err=%v", issued, err)
	}
	progress := DotsReplyInput{TaskID: task.ID, Kind: "PROGRESS", Body: "Found a lead", IdempotencyKey: "progress"}
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", progress); !errors.Is(err, ErrDotsRunTokenRequired) {
		t.Fatalf("missing token: %v", err)
	}
	progress.RunToken = strings.Repeat("0", 64)
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", progress); !errors.Is(err, ErrDotsRunTokenMismatch) {
		t.Fatalf("wrong token: %v", err)
	}
	// A refused reply leaves no comment behind: only START's comment exists.
	replies, err := s.ListDotsReplies(ctx, task.ID)
	if err != nil || len(replies) != 1 || replies[0].Kind != "START" {
		t.Fatalf("replies after refusals=%+v err=%v", replies, err)
	}
	progress.RunToken = start.RunToken
	first, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", progress)
	if err != nil || first.Duplicate || first.RunToken != "" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	again, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", progress)
	if err != nil || !again.Duplicate || again.DocumentKey != first.DocumentKey {
		t.Fatalf("replay=%+v err=%v", again, err)
	}
	replies, err = s.ListDotsReplies(ctx, task.ID)
	if err != nil || len(replies) != 2 || replies[1].Kind != "PROGRESS" {
		t.Fatalf("replies=%+v err=%v", replies, err)
	}
}

func TestDotsSecondStartNeedsTakeover(t *testing.T) {
	s := schemaStore(t, "dots_takeover")
	ctx := t.Context()
	task, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate"})
	if err != nil {
		t.Fatal(err)
	}
	first, firstRevision := startDotsRun(t, s, task.ID, "start-1")
	current, _, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A plain START on a started request is refused, not a silent second run.
	_, err = s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "START", Body: "Starting again", IdempotencyKey: "start-2", ExpectedUpdatedAt: &current.UpdatedAt})
	if !errors.Is(err, ErrTaskConflict) || !strings.Contains(err.Error(), "already claimed") {
		t.Fatalf("second start: %v", err)
	}
	second, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "START", Body: "Taking over", IdempotencyKey: "takeover-1", ExpectedUpdatedAt: &current.UpdatedAt, Takeover: true})
	if err != nil || second.Duplicate || second.RunToken == "" || second.RunToken == first.RunToken {
		t.Fatalf("takeover=%+v err=%v", second, err)
	}
	// The superseded run is refused; the new run is accepted.
	_, err = s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "PROGRESS", Body: "Old run", IdempotencyKey: "old-progress", RunToken: first.RunToken})
	if !errors.Is(err, ErrDotsRunTokenMismatch) {
		t.Fatalf("superseded token: %v", err)
	}
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "PROGRESS", Body: "New run", IdempotencyKey: "new-progress", RunToken: second.RunToken}); err != nil {
		t.Fatalf("new token: %v", err)
	}
	// Replaying the first START returns its own first result, token included.
	replay, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "START", Body: "Starting", IdempotencyKey: "start-1", ExpectedUpdatedAt: &firstRevision})
	if err != nil || !replay.Duplicate || replay.RunToken != first.RunToken {
		t.Fatalf("start replay=%+v err=%v", replay, err)
	}
	// The handover is in the request's history, and no event carries a token.
	var handovers int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND source='dots-run-token' AND field='run_token'`, task.ID).Scan(&handovers); err != nil || handovers != 1 {
		t.Fatalf("handovers=%d err=%v", handovers, err)
	}
	var leaked int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND (note LIKE '%'||$2||'%' OR from_value LIKE '%'||$2||'%' OR to_value LIKE '%'||$2||'%')`, task.ID, first.RunToken).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("token in history: leaked=%d err=%v", leaked, err)
	}
}

func TestDotsTakeoverRefusedWhenNotStarted(t *testing.T) {
	s := schemaStore(t, "dots_takeover_new")
	ctx := t.Context()
	task, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "START", Body: "Taking over", IdempotencyKey: "takeover", ExpectedUpdatedAt: &task.UpdatedAt, Takeover: true})
	if !errors.Is(err, ErrTaskConflict) || !strings.Contains(err.Error(), "not started") {
		t.Fatalf("takeover of an unstarted request: %v", err)
	}
}
