package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// dotsAnswer is a dots-channel answer: the real ChatGPT principal reaches the
// server through Access, so Verified is set, and only the operator's own words
// may close an operator_direct request (operator decision 399, hk 5132).
func dotsAnswer(request DecisionRequest) ResolveDecisionInput {
	in := answerA(request, DecisionChannelDots, "operator:operator@example.com")
	in.Verified = true
	return in
}

func dotsClaim(request DecisionRequest, quote string) ResolveDecisionInput {
	in := dotsAnswer(request)
	in.Provenance, in.OperatorQuote, in.DecisionRef = DecisionProvenanceOperatorDecided, quote, request.ID
	return in
}

// attemptQuote reads the quote a stored attempt kept.
func attemptQuote(t *testing.T, s *Store, id int64) string {
	t.Helper()
	var quote string
	if err := s.pool.QueryRow(context.Background(), `SELECT COALESCE(operator_quote,'') FROM decision_answer_attempts WHERE id=$1`, id).Scan(&quote); err != nil {
		t.Fatal(err)
	}
	return quote
}

func openRequestOf(t *testing.T, s *Store, taskID int64) DecisionRequest {
	t.Helper()
	got, found, err := s.GetTask(context.Background(), taskID)
	if err != nil || !found {
		t.Fatalf("task %d found=%v err=%v", taskID, found, err)
	}
	if got.Refs.DecisionRequest == nil {
		t.Fatalf("task %d has no decision request", taskID)
	}
	return *got.Refs.DecisionRequest
}

// Invariant: a dots answer that names operator_decided, carries the operator's
// verbatim words and binds them to the exact request id closes an
// operator_direct request. The record keeps channel dots, the words and
// operator_decided, and the asking lane gets the answer notice.
func TestResolveDecisionDotsQuoteClosesOperatorOnly(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	task := assistantTask(t, s, "in_progress")
	request := humanOnlyRequest(t, s, task.ID)
	const words = "A로 진행해, 이건 내 결정이야"

	out, err := s.ResolveDecision(ctx, dotsClaim(request, words))
	if err != nil {
		t.Fatal(err)
	}
	if out.Result != DecisionResultAnswered || out.ResolutionID == 0 || out.AttemptID == 0 {
		t.Fatalf("out=%+v", out)
	}
	prov, channel, decided := resolutionProvenance(t, pool, out.ResolutionID)
	if prov != DecisionProvenanceOperatorRelay || channel != DecisionChannelDots || decided != DecisionDecidedByOperator {
		t.Fatalf("resolution prov=%q channel=%q decided_by=%q", prov, channel, decided)
	}
	res := out.Request.Resolution
	if res == nil || res.Provenance != DecisionProvenanceOperatorDecided || res.OperatorQuote != words || res.DecisionRef != request.ID {
		t.Fatalf("resolution=%+v", res)
	}
	if got := openRequestOf(t, s, task.ID).Status; got != DecisionRequestAnswered {
		t.Fatalf("request status=%q", got)
	}
	attempts := attemptsOf(t, pool, task.ID)
	if len(attempts) != 1 || attempts[0].Channel != DecisionChannelDots || attempts[0].Provenance != DecisionProvenanceOperatorRelay || attempts[0].Result != DecisionResultAnswered {
		t.Fatalf("attempts=%+v", attempts)
	}
	if got := attemptQuote(t, s, out.AttemptID); got != words {
		t.Fatalf("attempt quote=%q", got)
	}
	notices := requestNotices(t, s, request.ID)
	if len(notices) != 1 || notices[0].TargetLane != task.Lane || !strings.Contains(notices[0].Text, " via dots") {
		t.Fatalf("notices=%+v", notices)
	}
}

