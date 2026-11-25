package store

import (
	"context"
	"testing"
	"time"
)

// settleInboxRow reads the inbox row of one request: its status and the
// superseded_by that closed it. Both dialects read it through s.q.
func settleInboxRow(t *testing.T, s *Store, requestID string) (status, supersededBy string) {
	t.Helper()
	if err := s.db().QueryRow(t.Context(),
		s.q(`SELECT status, COALESCE(superseded_by,'') FROM decision_requests WHERE source IN ('task_request','linear') AND source_ref=$1`,
			`SELECT status, COALESCE(superseded_by,'') FROM decision_requests WHERE source IN ('task_request','linear') AND source_ref=?`), requestID).Scan(&status, &supersededBy); err != nil {
		t.Fatalf("inbox row %s: %v", requestID, err)
	}
	return status, supersededBy
}

// settleTo moves a task with an open request to a settled state through the
// transition writer, the way an operator or a source event does.
func settleTo(t *testing.T, s *Store, id int64, to string) {
	t.Helper()
	ctx := context.Background()
	var err error
	switch to {
	case StateMerged:
		if _, err = s.TransitionTask(ctx, id, "verifying", "dr-test", "step", nil, ""); err == nil {
			_, err = s.TransitionTask(ctx, id, StateMerged, "dr-test", "finish", nil, "")
		}
	case StateCompleted:
		_, err = s.TransitionTaskWith(ctx, id, TransitionInput{To: StateCompleted, By: "dr-test", Note: "finish", Evidence: "done"})
	case StateDropped:
		_, err = s.TransitionTask(ctx, id, StateDropped, "dr-test", "finish", nil, "")
	default:
		t.Fatalf("settleTo: no path to %s", to)
	}
	if err != nil {
		t.Fatalf("settle %d to %s: %v", id, to, err)
	}
}

// settleWithdrawCase is TestSettleWithdrawsOpenDecisionRequest for one target
// state: the request is withdrawn with the reason, its inbox row is
// superseded, and drift no longer lists the task.
func settleWithdrawCase(t *testing.T, s *Store, to string) {
	t.Helper()
	ctx := context.Background()
	task := decisionTask(t, s, "in_progress")
	open, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", decisionInput())
	if err != nil {
		t.Fatal(err)
	}
	settleTo(t, s, task.ID, to)

	got, _, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := got.Refs.DecisionRequest
	if r == nil || r.Status != DecisionRequestWithdrawn || r.WithdrawnReason != DecisionWithdrawnTaskSettled {
		t.Fatalf("%s: refs request=%+v, want withdrawn with reason %s", to, r, DecisionWithdrawnTaskSettled)
	}
	if status, by := settleInboxRow(t, s, open.Request.ID); status != "superseded" || by != DecisionRequestWithdrawn {
		t.Fatalf("%s: inbox status=%s superseded_by=%s, want superseded by withdrawn", to, status, by)
	}
	if ids := driftIDs(driftAt(t, s, time.Now().UTC(), DriftDefaultMergedDays).SettledOpenDecision); ids[task.ID] {
		t.Fatalf("%s: drift still lists task %d", to, task.ID)
	}
}

// settleKeepsCase is TestSettleKeepsClosedDecisionRequest on either dialect:
// an answered request is not changed when its task settles.
func settleKeepsCase(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	task := decisionTask(t, s, "in_progress")
	open, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", decisionInput())
	if err != nil {
		t.Fatal(err)
	}
	closed, err := s.ResolveDecisionRequest(ctx, task.ID, "director-1", DecisionResolveInput{RequestID: open.Request.ID, Kind: DecisionRequestAnswered, Option: "A", Responder: "operator"})
	if err != nil || closed.Request.Status != DecisionRequestAnswered {
		t.Fatalf("answer: %+v err=%v", closed.Request, err)
	}
	before, _, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	settleTo(t, s, task.ID, StateMerged)

	got, _, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := got.Refs.DecisionRequest
	if r == nil || r.Status != DecisionRequestAnswered || r.WithdrawnReason != "" || r.Resolution == nil || r.Resolution.Option != "A" {
		t.Fatalf("answered request after settle=%+v, want unchanged", r)
	}
	if before.Refs.DecisionRequest == nil || before.Refs.DecisionRequest.Status != DecisionRequestAnswered {
		t.Fatalf("answered request before settle=%+v", before.Refs.DecisionRequest)
	}
}

// settledBackfillSetup leaves three settled tasks (merged, completed, dropped)
// and one unsettled task, each with an open request and an open inbox row, the
// shape the backfill finds in production. It writes the rows directly, so no
// writer withdraws them first.
func settledBackfillSetup(t *testing.T, s *Store) (settled []int64, open string) {
	t.Helper()
	ctx := context.Background()
	for _, state := range []string{StateMerged, StateCompleted, StateDropped} {
		task := decisionTask(t, s, "in_progress")
		if _, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", decisionInput()); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db().Exec(ctx, s.q(`UPDATE tasks SET state=$1 WHERE id=$2`, `UPDATE tasks SET state=? WHERE id=?`), state, task.ID); err != nil {
			t.Fatal(err)
		}
		settled = append(settled, task.ID)
	}
	unsettled := decisionTask(t, s, "in_progress")
	req, err := s.RecordDecisionRequest(ctx, unsettled.ID, "director-1", decisionInput())
	if err != nil {
		t.Fatal(err)
	}
	return settled, req.Request.ID
}
