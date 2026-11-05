package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func policyRequest(t *testing.T, s *Store, taskID int64, policy string, humanOnly bool) DecisionRequest {
	t.Helper()
	in := decisionInput()
	in.AnswerPolicy = policy
	in.HumanOnly = humanOnly
	return assistantRecordWith(t, s, taskID, in)
}

// attemptProposal reads the proposed answer an attempt kept.
func attemptProposal(t *testing.T, s *Store, id int64) (option, text, quote *string) {
	t.Helper()
	if err := s.pool.QueryRow(context.Background(), `SELECT option_id,text,operator_quote FROM decision_answer_attempts WHERE id=$1`, id).Scan(&option, &text, &quote); err != nil {
		t.Fatal(err)
	}
	return option, text, quote
}

func TestAnswerPolicyValidation(t *testing.T) {
	if got := (DecisionRequest{}).EffectivePolicy(false); got != DecisionPolicyRelayAllowed {
		t.Fatalf("empty policy=%q", got)
	}
	for _, c := range []struct {
		name string
		r    DecisionRequest
		disp bool
	}{
		{"human_only", DecisionRequest{HumanOnly: true}, false},
		{"disposition", DecisionRequest{AnswerPolicy: DecisionPolicyRelayAllowed}, true},
		{"explicit", DecisionRequest{AnswerPolicy: DecisionPolicyOperatorDirect}, false},
	} {
		if got := c.r.EffectivePolicy(c.disp); got != DecisionPolicyOperatorDirect {
			t.Fatalf("%s: policy=%q", c.name, got)
		}
	}
	unknown := decisionInput()
	unknown.AnswerPolicy = "anyone"
	if err := CheckDecisionRequestInput(unknown); !errors.Is(err, ErrInvalidDecisionRequest) || !strings.Contains(err.Error(), "answer_policy") {
		t.Fatalf("unknown policy err=%v", err)
	}
	mixed := decisionInput()
	mixed.HumanOnly, mixed.AnswerPolicy = true, DecisionPolicyRelayAllowed
	if err := CheckDecisionRequestInput(mixed); !errors.Is(err, ErrInvalidDecisionRequest) || !strings.Contains(err.Error(), "human_only") {
		t.Fatalf("human_only relay_allowed err=%v", err)
	}
	for _, policy := range []string{"", DecisionPolicyOperatorDirect, DecisionPolicyRelayAllowed} {
		in := decisionInput()
		in.AnswerPolicy = policy
		if err := CheckDecisionRequestInput(in); err != nil {
			t.Fatalf("policy %q err=%v", policy, err)
		}
	}

	s, _ := searchTestStore(t)
	ctx := context.Background()
	task := assistantTask(t, s, "in_progress")
	first := policyRequest(t, s, task.ID, "", false)
	same, err := s.RecordDecisionRequest(ctx, task.ID, "panewire-test", func() DecisionRequestInput {
		in := decisionInput()
		in.AnswerPolicy = DecisionPolicyRelayAllowed
		return in
	}())
	if err != nil || !same.Duplicate || same.Request.ID != first.ID {
		t.Fatalf("empty and relay_allowed are one policy: %+v err=%v", same, err)
	}
	direct := decisionInput()
	direct.AnswerPolicy = DecisionPolicyOperatorDirect
	if _, err := s.RecordDecisionRequest(ctx, task.ID, "panewire-test", direct); !errors.Is(err, ErrDecisionRequestOpen) {
		t.Fatalf("another policy is not a duplicate: err=%v", err)
	}
}

