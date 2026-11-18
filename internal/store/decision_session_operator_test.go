package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Invariant: a session answer that names operator_decided, carries the
// operator's verbatim words and binds them to the exact request id closes an
// operator_direct request with no second confirmation (operator decision 400,
// hk 5136). The record keeps channel session, the words and operator_decided,
// and the asking lane gets the answer notice.
func TestResolveDecisionSessionQuoteClosesOperatorOnly(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(*testing.T, *Store, int64) DecisionRequest
	}{
		{"human_only", humanOnlyRequest},
		{"operator_direct_policy", func(t *testing.T, s *Store, id int64) DecisionRequest {
			return policyRequest(t, s, id, DecisionPolicyOperatorDirect, false)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, pool := searchTestStore(t)
			task := assistantTask(t, s, "in_progress")
			request := tc.build(t, s, task.ID)
			const words = "A로 진행해, 이건 내 결정이야"

			out, err := s.ResolveDecision(context.Background(), sessionAnswer(request, words))
			if err != nil {
				t.Fatal(err)
			}
			if out.Result != DecisionResultAnswered || out.ResolutionID == 0 || out.AttemptID == 0 {
				t.Fatalf("out=%+v", out)
			}
			prov, channel, decided := resolutionProvenance(t, pool, out.ResolutionID)
			if prov != DecisionProvenanceOperatorRelay || channel != DecisionChannelSession || decided != DecisionDecidedByOperator {
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
			if len(attempts) != 1 || attempts[0].Channel != DecisionChannelSession || attempts[0].Provenance != DecisionProvenanceOperatorRelay || attempts[0].Result != DecisionResultAnswered {
				t.Fatalf("attempts=%+v", attempts)
			}
			if got := attemptQuote(t, s, out.AttemptID); got != words {
				t.Fatalf("attempt quote=%q", got)
			}
			notices := requestNotices(t, s, request.ID)
			if len(notices) != 1 || notices[0].TargetLane != task.Lane || !strings.Contains(notices[0].Text, " via session") {
				t.Fatalf("notices=%+v", notices)
			}
		})
	}
}

// Invariant: a session answer without the operator's words — an explicit
// judgement, or an operator_decided claim with an invisible quote — is its own
// judgement: refused, left open, and kept as a refused attempt. The
// session channel is never verified, so nothing else closes it.
func TestResolveDecisionSessionWithoutWordsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   func(DecisionRequest) ResolveDecisionInput
	}{
		{"judgement", func(r DecisionRequest) ResolveDecisionInput {
			in := sessionAnswer(r, "")
			in.Provenance = DecisionProvenanceAssistantJudgement
			return in
		}},
		{"blank_quote", func(r DecisionRequest) ResolveDecisionInput { return sessionAnswer(r, "  \t\n ") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, pool := searchTestStore(t)
			task := assistantTask(t, s, "in_progress")
			request := humanOnlyRequest(t, s, task.ID)

			out, err := s.ResolveDecision(context.Background(), tc.in(request))
			if !errors.Is(err, ErrDecisionRequestHumanOnly) || out.AttemptID == 0 {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			if got := openRequestOf(t, s, task.ID); got.Status != DecisionRequestOpen || got.Resolution != nil {
				t.Fatalf("refusal changed the request: %+v", got)
			}
			attempts := attemptsOf(t, pool, task.ID)
			if len(attempts) != 1 || attempts[0].Channel != DecisionChannelSession || attempts[0].Result != decisionAttemptRefused {
				t.Fatalf("attempts=%+v", attempts)
			}
			if resolutionsOf(t, pool, task.ID) != 0 {
				t.Fatal("a refused session answer wrote a resolution")
			}
			if n := len(requestNotices(t, s, request.ID)); n != 0 {
				t.Fatalf("a refused session answer left %d notices", n)
			}
		})
	}
}

// Invariant: a session quote bound to another request, or to an earlier
// revision of this one, answers nothing. The refusal is kept as an attempt.
func TestResolveDecisionSessionRefMismatchRefusedAndKept(t *testing.T) {
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
			in := sessionAnswer(second.Request, "이전 답")
			in.DecisionRef = tc.ref
			out, err := s.ResolveDecision(ctx, in)
			if !errors.Is(err, ErrProvenanceRefMismatch) || out.AttemptID == 0 {
				t.Fatalf("out=%+v err=%v", out, err)
			}
			if got := openRequestOf(t, s, task.ID); got.ID != second.Request.ID || got.Status != DecisionRequestOpen {
				t.Fatalf("request after a ref mismatch: %+v", got)
			}
			attempts := attemptsOf(t, pool, task.ID)
			if len(attempts) != before+1 || attempts[len(attempts)-1].Result != decisionAttemptRefused || attempts[len(attempts)-1].Channel != DecisionChannelSession {
				t.Fatalf("attempts=%+v", attempts)
			}
			if resolutionsOf(t, pool, task.ID) != 0 {
				t.Fatal("a ref mismatch wrote a resolution")
			}
		})
	}
}

