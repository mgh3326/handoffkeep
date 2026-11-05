package store

import (
	"context"
	"errors"
	"testing"
)

// TestDecisionDenyMigration pins version 46: a denied row names the attempt
// it denies, only a denied row does, and one attempt is denied once.
func TestDecisionDenyMigration(t *testing.T) {
	s := schemaStore(t, "decision_deny_mig")
	ctx := context.Background()
	var version int
	if err := s.pool.QueryRow(ctx, `SELECT max(version) FROM schema_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("max version=%d err=%v want %d", version, err, SchemaVersion)
	}
	task := decisionTask(t, s, "in_progress")
	insert := `INSERT INTO decision_answer_attempts(channel,event_key,payload_hash,task_id,revision_request_id,actor,result,received_at,denies_attempt_id) VALUES('console',$2,'h',$1,'dr-1-1','a',$3,now(),$4) RETURNING id`
	var pending int64
	if err := s.pool.QueryRow(ctx, insert, task.ID, "k1", "pending_confirmation", nil).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := s.pool.QueryRow(ctx, insert, task.ID, "k2", "denied", nil).Scan(&id); err == nil {
		t.Fatal("a denied row without denies_attempt_id was accepted")
	}
	if err := s.pool.QueryRow(ctx, insert, task.ID, "k3", "answered", pending).Scan(&id); err == nil {
		t.Fatal("denies_attempt_id on a row that is not denied was accepted")
	}
	if err := s.pool.QueryRow(ctx, insert, task.ID, "k4", "denied", pending).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, insert, task.ID, "k5", "denied", pending).Scan(&id); err == nil {
		t.Fatal("a second deny of one attempt was accepted")
	}
}

// TestResolveDecisionDeny walks the operator's deny of a relayed answer
// (hk 3163): the request stays open, a verified answer still closes it, and
// each refusal has its own sentinel.
func TestResolveDecisionDeny(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	deny := func(request DecisionRequest, attemptID int64, key string) ResolveDecisionInput {
		return ResolveDecisionInput{RequestID: request.ID, Kind: DecisionKindDeny, DeniesAttemptID: attemptID, Receipt: "relay was wrong", EventKey: key, Channel: DecisionChannelConsole, Actor: "operator:robin@example.com", Verified: true}
	}
	relay := func(t *testing.T, request DecisionRequest, key string) ResolveDecisionOutcome {
		t.Helper()
		in := answerA(request, DecisionChannelCLI, "session-a")
		in.EventKey = key
		out, err := s.ResolveDecision(ctx, in)
		if err != nil || out.Result != DecisionResultPendingConfirmation {
			t.Fatalf("relay: %+v %v", out, err)
		}
		return out
	}
	deniedEvents := func(t *testing.T, taskID int64) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND kind='decision' AND note LIKE 'decision-denied %'`, taskID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("deny_then_answer", func(t *testing.T) {
		task := decisionTask(t, s, "in_progress")
		request := humanOnlyRequest(t, s, task.ID)
		pending := relay(t, request, "relay-1")

		out, err := s.ResolveDecision(ctx, deny(request, pending.AttemptID, "deny-1"))
		if err != nil || out.Result != DecisionResultDenied || out.Request.Status != DecisionRequestOpen || out.ResolutionID != 0 {
			t.Fatalf("deny: %+v %v", out, err)
		}
		var denies int64
		var receipt string
		if err := pool.QueryRow(ctx, `SELECT denies_attempt_id,receipt FROM decision_answer_attempts WHERE id=$1`, out.AttemptID).Scan(&denies, &receipt); err != nil || denies != pending.AttemptID || receipt != "relay was wrong" {
			t.Fatalf("denied row denies=%d receipt=%q err=%v", denies, receipt, err)
		}
		live, _, _ := s.GetTask(ctx, task.ID)
		if live.Refs.DecisionRequest.Status != DecisionRequestOpen {
			t.Fatalf("deny closed the request: %+v", live.Refs.DecisionRequest)
		}
		if n := resolutionsOf(t, pool, task.ID); n != 0 {
			t.Fatalf("resolutions=%d after a deny", n)
		}
		if n := deniedEvents(t, task.ID); n != 1 {
			t.Fatalf("decision-denied events=%d", n)
		}

		replay, err := s.ResolveDecision(ctx, deny(request, pending.AttemptID, "deny-1"))
		if err != nil || !replay.Duplicate || replay.Result != DecisionResultDenied || replay.AttemptID != out.AttemptID {
			t.Fatalf("replay: %+v %v", replay, err)
		}
		if _, err = s.ResolveDecision(ctx, deny(request, pending.AttemptID, "deny-2")); !errors.Is(err, ErrDecisionAttemptDenied) {
			t.Fatalf("second deny err=%v", err)
		}
		if n := len(attemptsOf(t, pool, task.ID)); n != 2 {
			t.Fatalf("attempts=%d, want relay and one deny", n)
		}

		answer := answerA(request, DecisionChannelConsole, "operator:robin@example.com")
		answer.Verified = true
		answer.EventKey = "answer-1"
		if got, err := s.ResolveDecision(ctx, answer); err != nil || got.Result != DecisionResultAnswered {
			t.Fatalf("verified answer after a deny: %+v %v", got, err)
		}
		if _, err = s.ResolveDecision(ctx, deny(request, pending.AttemptID, "deny-3")); !errors.Is(err, ErrDecisionRequestResolved) {
			t.Fatalf("deny on a closed request err=%v", err)
		}
	})

	t.Run("not_pending", func(t *testing.T) {
		task := decisionTask(t, s, "in_progress")
		request := assistantRecord(t, s, task.ID)
		answered, err := s.ResolveDecision(ctx, answerA(request, DecisionChannelCLI, "director-1"))
		if err != nil {
			t.Fatal(err)
		}
		next := decisionInput()
		next.Question = "다음 질문"
		next.HumanOnly = true
		fresh := assistantRecordWith(t, s, task.ID, next)
		if _, err = s.ResolveDecision(ctx, deny(fresh, answered.AttemptID, "np-1")); !errors.Is(err, ErrDecisionAttemptNotPending) {
			t.Fatalf("deny of an answered attempt err=%v", err)
		}
	})

	t.Run("older_revision", func(t *testing.T) {
		task := decisionTask(t, s, "in_progress")
		first := humanOnlyRequest(t, s, task.ID)
		pending := relay(t, first, "old-relay")
		in := decisionInput()
		in.HumanOnly = true
		in.Question = "고쳐 묻는다"
		in.Supersedes = first.ID
		second := assistantRecordWith(t, s, task.ID, in)
		if _, err := s.ResolveDecision(ctx, deny(second, pending.AttemptID, "old-1")); !errors.Is(err, ErrDecisionAttemptNotPending) {
			t.Fatalf("deny of an older revision's attempt err=%v", err)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		task := decisionTask(t, s, "in_progress")
		request := humanOnlyRequest(t, s, task.ID)
		pending := relay(t, request, "ref-relay")
		unverified := deny(request, pending.AttemptID, "ref-1")
		unverified.Verified = false
		unverified.Channel = DecisionChannelCLI
		if _, err := s.ResolveDecision(ctx, unverified); !errors.Is(err, ErrInvalidDecisionRequest) {
			t.Fatalf("unverified deny err=%v", err)
		}
		if _, err := s.ResolveDecision(ctx, deny(request, pending.AttemptID+1_000_000, "ref-2")); !errors.Is(err, ErrDecisionAttemptNotFound) {
			t.Fatalf("unknown attempt err=%v", err)
		}
		other := decisionTask(t, s, "in_progress")
		otherRequest := humanOnlyRequest(t, s, other.ID)
		if _, err := s.ResolveDecision(ctx, deny(otherRequest, pending.AttemptID, "ref-3")); !errors.Is(err, ErrDecisionAttemptNotPending) {
			t.Fatalf("deny of another task's attempt err=%v", err)
		}
		if n := len(attemptsOf(t, pool, task.ID)); n != 1 {
			t.Fatalf("refusals wrote attempts: %d", n)
		}
	})
}