func TestAnswerPolicyEnforceHumanOnlyForcesDirect(t *testing.T) {
	s, _ := searchTestStore(t)
	task := assistantTask(t, s, "in_progress")
	request := policyRequest(t, s, task.ID, "", true)
	out, err := s.ResolveDecision(context.Background(), answerA(request, DecisionChannelCLI, "worker-1"))
	if err != nil || out.Result != DecisionResultPendingConfirmation {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestAnswerPolicyEnforceDirectRelayPends(t *testing.T) {
	s, _ := searchTestStore(t)
	task := assistantTask(t, s, "in_progress")
	request := policyRequest(t, s, task.ID, DecisionPolicyOperatorDirect, false)
	in := answerA(request, DecisionChannelCLI, "worker-1")
	in.Text = "operator said A"
	out, err := s.ResolveDecision(context.Background(), in)
	if err != nil || out.Result != DecisionResultPendingConfirmation || out.Request.Status != DecisionRequestOpen {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	option, text, _ := attemptProposal(t, s, out.AttemptID)
	if option == nil || *option != "A" || text == nil || *text != "operator said A" {
		t.Fatalf("pending attempt lost its proposal: option=%v text=%v", option, text)
	}
	if resolutionsOf(t, s.pool, task.ID) != 0 {
		t.Fatal("a pending relay wrote a resolution")
	}
}

func TestAnswerPolicyEnforceRelayAllowedAnswers(t *testing.T) {
	s, _ := searchTestStore(t)
	task := assistantTask(t, s, "in_progress")
	request := policyRequest(t, s, task.ID, DecisionPolicyRelayAllowed, false)
	out, err := s.ResolveDecision(context.Background(), answerA(request, DecisionChannelCLI, "worker-1"))
	if err != nil || out.Result != DecisionResultAnswered {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if prov, _, _ := resolutionProvenance(t, s.pool, out.ResolutionID); prov != DecisionProvenanceOperatorRelay {
		t.Fatalf("provenance=%q", prov)
	}
}

func TestAnswerPolicyEnforceDirectRefusesJudgement(t *testing.T) {
	s, _ := searchTestStore(t)
	task := assistantTask(t, s, "in_progress")
	request := policyRequest(t, s, task.ID, DecisionPolicyOperatorDirect, false)
	out, err := s.ResolveDecision(context.Background(), answerA(request, DecisionChannelAssistant, "berry-mcp"))
	if !errors.Is(err, ErrDecisionRequestHumanOnly) || out.AttemptID == 0 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if got := attemptsOf(t, s.pool, task.ID); len(got) != 1 || got[0].Result != decisionAttemptRefused {
		t.Fatalf("attempts=%+v", got)
	}
}

func sessionAnswer(request DecisionRequest, quote string) ResolveDecisionInput {
	in := answerA(request, DecisionChannelSession, "operator-desk")
	if quote != "" {
		in.Provenance, in.OperatorQuote, in.DecisionRef = DecisionProvenanceOperatorDecided, quote, request.ID
	}
	return in
}

func TestResolveDecisionSessionQuoteIsRelay(t *testing.T) {
	s, _ := searchTestStore(t)
	task := assistantTask(t, s, "in_progress")
	request := assistantRecord(t, s, task.ID)
	out, err := s.ResolveDecision(context.Background(), sessionAnswer(request, "A로 가자"))
	if err != nil || out.Result != DecisionResultAnswered {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	prov, channel, decided := resolutionProvenance(t, s.pool, out.ResolutionID)
	if prov != DecisionProvenanceOperatorRelay || channel != DecisionChannelSession || decided != DecisionDecidedByOperator {
		t.Fatalf("prov=%q channel=%q decided_by=%q", prov, channel, decided)
	}
	if got := attemptsOf(t, s.pool, task.ID); len(got) != 1 || got[0].Channel != DecisionChannelSession {
		t.Fatalf("attempts=%+v", got)
	}
	if out.Request.Resolution.OperatorQuote != "A로 가자" || out.Request.Resolution.DecisionRef != request.ID {
		t.Fatalf("resolution=%+v", out.Request.Resolution)
	}
	if n := len(outboxRows(t, s)); n != 0 {
		t.Fatalf("a session answer wrote %d [via berry] outbox rows", n)
	}
}

func TestResolveDecisionSessionNoQuoteIsJudgement(t *testing.T) {
	s, _ := searchTestStore(t)
	task := assistantTask(t, s, "in_progress")
	request := assistantRecord(t, s, task.ID)
	out, err := s.ResolveDecision(context.Background(), sessionAnswer(request, ""))
	if err != nil || out.Result != DecisionResultAnswered {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if prov, channel, _ := resolutionProvenance(t, s.pool, out.ResolutionID); prov != DecisionProvenanceAssistantJudgement || channel != DecisionChannelSession {
		t.Fatalf("prov=%q channel=%q", prov, channel)
	}
}

func TestResolveDecisionSessionRefMismatch(t *testing.T) {
	s, _ := searchTestStore(t)
	task := assistantTask(t, s, "in_progress")
	request := assistantRecord(t, s, task.ID)
	in := sessionAnswer(request, "A로 가자")
	in.DecisionRef = "dr-999-1"
	if _, err := s.ResolveDecision(context.Background(), in); !errors.Is(err, ErrProvenanceRefMismatch) {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveDecisionSessionVerifiedRefused(t *testing.T) {
	s, _ := searchTestStore(t)
	task := assistantTask(t, s, "in_progress")
	request := assistantRecord(t, s, task.ID)
	in := sessionAnswer(request, "A로 가자")
	in.Verified = true
	if _, err := s.ResolveDecision(context.Background(), in); !errors.Is(err, ErrInvalidDecisionRequest) {
		t.Fatalf("err=%v", err)
	}
	if got := attemptsOf(t, s.pool, task.ID); len(got) != 0 {
		t.Fatalf("attempts=%+v", got)
	}
}

func TestResolveDecisionSessionReplay(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	task := assistantTask(t, s, "in_progress")
	request := assistantRecord(t, s, task.ID)
	in := sessionAnswer(request, "A로 가자")
	in.EventKey = "session-1"
	first, err := s.ResolveDecision(ctx, in)
	if err != nil || first.Result != DecisionResultAnswered {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	again, err := s.ResolveDecision(ctx, in)
	if err != nil || !again.Duplicate || again.AttemptID != first.AttemptID || again.ResolutionID != first.ResolutionID {
		t.Fatalf("replay=%+v err=%v", again, err)
	}
	if got := attemptsOf(t, s.pool, task.ID); len(got) != 1 {
		t.Fatalf("attempts=%+v", got)
	}
}

// TestResolveDecisionSessionPendingKeepsQuote: a quoted session answer on a
// direct-policy request waits, and its attempt keeps option and quote.
func TestResolveDecisionSessionPendingKeepsQuote(t *testing.T) {
	s, _ := searchTestStore(t)
	task := assistantTask(t, s, "in_progress")
	request := policyRequest(t, s, task.ID, DecisionPolicyOperatorDirect, false)
	out, err := s.ResolveDecision(context.Background(), sessionAnswer(request, "A로 가자"))
	if err != nil || out.Result != DecisionResultPendingConfirmation {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	option, _, quote := attemptProposal(t, s, out.AttemptID)
	if option == nil || *option != "A" || quote == nil || *quote != "A로 가자" {
		t.Fatalf("option=%v quote=%v", option, quote)
	}
}

// TestDecisionSessionMigration pins version 47 in a throwaway schema: the
// session channel is admitted and the proposal columns exist.
func TestDecisionSessionMigration(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	var version int
	if err := pool.QueryRow(ctx, `SELECT max(version) FROM schema_version`).Scan(&version); err != nil || version != SchemaVersion || SchemaVersion < 47 {
		t.Fatalf("max version=%d err=%v want %d", version, err, SchemaVersion)
	}
	task := decisionTask(t, s, "in_progress")
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO decision_answer_attempts(channel,event_key,payload_hash,task_id,revision_request_id,actor,result,received_at,option_id,text,operator_quote)
		VALUES('session','k1','h',$1,'dr-1-1','a','answered',now(),'A','t','q') RETURNING id`, task.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	option, text, quote := attemptProposal(t, s, id)
	if option == nil || *option != "A" || text == nil || *text != "t" || quote == nil || *quote != "q" {
		t.Fatalf("option=%v text=%v quote=%v", option, text, quote)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO decision_answer_attempts(channel,event_key,payload_hash,task_id,revision_request_id,actor,result,received_at) VALUES('mail','k2','h',$1,'dr-1-1','a','answered',now())`, task.ID); err == nil {
		t.Fatal("unknown channel accepted")
	}
}

// A quote kept on an attempt is checked even when the request is no longer
// open: a superseded answer must not store a secret-like quote in the
// append-only attempts table.
func TestResolveDecisionSessionStaleQuoteChecked(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	task := assistantTask(t, s, "in_progress")
	first := assistantRecord(t, s, task.ID)
	next := decisionInput()
	next.Question = "다시 묻는다"
	next.Supersedes = first.ID
	assistantRecordWith(t, s, task.ID, next)
	in := sessionAnswer(first, "AKIA"+"ABCDEFGHIJKLMNOP")
	if _, err := s.ResolveDecision(ctx, in); err == nil {
		t.Fatal("secret-like quote on a stale request accepted")
	}
	if got := attemptsOf(t, s.pool, task.ID); len(got) != 0 {
		t.Fatalf("attempts=%+v", got)
	}
}
