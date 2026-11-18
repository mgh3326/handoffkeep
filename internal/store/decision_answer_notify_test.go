package store

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// Invariant: every successful resolution, on every channel, writes exactly one
// answered notice to the requesting lane, named by its resolution; a replay
// and a refusal write none.
func TestDecisionAnswerNoticePerChannel(t *testing.T) {
	s, _ := searchTestStore(t)
	s.SetConsoleURL("https://console.example.test")
	ctx := context.Background()
	verified := func(r DecisionRequest, channel string) ResolveDecisionInput {
		in := answerA(r, channel, "operator")
		in.Responder, in.Verified = "operator", true
		return in
	}
	cases := []struct {
		channel string
		in      func(DecisionRequest) ResolveDecisionInput
	}{
		{DecisionChannelCLI, func(r DecisionRequest) ResolveDecisionInput { return verified(r, DecisionChannelCLI) }},
		{DecisionChannelConsole, func(r DecisionRequest) ResolveDecisionInput { return verified(r, DecisionChannelConsole) }},
		{DecisionChannelAssistant, func(r DecisionRequest) ResolveDecisionInput {
			return answerA(r, DecisionChannelAssistant, "berry-mcp")
		}},
		{DecisionChannelSession, func(r DecisionRequest) ResolveDecisionInput { return sessionAnswer(r, "A로 가자") }},
	}
	for _, c := range cases {
		task := assistantTask(t, s, "in_progress")
		request := assistantRecord(t, s, task.ID)
		out, err := s.ResolveDecision(ctx, c.in(request))
		if err != nil || out.Result != DecisionResultAnswered {
			t.Fatalf("%s: out=%+v err=%v", c.channel, out, err)
		}
		rows := requestNotices(t, s, request.ID)
		if len(rows) != 1 {
			t.Fatalf("%s: notices=%+v", c.channel, rows)
		}
		row := rows[0]
		wantText := "[hk] 결정 답변 " + request.ID + " task " + strconv.FormatInt(task.ID, 10) + ": "
		if row.EventID != decisionAnswerNoticeEventID(request.ID, out.ResolutionID) || row.Kind != OutboxKindDecisionAnswered || row.TargetLane != task.Lane {
			t.Fatalf("%s: row=%+v", c.channel, row)
		}
		if !strings.HasPrefix(row.Text, wantText) || !strings.Contains(row.Text, " via "+c.channel) || !strings.HasSuffix(row.Text, "| https://console.example.test/ui/tasks/"+strconv.FormatInt(task.ID, 10)) {
			t.Fatalf("%s: text=%q", c.channel, row.Text)
		}
	}
}

// Invariant: a replayed delivery writes no second notice, and a refused
// answer writes none at all.
func TestDecisionAnswerNoticeReplayAndRefusal(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()

	task := assistantTask(t, s, "in_progress")
	request := assistantRecord(t, s, task.ID)
	in := answerA(request, DecisionChannelCLI, "operator")
	in.Responder, in.Verified, in.EventKey = "operator", true, "notice-replay-1"
	if _, err := s.ResolveDecision(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveDecision(ctx, in); err != nil {
		t.Fatalf("replay err=%v", err)
	}
	if rows := requestNotices(t, s, request.ID); len(rows) != 1 {
		t.Fatalf("replay left %d notices", len(rows))
	}

	refusedTask := assistantTask(t, s, "in_progress")
	refused := assistantRecord(t, s, refusedTask.ID)
	bad := answerA(refused, DecisionChannelCLI, "operator")
	bad.Option, bad.Responder, bad.Verified = "Z", "operator", true
	if _, err := s.ResolveDecision(ctx, bad); err == nil {
		t.Fatal("unknown option was accepted")
	}
	if rows := requestNotices(t, s, refused.ID); len(rows) != 0 {
		t.Fatalf("refusal wrote %d notices", len(rows))
	}
}

// requestNotices returns the unsent answered notices of one request.
func requestNotices(t *testing.T, s *Store, requestID string) []NotificationOutbox {
	t.Helper()
	var out []NotificationOutbox
	for _, row := range outboxRows(t, s) {
		if strings.HasPrefix(row.EventID, "decision-answered-"+requestID+"-") {
			out = append(out, row)
		}
	}
	return out
}

// Invariant: an answer's free text reaches the lane notice after the option,
// clipped with "…" and a full-text note when long; an option-only answer is
// unchanged. The asker's copy (hk 5129) carries the same text.
func TestDecisionAnswerNoticeCarriesText(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()

	answerText := func(request DecisionRequest, option, text string) string {
		t.Helper()
		in := answerA(request, DecisionChannelCLI, "operator")
		in.Option, in.Text = option, text
		if _, err := s.ResolveDecision(ctx, in); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		rows := requestNotices(t, s, request.ID)
		if len(rows) != 1 {
			t.Fatalf("notices=%+v, want 1", rows)
		}
		return rows[0].Text
	}

	task := assistantTask(t, s, "in_progress")
	got := answerText(assistantRecord(t, s, task.ID), "A", "")
	if !strings.Contains(got, "A #481 writer 명세에 통합 by operator via cli") || strings.Contains(got, "—") {
		t.Fatalf("option-only changed: %q", got)
	}

	task = assistantTask(t, s, "in_progress")
	got = answerText(assistantRecord(t, s, task.ID), "A", "나한테 안 보여주고 바로 정리해도 돼")
	if !strings.Contains(got, "A #481 writer 명세에 통합 — 나한테 안 보여주고 바로 정리해도 돼") || strings.Contains(got, "…") || strings.Contains(got, "full text in record") {
		t.Fatalf("option plus text: %q", got)
	}

	task = assistantTask(t, s, "in_progress")
	got = answerText(assistantRecord(t, s, task.ID), "", "직접 정리함")
	if !strings.Contains(got, "직접 정리함") || strings.Contains(got, "…") {
		t.Fatalf("text-only: %q", got)
	}

	task = assistantTask(t, s, "in_progress")
	long := strings.Repeat("가", 300)
	got = answerText(assistantRecord(t, s, task.ID), "A", long)
	if !strings.Contains(got, strings.Repeat("가", 200)+"…") || !strings.Contains(got, " (full text in record)") || strings.Contains(got, long) {
		t.Fatalf("long text: %q", got)
	}

	askerTask := assistantTask(t, s, "in_progress")
	askerIn := decisionInput()
	askerIn.RequestLane = "asker-lane"
	askerRequest := assistantRecordWith(t, s, askerTask.ID, askerIn)
	in := answerA(askerRequest, DecisionChannelCLI, "operator")
	in.Text = "나한테 안 보여주고 바로 정리해도 돼"
	if _, err := s.ResolveDecision(ctx, in); err != nil {
		t.Fatalf("resolve asker: %v", err)
	}
	rows := requestNotices(t, s, askerRequest.ID)
	if len(rows) != 2 {
		t.Fatalf("asker notices=%+v, want 2", rows)
	}
	for _, row := range rows {
		if !strings.Contains(row.Text, "A #481 writer 명세에 통합 — 나한테 안 보여주고 바로 정리해도 돼") {
			t.Fatalf("asker copy lost the text: %q", row.Text)
		}
	}
}