// Invariant: a refused session answer is replayed from its attempt with the
// refusal it first got, and the retry writes no second row.
func TestResolveDecisionSessionRefusalReplay(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		in   func(DecisionRequest) ResolveDecisionInput
		want error
	}{
		{"human_only", func(r DecisionRequest) ResolveDecisionInput { return sessionAnswer(r, "") }, ErrDecisionRequestHumanOnly},
		{"ref_mismatch", func(r DecisionRequest) ResolveDecisionInput {
			in := sessionAnswer(r, "다른 요청의 답")
			in.DecisionRef = "dr-999999-1"
			return in
		}, ErrProvenanceRefMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := assistantTask(t, s, "in_progress")
			request := humanOnlyRequest(t, s, task.ID)
			in := tc.in(request)
			in.EventKey = "session-retry-" + tc.name
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

// Invariant: the session close is idempotent. The same keyed answer replays
// the first result (one attempt, one resolution, one notice); and another
// session answer to the now-answered request is a conflict, not a second
// close.
func TestResolveDecisionSessionOperatorQuoteReplay(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	task := assistantTask(t, s, "in_progress")
	request := humanOnlyRequest(t, s, task.ID)
	in := sessionAnswer(request, "A로 가자")
	in.EventKey = "session-close-1"
	first, err := s.ResolveDecision(ctx, in)
	if err != nil || first.Result != DecisionResultAnswered {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	again, err := s.ResolveDecision(ctx, in)
	if err != nil || !again.Duplicate || again.AttemptID != first.AttemptID || again.ResolutionID != first.ResolutionID {
		t.Fatalf("replay=%+v err=%v", again, err)
	}
	other := sessionAnswer(request, "B로 가자")
	other.Option = "B"
	if out, err := s.ResolveDecision(ctx, other); err != nil || out.Result != DecisionResultConflict || !errors.Is(out.Refusal, ErrDecisionRequestResolved) {
		t.Fatalf("second answer out=%+v err=%v", out, err)
	}
	if n := resolutionsOf(t, pool, task.ID); n != 1 {
		t.Fatalf("resolutions=%d", n)
	}
	if n := len(requestNotices(t, s, request.ID)); n != 1 {
		t.Fatalf("notices=%d", n)
	}
}

// Invariant: provenance never widens a disposition item. A disposition's
// effective policy is operator_direct and the console is its only answer
// route, so a session quote that matches the request is still refused.
func TestResolveDecisionSessionQuoteRefusedOnDisposition(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	task := assistantTask(t, s, "in_progress")
	request := humanOnlyRequest(t, s, task.ID)
	if _, err := pool.Exec(ctx, `UPDATE tasks SET refs=refs||'{"disposition":{"schema":"disposition/v1","facts":{"facts_source":"gh-pr-view","facts_as_of":"2026-10-09T00:00:00Z","install":{"state":"unknown"},"residual_n":0},"recommended":"E"}}'::jsonb WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveDecision(ctx, sessionAnswer(request, "operator said so")); !errors.Is(err, ErrDispositionOperatorOnly) {
		t.Fatalf("err=%v", err)
	}
	if resolutionsOf(t, pool, task.ID) != 0 {
		t.Fatal("a session quote closed a disposition item")
	}
}

// Invariant: the verified path and the other channels are unchanged. A
// verified console/cli answer still closes operator_direct as operator_direct;
// an unverified cli relay still waits as pending_confirmation.
func TestResolveDecisionSessionChangeLeavesOtherChannels(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	task := assistantTask(t, s, "in_progress")
	request := humanOnlyRequest(t, s, task.ID)

	pending, err := s.ResolveDecision(ctx, answerA(request, DecisionChannelCLI, "worker-1"))
	if err != nil || pending.Result != DecisionResultPendingConfirmation {
		t.Fatalf("cli relay out=%+v err=%v", pending, err)
	}
	verified := answerA(request, DecisionChannelCLI, "operator")
	verified.Verified = true
	out, err := s.ResolveDecision(ctx, verified)
	if err != nil || out.Result != DecisionResultAnswered {
		t.Fatalf("verified out=%+v err=%v", out, err)
	}
	if prov, _, _ := resolutionProvenance(t, pool, out.ResolutionID); prov != DecisionProvenanceOperatorDirect {
		t.Fatalf("verified provenance=%q", prov)
	}
}