// Invariant: a dots answer with the operator's words closes a request that
// allows a relayed answer too, and keeps the same relayed record.
func TestResolveDecisionDotsQuoteClosesRelayAllowed(t *testing.T) {
	s, pool := searchTestStore(t)
	task := assistantTask(t, s, "in_progress")
	request := assistantRecord(t, s, task.ID)

	out, err := s.ResolveDecision(context.Background(), dotsClaim(request, "B로 가자"))
	if err != nil || out.Result != DecisionResultAnswered {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if prov, channel, decided := resolutionProvenance(t, pool, out.ResolutionID); prov != DecisionProvenanceOperatorRelay || channel != DecisionChannelDots || decided != DecisionDecidedByOperator {
		t.Fatalf("prov=%q channel=%q decided_by=%q", prov, channel, decided)
	}
}

// Invariant: a dots answer without the operator's words — no claim at all, an
// explicit judgement, or an operator_decided claim with an invisible quote —
// is refused by the operator_only gate, left open, and kept as a refused
// attempt. Verified does not rescue it: dots is not the verified path.
func TestResolveDecisionDotsWithoutWordsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   func(DecisionRequest) ResolveDecisionInput
	}{
		{"no_claim", func(r DecisionRequest) ResolveDecisionInput { return dotsAnswer(r) }},
		{"judgement", func(r DecisionRequest) ResolveDecisionInput {
			in := dotsAnswer(r)
			in.Provenance = DecisionProvenanceAssistantJudgement
			return in
		}},
		{"blank_quote", func(r DecisionRequest) ResolveDecisionInput { return dotsClaim(r, "  \t\n ") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, pool := searchTestStore(t)
			ctx := context.Background()
			task := assistantTask(t, s, "in_progress")
			request := humanOnlyRequest(t, s, task.ID)

			out, err := s.ResolveDecision(ctx, tc.in(request))
			if !errors.Is(err, ErrOperatorOnlyDecision) || out.AttemptID == 0 {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			if got := openRequestOf(t, s, task.ID); got.Status != DecisionRequestOpen || got.Resolution != nil {
				t.Fatalf("refusal changed the request: %+v", got)
			}
			attempts := attemptsOf(t, pool, task.ID)
			if len(attempts) != 1 || attempts[0].Channel != DecisionChannelDots || attempts[0].Result != decisionAttemptRefused {
				t.Fatalf("attempts=%+v", attempts)
			}
			if resolutionsOf(t, pool, task.ID) != 0 {
				t.Fatal("a refused dots answer wrote a resolution")
			}
			if n := len(requestNotices(t, s, request.ID)); n != 0 {
				t.Fatalf("a refused dots answer left %d notices", n)
			}
		})
	}
}

// Invariant: an operator_decided quote for another request, or for an earlier
// revision of this one, answers nothing. The refusal is kept as an attempt.
func TestResolveDecisionDotsRefMismatchRefused(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	task := assistantTask(t, s, "in_progress")
	first, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", decisionInput())
	if err != nil {
		t.Fatal(err)
	}
	replaced := decisionInput()
	replaced.Question = "다시 묻는다: allowlist 위치?"
	replaced.Supersedes = first.Request.ID
	second, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", replaced)
	if err != nil {
		t.Fatal(err)
	}
	other := assistantTask(t, s, "in_progress")
	otherRequest := assistantRecord(t, s, other.ID)

	for _, tc := range []struct {
		name string
		ref  string
	}{
		{"another_request", otherRequest.ID},
		{"old_revision", first.Request.ID},
		{"missing", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(attemptsOf(t, pool, task.ID))
			in := dotsClaim(second.Request, "이전 답")
			in.DecisionRef = tc.ref
			out, err := s.ResolveDecision(ctx, in)
			if !errors.Is(err, ErrProvenanceRefMismatch) || out.AttemptID == 0 {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			if got := openRequestOf(t, s, task.ID); got.ID != second.Request.ID || got.Status != DecisionRequestOpen {
				t.Fatalf("request after a ref mismatch: %+v", got)
			}
			attempts := attemptsOf(t, pool, task.ID)
			if len(attempts) != before+1 || attempts[len(attempts)-1].Result != decisionAttemptRefused {
				t.Fatalf("attempts=%+v", attempts)
			}
			if resolutionsOf(t, pool, task.ID) != 0 {
				t.Fatal("a ref mismatch wrote a resolution")
			}
		})
	}
}

// Invariant: a refused dots answer is replayed from its attempt with the
// refusal it first got, and the retry writes no second row.
func TestResolveDecisionDotsRefusalReplay(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		in   func(DecisionRequest) ResolveDecisionInput
		want error
	}{
		{"operator_only", func(r DecisionRequest) ResolveDecisionInput { return dotsAnswer(r) }, ErrOperatorOnlyDecision},
		{"ref_mismatch", func(r DecisionRequest) ResolveDecisionInput {
			in := dotsClaim(r, "다른 요청의 답")
			in.DecisionRef = "dr-999999-1"
			return in
		}, ErrProvenanceRefMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := assistantTask(t, s, "in_progress")
			request := humanOnlyRequest(t, s, task.ID)
			in := tc.in(request)
			in.EventKey = "dots-retry-" + tc.name
			first, err := s.ResolveDecision(ctx, in)
			if !errors.Is(err, tc.want) || first.AttemptID == 0 {
				t.Fatalf("first out=%+v err=%v", first, err)
			}
			again, err := s.ResolveDecision(ctx, in)
			if !errors.Is(err, tc.want) || again.AttemptID != first.AttemptID || again.Result != "" {
				t.Fatalf("replay out=%+v err=%v", again, err)
			}
			if attempts := attemptsOf(t, pool, task.ID); len(attempts) != 1 {
				t.Fatalf("a replay wrote another attempt: %+v", attempts)
			}
		})
	}
}
