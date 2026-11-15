package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/guard"
)

// Each test runs in a throwaway schema so it can count every decision row.
func decisionStore(t *testing.T) *Store { return schemaStore(t, "decision_records") }

func decisionRowsWhere(t *testing.T, s *Store, where string, args ...any) []DecisionRecord {
	t.Helper()
	rows, err := s.pool.Query(t.Context(), `SELECT `+decisionRecordColumns+` FROM decision_requests WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []DecisionRecord{}
	for rows.Next() {
		var x DecisionRecord
		if err := scanDecisionRecord(rows, &x, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func decisionRowsOf(t *testing.T, s *Store, source, ref string) []DecisionRecord {
	t.Helper()
	return decisionRowsWhere(t, s, `source=$1 AND source_ref=$2`, source, ref)
}

// onlyDecision asserts exactly one row with the given status.
func onlyDecision(t *testing.T, rows []DecisionRecord, status string) DecisionRecord {
	t.Helper()
	if len(rows) != 1 || rows[0].Status != status {
		t.Fatalf("rows=%+v, want one %s", rows, status)
	}
	return rows[0]
}

func decisionRelay(t *testing.T, s *Store, x RelayEvent) (RelayEvent, bool) {
	t.Helper()
	out, created, err := s.AppendRelayEvent(t.Context(), x)
	if err != nil {
		t.Fatalf("append %s: %v", x.Kind, err)
	}
	return out, created
}

func laneDecisionOpen(t *testing.T, s *Store, id int64) bool {
	t.Helper()
	open, err := s.ListOpenLaneDecisions(t.Context(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range open {
		if e.ID == id {
			return true
		}
	}
	return false
}

func escalationOpen(t *testing.T, s *Store, id int64) bool {
	t.Helper()
	open, err := s.ListOpenEscalations(t.Context(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range open {
		if e.ID == id {
			return true
		}
	}
	return false
}

func TestDecisionRecordTaskFlag(t *testing.T) {
	s := decisionStore(t)
	ctx := t.Context()
	task := decisionTask(t, s, "backlog")
	if _, err := s.TransitionTask(ctx, task.ID, "needs_decision", "asker", "ship it?", nil, ""); err != nil {
		t.Fatal(err)
	}
	row := onlyDecision(t, decisionRowsWhere(t, s, `task_id=$1`, task.ID), DecisionStatusOpen)
	if row.Source != DecisionSourceTaskFlag || row.Question != "ship it?" || row.Lane != task.Lane || !strings.HasPrefix(row.SourceRef, fmt.Sprintf("%d/", task.ID)) {
		t.Fatalf("flag row=%+v", row)
	}
	// A second ask while the task waits is refused and adds nothing.
	if _, err := s.TransitionTask(ctx, task.ID, "needs_decision", "asker", "again?", nil, ""); err == nil {
		t.Fatal("second ask on a flagged task was accepted")
	}
	onlyDecision(t, decisionRowsWhere(t, s, `task_id=$1`, task.ID), DecisionStatusOpen)

	if _, err := s.ClearAwaitingDecision(ctx, task.ID, "operator-lane", "go ahead"); err != nil {
		t.Fatal(err)
	}
	row = onlyDecision(t, decisionRowsWhere(t, s, `task_id=$1`, task.ID), DecisionStatusAnswered)
	if row.Answer != "go ahead" || row.AnsweredBy != "operator-lane" || row.AnsweredAt == nil {
		t.Fatalf("answered row=%+v", row)
	}
	// Asking again is a new question with its own row.
	if _, err := s.TransitionTask(ctx, task.ID, "needs_decision", "asker", "next?", nil, ""); err != nil {
		t.Fatal(err)
	}
	rows := decisionRowsWhere(t, s, `task_id=$1`, task.ID)
	if len(rows) != 2 || rows[0].Status != DecisionStatusAnswered || rows[1].Status != DecisionStatusOpen || rows[1].Question != "next?" || rows[0].SourceRef == rows[1].SourceRef {
		t.Fatalf("re-ask rows=%+v", rows)
	}
}

func TestRecordSessionDecisionClosesAndReplaysVerbatimRecord(t *testing.T) {
	s := decisionStore(t)
	now := time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)
	decidedAt := now.Add(-time.Hour)
	in := SessionDecisionInput{
		Key: "chat-42", Lane: "design-lane", By: "settling-session", Reading: "Keep the current boundary.",
		Quote: "Keep the existing boundary as-is.", DecidedAt: decidedAt,
		Grounds: []string{"hk:doc decision/2026-11-13/session-a", "https://example.test/ground"},
	}
	first, err := s.RecordSessionDecision(t.Context(), in, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := first.Record
	if first.Duplicate || r.Source != DecisionSourceSessionChat || r.SourceRef != "design-lane:chat-42" || r.Status != DecisionStatusAnswered ||
		r.Question != in.Reading || r.Reading != in.Reading || r.Answer != in.Quote || r.AnsweredBy != in.By || r.AnsweredAt == nil || !r.AnsweredAt.Equal(decidedAt) || !sameStrings(r.Grounds, in.Grounds) {
		t.Fatalf("record=%+v, duplicate=%t", r, first.Duplicate)
	}
	issue := decisionTextOf(r, "")
	if issue.Reading != in.Reading || !sameStrings(issue.Grounds, in.Grounds) || issue.Answer != in.Quote {
		t.Fatalf("YouTrack decision text=%+v", issue)
	}
	second, err := s.RecordSessionDecision(t.Context(), in, now.Add(time.Minute), nil)
	if err != nil || !second.Duplicate || second.Record.ID != r.ID {
		t.Fatalf("retry=%+v err=%v", second, err)
	}
	in.Quote = "Changed words"
	if _, err := s.RecordSessionDecision(t.Context(), in, now, nil); !errors.Is(err, ErrSessionDecisionConflict) {
		t.Fatalf("changed retry err=%v, want conflict", err)
	}
}

// hk 4518: the same settled decision retried through the other verified
// channel — the MCP Access principal vs the reserved operator bearer —
// replays the first record instead of conflicting on AnsweredBy, and a
// decided_at finer than Postgres microseconds still matches.
func TestRecordSessionDecisionReplayAcrossChannels(t *testing.T) {
	s := decisionStore(t)
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	in := SessionDecisionInput{
		Key: "chat-cross", Lane: "design-lane", By: "operator:admin@example.com",
		Reading: "Keep the boundary.", Quote: "Keep it.",
		DecidedAt: time.Date(2026, 11, 13, 8, 30, 0, 123456789, time.UTC),
		Grounds:   []string{"hk:doc decision/2026-11-13/cross"},
	}
	first, err := s.RecordSessionDecision(t.Context(), in, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The operator bearer retries the identical decision.
	in.By = "operator"
	second, err := s.RecordSessionDecision(t.Context(), in, now.Add(time.Minute), nil)
	if err != nil || !second.Duplicate || second.Record.ID != first.Record.ID {
		t.Fatalf("cross-channel retry=%+v err=%v", second, err)
	}
	if second.Record.AnsweredBy != "operator:admin@example.com" {
		t.Fatalf("answered_by=%q, want the first channel's recorder", second.Record.AnsweredBy)
	}
	// The original channel's own retry matches the truncated timestamp too.
	in.By = "operator:admin@example.com"
	third, err := s.RecordSessionDecision(t.Context(), in, now.Add(2*time.Minute), nil)
	if err != nil || !third.Duplicate || third.Record.ID != first.Record.ID {
		t.Fatalf("same-channel retry=%+v err=%v", third, err)
	}
}

func TestRecordSessionDecisionRejectsSecretsBeforeInsert(t *testing.T) {
	s := decisionStore(t)
	in := SessionDecisionInput{
		Key: "secret-quote", Lane: "design-lane", By: "settling-session", Reading: "Keep the boundary.",
		Quote: "The credential is " + "AKIA" + strings.Repeat("X", 16), DecidedAt: time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC),
		Grounds: []string{"hk:doc decision/2026-11-13/session-a"},
	}
	_, err := s.RecordSessionDecision(t.Context(), in, time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC), nil)
	if pattern, ok := guard.SecretPattern(err); !ok || pattern != "aws_access_key" {
		t.Fatalf("error=%v, secret pattern=%q ok=%t", err, pattern, ok)
	}
	if rows := decisionRowsOf(t, s, DecisionSourceSessionChat, "design-lane:secret-quote"); len(rows) != 0 {
		t.Fatalf("secret-bearing decision was inserted: %+v", rows)
	}
}

// A disposition item is a task_flag question only the operator's web answer
// resolves; any other answer leaves the item and its row open.
func TestDecisionRecordDispositionOperatorOnly(t *testing.T) {
	s := decisionStore(t)
	ctx := t.Context()
	merged := time.Now().UTC().Add(-time.Hour)
	in := DispositionInput{Lane: "decision-disposition", Project: testProjectName, Title: "disposition", OriginPR: "https://github.com/example/decisions/pull/7", MergeSHA: strings.Repeat("b", 40), MergedAt: &merged, Install: DispositionInstall{State: "unknown"}, Recommended: "E", CreatedBy: "director"}
	item, created, err := s.CreateDisposition(ctx, in)
	if err != nil || !created {
		t.Fatalf("create created=%v err=%v", created, err)
	}
	gen, err := dispositionGen(ctx, s.pool, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	row := onlyDecision(t, decisionRowsWhere(t, s, `task_id=$1`, item.ID), DecisionStatusOpen)
	if row.Source != DecisionSourceTaskFlag || row.SourceRef != fmt.Sprintf("%d/%d", item.ID, gen) || row.Options == nil || row.Recommended != "E" {
		t.Fatalf("disposition row=%+v", row)
	}
	again, created, err := s.CreateDisposition(ctx, in)
	if err != nil || created || again.ID != item.ID {
		t.Fatalf("redelivered create id=%d created=%v err=%v", again.ID, created, err)
	}
	onlyDecision(t, decisionRowsWhere(t, s, `task_id=$1`, item.ID), DecisionStatusOpen)

	if _, err := s.ClearAwaitingDecision(ctx, item.ID, "some-lane", "E"); !errors.Is(err, ErrDispositionOperatorOnly) {
		t.Fatalf("non-operator answer err=%v", err)
	}
	if got, _, err := s.GetTask(ctx, item.ID); err != nil || !got.AwaitingDecision {
		t.Fatalf("source after refused answer awaiting=%v err=%v", got.AwaitingDecision, err)
	}
	onlyDecision(t, decisionRowsWhere(t, s, `task_id=$1`, item.ID), DecisionStatusOpen)

	answered, err := s.AnswerDisposition(ctx, DispositionAnswerInput{ID: item.ID, Gen: gen, Key: "E", OperatorEmail: "admin@example.com", EventID: "decision-disposition"})
	if err != nil {
		t.Fatal(err)
	}
	stored := stateOf(t, s, item.ID)
	if !answered.LastActivityAt.After(item.LastActivityAt) || !answered.LastActivityAt.Equal(stored.LastActivityAt) {
		t.Fatalf("disposition activity before=%v returned=%v stored=%v", item.LastActivityAt, answered.LastActivityAt, stored.LastActivityAt)
	}
	row = onlyDecision(t, decisionRowsWhere(t, s, `task_id=$1`, item.ID), DecisionStatusAnswered)
	if row.AnsweredBy != "operator:admin@example.com" || !strings.Contains(row.Answer, "disposition-answer E") {
		t.Fatalf("operator answer row=%+v", row)
	}
}

func TestDecisionRecordTaskRequest(t *testing.T) {
	s := decisionStore(t)
	ctx := t.Context()
	task := decisionTask(t, s, "in_progress")
	first, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", decisionInput())
	if err != nil {
		t.Fatal(err)
	}
	row := onlyDecision(t, decisionRowsOf(t, s, DecisionSourceTaskRequest, first.Request.ID), DecisionStatusOpen)
	if row.TaskID == nil || *row.TaskID != task.ID || row.Recommended != "A" || row.DefaultAction != decisionInput().DefaultAction || row.Options == nil || row.Question != decisionInput().Question {
		t.Fatalf("request row=%+v", row)
	}
	if dup, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", decisionInput()); err != nil || !dup.Duplicate {
		t.Fatalf("re-send duplicate=%v err=%v", dup.Duplicate, err)
	}
	onlyDecision(t, decisionRowsWhere(t, s, `task_id=$1`, task.ID), DecisionStatusOpen)

	// A new revision replaces the open one: one open row per question.
	next := decisionInput()
	next.Question = "allowlist 는 writer 에 둘까?"
	next.Supersedes = first.Request.ID
	second, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", next)
	if err != nil {
		t.Fatal(err)
	}
	old := onlyDecision(t, decisionRowsOf(t, s, DecisionSourceTaskRequest, first.Request.ID), DecisionStatusSuperseded)
	if old.SupersededBy != "task_request:"+second.Request.ID {
		t.Fatalf("superseded_by=%q", old.SupersededBy)
	}
	onlyDecision(t, decisionRowsWhere(t, s, `task_id=$1 AND status='open'`, task.ID), DecisionStatusOpen)

	if _, err := s.ResolveDecisionRequest(ctx, task.ID, "operator", DecisionResolveInput{RequestID: second.Request.ID, Kind: DecisionRequestAnswered, Option: "B", Text: "repo"}); err != nil {
		t.Fatal(err)
	}
	row = onlyDecision(t, decisionRowsOf(t, s, DecisionSourceTaskRequest, second.Request.ID), DecisionStatusAnswered)
	if row.Answer != "B: repo" || row.AnsweredBy != "operator" || row.AnsweredAt == nil {
		t.Fatalf("answered row=%+v", row)
	}
}

func TestDecisionRecordJobEscalate(t *testing.T) {
	s := decisionStore(t)
	ctx := t.Context()
	task := decisionTask(t, s, "backlog")
	if _, _, err := s.ClaimJob(ctx, Job{JobID: "dr-esc-1", TaskID: &task.ID, Host: "host-a", CreatedBy: "decision-test"}); err != nil {
		t.Fatal(err)
	}
	esc := RelayEvent{Kind: "job.escalate", JobID: "dr-esc-1", OwnerLane: "esc-lane", Question: "which way?", ReportPath: "report-1"}
	e, created := decisionRelay(t, s, esc)
	if !created {
		t.Fatal("escalation not created")
	}
	ref := fmt.Sprint(e.ID)
	row := onlyDecision(t, decisionRowsOf(t, s, DecisionSourceJobEscalate, ref), DecisionStatusOpen)
	if row.JobID != "dr-esc-1" || row.TaskID == nil || *row.TaskID != task.ID || row.Lane != "esc-lane" || row.Question != "which way?" {
		t.Fatalf("escalation row=%+v", row)
	}
	if _, created := decisionRelay(t, s, esc); created {
		t.Fatal("redelivered escalation created a second event")
	}
	onlyDecision(t, decisionRowsOf(t, s, DecisionSourceJobEscalate, ref), DecisionStatusOpen)

	// A reply from another lane answers nothing, in the source or the row.
	decisionRelay(t, s, RelayEvent{Kind: "lane.event", OwnerLane: "other-lane", EventID: fmt.Sprintf("cli-decision-escalation-%d-a", e.ID), Text: fmt.Sprintf("[decision] #%d: left", e.ID)})
	if !escalationOpen(t, s, e.ID) {
		t.Fatal("another lane's reply closed the escalation")
	}
	onlyDecision(t, decisionRowsOf(t, s, DecisionSourceJobEscalate, ref), DecisionStatusOpen)

	answer, _ := decisionRelay(t, s, RelayEvent{Kind: "lane.event", OwnerLane: "esc-lane", EventID: fmt.Sprintf("cli-decision-escalation-%d-b", e.ID), Text: fmt.Sprintf("[decision] #%d: left (from operator)", e.ID)})
	if escalationOpen(t, s, e.ID) {
		t.Fatal("the owner lane's reply left the escalation open")
	}
	row = onlyDecision(t, decisionRowsOf(t, s, DecisionSourceJobEscalate, ref), DecisionStatusAnswered)
	if row.Answer != answer.Text || row.AnsweredBy != fmt.Sprintf("lane.event #%d", answer.ID) {
		t.Fatalf("answered row=%+v", row)
	}
}

// A lane ask closes only on an exact "[decision-answered] #<id>:" reply from
// its own lane, after it; every other reply leaves the ask and its row open.
func TestDecisionRecordLaneEvent(t *testing.T) {
	s := decisionStore(t)
	q, created := decisionRelay(t, s, RelayEvent{Kind: "lane.event", OwnerLane: "ask-lane", EventID: "ask-1", Text: "[decision-needed] pick one?"})
	if !created {
		t.Fatal("ask not created")
	}
	ref := fmt.Sprint(q.ID)
	row := onlyDecision(t, decisionRowsOf(t, s, DecisionSourceLaneEvent, ref), DecisionStatusOpen)
	if row.Lane != "ask-lane" || row.Question != q.Text || row.TaskID != nil || row.JobID != "" {
		t.Fatalf("ask row=%+v", row)
	}
	if _, created := decisionRelay(t, s, RelayEvent{Kind: "lane.event", OwnerLane: "ask-lane", EventID: "ask-1", Text: "[decision-needed] pick one?"}); created {
		t.Fatal("redelivered ask created a second event")
	}
	onlyDecision(t, decisionRowsOf(t, s, DecisionSourceLaneEvent, ref), DecisionStatusOpen)

	for i, reply := range []RelayEvent{
		{OwnerLane: "other-lane", Text: fmt.Sprintf("[decision-answered] #%d: yes", q.ID)},
		{OwnerLane: "ask-lane", Text: fmt.Sprintf("[decision-answered] #%d yes", q.ID)},
		{OwnerLane: "ask-lane", Text: fmt.Sprintf("[decision-answered] #%d: yes", q.ID+1000)},
		{OwnerLane: "ask-lane", Text: fmt.Sprintf("[decision-answered] #0%d: yes", q.ID)},
		{OwnerLane: "ask-lane", Text: "[decision-answered] yes"},
	} {
		reply.Kind, reply.EventID = "lane.event", fmt.Sprintf("mismatch-%d", i)
		decisionRelay(t, s, reply)
		if !laneDecisionOpen(t, s, q.ID) {
			t.Fatalf("reply %q from %s closed the source", reply.Text, reply.OwnerLane)
		}
		onlyDecision(t, decisionRowsOf(t, s, DecisionSourceLaneEvent, ref), DecisionStatusOpen)
	}

	answer, _ := decisionRelay(t, s, RelayEvent{Kind: "lane.event", OwnerLane: "ask-lane", EventID: "answer-1", Text: fmt.Sprintf("[decision-answered] #%d: yes", q.ID)})
	if laneDecisionOpen(t, s, q.ID) {
		t.Fatal("exact reply left the source open")
	}
	row = onlyDecision(t, decisionRowsOf(t, s, DecisionSourceLaneEvent, ref), DecisionStatusAnswered)
	if row.Answer != answer.Text || row.AnsweredBy != fmt.Sprintf("lane.event #%d", answer.ID) {
		t.Fatalf("answered row=%+v", row)
	}
}

func TestDecisionRecordChat(t *testing.T) {
	s := decisionStore(t)
	ctx := t.Context()
	const qid = "Q-20261105-01"
	if _, created, err := s.UpsertChatQuestion(ctx, ChatQuestion{ID: qid, Lane: "desk-lane", Body: "which?"}); err != nil || !created {
		t.Fatalf("upsert created=%v err=%v", created, err)
	}
	onlyDecision(t, decisionRowsOf(t, s, DecisionSourceChat, qid), DecisionStatusOpen)
	if _, created, err := s.UpsertChatQuestion(ctx, ChatQuestion{ID: qid, Lane: "desk-lane", Body: "which?"}); err != nil || created {
		t.Fatalf("re-send created=%v err=%v", created, err)
	}
	if _, _, err := s.UpsertChatQuestion(ctx, ChatQuestion{ID: qid, Lane: "desk-lane", Body: "which one?"}); err != nil {
		t.Fatal(err)
	}
	row := onlyDecision(t, decisionRowsOf(t, s, DecisionSourceChat, qid), DecisionStatusOpen)
	if row.Question != "which one?" || row.Lane != "desk-lane" {
		t.Fatalf("revised row=%+v", row)
	}
	if _, err := s.TransitionChatQuestion(ctx, qid, "resolved", "desk"); err != nil {
		t.Fatal(err)
	}
	row = onlyDecision(t, decisionRowsOf(t, s, DecisionSourceChat, qid), DecisionStatusAnswered)
	if row.AnsweredBy != "desk" || row.AnsweredAt == nil {
		t.Fatalf("resolved row=%+v", row)
	}

	const withdrawn = "Q-20261105-02"
	if _, _, err := s.UpsertChatQuestion(ctx, ChatQuestion{ID: withdrawn, Lane: "desk-lane", Body: "never mind?"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionChatQuestion(ctx, withdrawn, "withdrawn", "desk"); err != nil {
		t.Fatal(err)
	}
	if row := onlyDecision(t, decisionRowsOf(t, s, DecisionSourceChat, withdrawn), DecisionStatusSuperseded); row.SupersededBy != "withdrawn" {
		t.Fatalf("withdrawn row=%+v", row)
	}
}

func TestDecisionRecordSupersededWhenTaskEnds(t *testing.T) {
	for _, tc := range []struct {
		name string
		path []string
	}{
		{"dropped", []string{"dropped"}},
		{"icebox", []string{"icebox"}},
		{"merged", []string{"verifying", "merged"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := decisionStore(t)
			ctx := t.Context()
			ending, other := decisionTask(t, s, "in_progress"), decisionTask(t, s, "in_progress")
			for _, task := range []Task{ending, other} {
				if _, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", decisionInput()); err != nil {
					t.Fatal(err)
				}
			}
			x := ending
			for _, to := range tc.path {
				var err error
				if x, err = s.TransitionTask(ctx, x.ID, to, "decision-test", "end", nil, ""); err != nil {
					t.Fatalf("to %s: %v", to, err)
				}
			}
			row := onlyDecision(t, decisionRowsWhere(t, s, `task_id=$1`, ending.ID), DecisionStatusSuperseded)
			if row.SupersededBy != fmt.Sprintf("task:%d:%s", ending.ID, x.State) {
				t.Fatalf("superseded_by=%q state=%s", row.SupersededBy, x.State)
			}
			onlyDecision(t, decisionRowsWhere(t, s, `task_id=$1`, other.ID), DecisionStatusOpen)
			// The source's own record is not closed by the supersede.
			got, _, err := s.GetTask(ctx, ending.ID)
			if err != nil || got.Refs.DecisionRequest == nil || got.Refs.DecisionRequest.Status != DecisionRequestOpen {
				t.Fatalf("source request after supersede=%+v err=%v", got.Refs.DecisionRequest, err)
			}
		})
	}
}

func TestDecisionRecordSupersededWhenJobEnds(t *testing.T) {
	for _, end := range []string{"completed", "lost"} {
		t.Run(end, func(t *testing.T) {
			s := decisionStore(t)
			ctx := t.Context()
			task := decisionTask(t, s, "backlog")
			refs := map[string]string{}
			for _, job := range []string{"dr-end-a", "dr-end-b"} {
				if _, _, err := s.ClaimJob(ctx, Job{JobID: job, TaskID: &task.ID, Host: "host-a", CreatedBy: "decision-test"}); err != nil {
					t.Fatal(err)
				}
				e, _ := decisionRelay(t, s, RelayEvent{Kind: "job.escalate", JobID: job, OwnerLane: "end-lane", Question: "stop?", ReportPath: job})
				refs[job] = fmt.Sprint(e.ID)
			}
			spawned := "spawned"
			if _, err := s.UpdateJob(ctx, "dr-end-a", JobUpdate{State: &spawned}); err != nil {
				t.Fatal(err)
			}
			state := end
			if _, err := s.UpdateJob(ctx, "dr-end-a", JobUpdate{State: &state}); err != nil {
				t.Fatal(err)
			}
			row := onlyDecision(t, decisionRowsOf(t, s, DecisionSourceJobEscalate, refs["dr-end-a"]), DecisionStatusSuperseded)
			if row.SupersededBy != "job:dr-end-a:"+end {
				t.Fatalf("superseded_by=%q", row.SupersededBy)
			}
			onlyDecision(t, decisionRowsOf(t, s, DecisionSourceJobEscalate, refs["dr-end-b"]), DecisionStatusOpen)
		})
	}
}

// The relay event that closes an escalation in the source supersedes its row.
func TestDecisionRecordSupersededByJobEndEvent(t *testing.T) {
	s := decisionStore(t)
	task := decisionTask(t, s, "backlog")
	if _, _, err := s.ClaimJob(t.Context(), Job{JobID: "dr-relay-end", TaskID: &task.ID, Host: "host-a", CreatedBy: "decision-test"}); err != nil {
		t.Fatal(err)
	}
	e, _ := decisionRelay(t, s, RelayEvent{Kind: "job.escalate", JobID: "dr-relay-end", OwnerLane: "end-lane", Question: "stop?", ReportPath: "r"})
	decisionRelay(t, s, RelayEvent{Kind: "job.lost", JobID: "dr-relay-end", OwnerLane: "end-lane", Reason: "pane gone"})
	if escalationOpen(t, s, e.ID) {
		t.Fatal("job.lost left the escalation open in the source")
	}
	if row := onlyDecision(t, decisionRowsOf(t, s, DecisionSourceJobEscalate, fmt.Sprint(e.ID)), DecisionStatusSuperseded); row.SupersededBy != "job:dr-relay-end:lost" {
		t.Fatalf("superseded_by=%q", row.SupersededBy)
	}
}

func listedIDs(t *testing.T, s *Store, f DecisionRecordFilter, now time.Time) map[int64]string {
	t.Helper()
	xs, err := s.ListDecisionRecords(t.Context(), f, now)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int64]string{}
	for _, x := range xs {
		out[x.ID] = x.Status
	}
	return out
}

func TestDecisionRecordSnoozeAndExpiry(t *testing.T) {
	s := decisionStore(t)
	ctx := t.Context()
	for _, id := range []string{"Q-20261105-10", "Q-20261105-11"} {
		if _, _, err := s.UpsertChatQuestion(ctx, ChatQuestion{ID: id, Lane: "desk-lane", Body: "q " + id}); err != nil {
			t.Fatal(err)
		}
	}
	snoozed := onlyDecision(t, decisionRowsOf(t, s, DecisionSourceChat, "Q-20261105-10"), DecisionStatusOpen)
	expiring := onlyDecision(t, decisionRowsOf(t, s, DecisionSourceChat, "Q-20261105-11"), DecisionStatusOpen)
	before, _, err := s.GetChatQuestion(ctx, "Q-20261105-10")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2030, 1, 1, 9, 0, 0, 0, time.UTC)

	// Snooze: hidden from the default list until the time, then shown.
	got, err := s.SnoozeDecisionRecord(ctx, snoozed.ID, t0.Add(time.Hour), t0)
	if err != nil || !got.Snoozed || got.SnoozedUntil == nil || !got.SnoozedUntil.Equal(t0.Add(time.Hour)) {
		t.Fatalf("snooze=%+v err=%v", got, err)
	}
	if _, ok := listedIDs(t, s, DecisionRecordFilter{}, t0)[snoozed.ID]; ok {
		t.Fatal("snoozed record listed by default")
	}
	if listedIDs(t, s, DecisionRecordFilter{IncludeSnoozed: true}, t0)[snoozed.ID] != DecisionStatusOpen {
		t.Fatal("include_snoozed hid the record")
	}
	if listedIDs(t, s, DecisionRecordFilter{}, t0.Add(time.Hour))[snoozed.ID] != DecisionStatusOpen {
		t.Fatal("record still hidden once its snooze ended")
	}

	// Expiry: open until expires_at, then listed as expired; the stored status
	// stays open.
	if _, err := s.pool.Exec(ctx, `UPDATE decision_requests SET expires_at=$2 WHERE id=$1`, expiring.ID, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if listedIDs(t, s, DecisionRecordFilter{}, t0)[expiring.ID] != DecisionStatusOpen {
		t.Fatal("record hidden before its expiry")
	}
	late := t0.Add(2 * time.Hour)
	if _, ok := listedIDs(t, s, DecisionRecordFilter{}, late)[expiring.ID]; ok {
		t.Fatal("expired record listed as open")
	}
	if listedIDs(t, s, DecisionRecordFilter{Status: DecisionStatusExpired}, late)[expiring.ID] != DecisionStatusExpired {
		t.Fatal("expired record not listed as expired")
	}
	onlyDecision(t, decisionRowsOf(t, s, DecisionSourceChat, "Q-20261105-11"), DecisionStatusOpen)

	// Neither touched the source.
	after, _, err := s.GetChatQuestion(ctx, "Q-20261105-10")
	if err != nil || after.State != "pending" || after.Revision != before.Revision || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("source after snooze=%+v before=%+v err=%v", after, before, err)
	}
	if q, _, err := s.GetChatQuestion(ctx, "Q-20261105-11"); err != nil || q.State != "pending" {
		t.Fatalf("source after expiry=%+v err=%v", q, err)
	}

	// Only an open record can be snoozed.
	if _, err := s.TransitionChatQuestion(ctx, "Q-20261105-10", "resolved", "desk"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SnoozeDecisionRecord(ctx, snoozed.ID, t0.Add(time.Hour), t0); !errors.Is(err, ErrDecisionRecordNotOpen) {
		t.Fatalf("snooze of an answered record err=%v", err)
	}
	if _, err := s.SnoozeDecisionRecord(ctx, snoozed.ID+1000, t0.Add(time.Hour), t0); !errors.Is(err, ErrDecisionRecordNotFound) {
		t.Fatalf("snooze of a missing record err=%v", err)
	}
}

// A due date passing changes how a record reads, never its status.
func TestDecisionRecordOverdueIsDisplayOnly(t *testing.T) {
	s := decisionStore(t)
	ctx := context.Background()
	task := decisionTask(t, s, "in_progress")
	in := decisionInput()
	due := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	in.DueAt = &due
	got, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", in)
	if err != nil {
		t.Fatal(err)
	}
	xs, err := s.ListDecisionRecords(ctx, DecisionRecordFilter{Source: DecisionSourceTaskRequest}, due.Add(time.Minute))
	if err != nil || len(xs) != 1 || !xs[0].Overdue || xs[0].Status != DecisionStatusOpen || xs[0].SourceRef != got.Request.ID {
		t.Fatalf("overdue list=%+v err=%v", xs, err)
	}
}

func TestTasklessJobDecisions(t *testing.T) {
	s := decisionStore(t)
	for _, id := range []string{"taskless-escalation", "taskless-lane"} {
		if _, _, err := s.ClaimJob(t.Context(), Job{JobID: id, Host: "host-a", CreatedBy: "decision-test"}); err != nil {
			t.Fatal(err)
		}
	}
	escalation, created := decisionRelay(t, s, RelayEvent{Kind: "job.escalate", JobID: "taskless-escalation", OwnerLane: "test-lane", Question: "which way?", ReportPath: "taskless-report"})
	if !created {
		t.Fatal("escalation not created")
	}
	row := onlyDecision(t, decisionRowsOf(t, s, DecisionSourceJobEscalate, fmt.Sprint(escalation.ID)), DecisionStatusOpen)
	if row.JobID != "taskless-escalation" || row.TaskID != nil {
		t.Fatalf("escalation: %+v", row)
	}
	lane, created := decisionRelay(t, s, RelayEvent{Kind: "lane.event", JobID: "taskless-lane", OwnerLane: "test-lane", EventID: "taskless-ask", Text: "[decision-needed] choose?"})
	if !created {
		t.Fatal("lane event not created")
	}
	row = onlyDecision(t, decisionRowsOf(t, s, DecisionSourceLaneEvent, fmt.Sprint(lane.ID)), DecisionStatusOpen)
	if row.JobID != "taskless-lane" || row.TaskID != nil {
		t.Fatalf("lane decision: %+v", row)
	}
	sources, err := s.ListOpenDecisionSources(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range sources.Items {
		if item.Source == DecisionSourceJobEscalate && item.JobID == "taskless-escalation" {
			found = true
			if item.TaskID != nil || item.JobState != "claimed" {
				t.Fatalf("source: %+v", item)
			}
		}
	}
	if !found {
		t.Fatalf("taskless escalation missing: %+v", sources.Items)
	}
}
