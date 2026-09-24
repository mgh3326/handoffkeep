package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func decisionTask(t *testing.T, s *Store, state string) Task {
	t.Helper()
	ctx := context.Background()
	x, err := s.CreateTask(ctx, Task{Lane: "b618-" + strings.ReplaceAll(t.Name(), "/", "-"), Title: "decision task", Kind: "implement", CreatedBy: "dr-test"})
	if err != nil {
		t.Fatal(err)
	}
	path := map[string][]string{
		"backlog":     nil,
		"claimed":     {"claimed"},
		"in_progress": {"claimed", "in_progress"},
		"merged":      {"claimed", "in_progress", "verifying", "merged"},
		"dropped":     {"dropped"},
	}[state]
	for _, to := range path {
		if to == "claimed" {
			x, err = s.ClaimTask(ctx, x.ID, "dr-test")
		} else {
			x, err = s.TransitionTask(ctx, x.ID, to, "dr-test", "step", nil)
		}
		if err != nil {
			t.Fatalf("to %s: %v", to, err)
		}
	}
	return x
}

// The #492 shape: recommendation A, but silence means "hold and move on" —
// not A. The two stay separate fields.
func decisionInput() DecisionRequestInput {
	return DecisionRequestInput{
		Question: "allowlist 를 어디에 구현할까?",
		Options: DecisionOptions{AllowFree: true, Options: []DecisionOption{
			{Key: "A", Label: "#481 writer 명세에 통합", Recommended: true},
			{Key: "B", Label: "백필 도구 레포에 반영"},
			{Key: "C", Label: "hk 투영 함수"},
		}},
		Reason:        "writer 한 곳에서 막는 것이 가장 싸다",
		DefaultAction: "보류하고 다음 태스크로 이동",
	}
}

func TestDecisionRequestRecommendationAndDefaultAreSeparate(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	task := decisionTask(t, s, "in_progress")
	got, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", decisionInput())
	if err != nil {
		t.Fatal(err)
	}
	r := got.Request
	if got.Duplicate || r.ID != "dr-"+strconv.FormatInt(task.ID, 10)+"-1" || r.Revision != 1 || r.Status != DecisionRequestOpen || r.Resolution != nil {
		t.Fatalf("request=%+v", r)
	}
	if r.DefaultAction != "보류하고 다음 태스크로 이동" || r.DefaultOption != "" || r.DueAt != nil || r.RequestedBy != "director-1" {
		t.Fatalf("default fields=%+v", r)
	}
	if got.Task.State != "in_progress" {
		t.Fatalf("a non-blocking request changed the state: %s", got.Task.State)
	}
	rec := ""
	for _, o := range got.Task.Refs.DecisionOptions.Options {
		if o.Recommended {
			rec = o.Key
		}
	}
	if rec != "A" {
		t.Fatalf("recommended=%q", rec)
	}
	// No deadline: open, never overdue, however late it is read.
	if state := DecisionRequestState(r, got.Task.State, false, time.Now().Add(10*365*24*time.Hour)); state != DecisionViewOpen {
		t.Fatalf("no-deadline state=%s", state)
	}
	// The recording row is a decision event, not a state transition.
	full, _, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := full.Events[len(full.Events)-1]
	if last.Kind != TaskEventDecision || last.From != "in_progress" || last.To != "in_progress" || last.Refs == nil || last.Refs.DecisionRequest == nil || last.Refs.DecisionRequest.ID != r.ID {
		t.Fatalf("event=%+v", last)
	}
}

// A3: a passed deadline is "overdue, not applied" — never applied by time
// alone. Only a default_applied resolution with a receipt reads as applied.
func TestDecisionRequestDeadlinePassedIsNotApplied(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	task := decisionTask(t, s, "in_progress")
	in := decisionInput()
	due := time.Now().Add(-time.Hour)
	in.DueAt = &due
	in.DefaultTrigger = "기한까지 무응답이면 director 가 적용"
	got, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", in)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if state := DecisionRequestState(got.Request, "in_progress", false, now); state != DecisionViewOverdue {
		t.Fatalf("state=%s", state)
	}
	// Reading does not write: the record is still open with no resolution.
	again, _, _ := s.GetTask(ctx, task.ID)
	if again.Refs.DecisionRequest.Status != DecisionRequestOpen || again.Refs.DecisionRequest.Resolution != nil {
		t.Fatalf("deadline changed the record: %+v", again.Refs.DecisionRequest)
	}
	open, err := s.ListOpenDecisionRequests(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if pending, uncleaned := DecisionRequestCounts(open); pending != 1 || uncleaned != 0 {
		t.Fatalf("pending=%d uncleaned=%d", pending, uncleaned)
	}
	// No receipt, no application.
	if _, err := s.ResolveDecisionRequest(ctx, task.ID, "director-1", DecisionResolveInput{RequestID: got.Request.ID, Kind: DecisionRequestDefaultApplied}); !errors.Is(err, ErrInvalidDecisionRequest) {
		t.Fatalf("default_applied without receipt: %v", err)
	}
	applied, err := s.ResolveDecisionRequest(ctx, task.ID, "director-1", DecisionResolveInput{RequestID: got.Request.ID, Kind: DecisionRequestDefaultApplied, Receipt: "hk:doc receipt/2026-09-24/t618"})
	if err != nil {
		t.Fatal(err)
	}
	if state := DecisionRequestState(applied.Request, "in_progress", false, now); state != DecisionViewDefaultApplied {
		t.Fatalf("applied state=%s", state)
	}
	// A status forged without a receipt still does not read as applied.
	forged := applied.Request
	forged.Resolution = &DecisionResolution{Kind: DecisionRequestDefaultApplied}
	if state := DecisionRequestState(forged, "in_progress", false, now); state == DecisionViewDefaultApplied {
		t.Fatalf("receipt-less resolution read as applied")
	}
}

// A5/A9: a new request on the same task replaces an open one only when it
// names it, gets a new id and revision, and never carries the old answer.
func TestDecisionRequestSupersedeNeverInheritsAnswer(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	task := decisionTask(t, s, "in_progress")
	first, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", decisionInput())
	if err != nil {
		t.Fatal(err)
	}
	second := decisionInput()
	second.Question = "범위를 줄여서 다시 묻는다: allowlist 위치?"
	second.Options.Options = []DecisionOption{{Key: "A", Label: "writer"}, {Key: "B", Label: "투영", Recommended: true}}
	if _, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", second); !errors.Is(err, ErrDecisionRequestOpen) {
		t.Fatalf("unnamed replacement: %v", err)
	}
	second.Supersedes = "dr-999999-1"
	if _, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", second); !errors.Is(err, ErrDecisionRequestStale) {
		t.Fatalf("wrong supersedes: %v", err)
	}
	second.Supersedes = first.Request.ID
	replaced, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", second)
	if err != nil {
		t.Fatal(err)
	}
	if replaced.Request.Revision != 2 || replaced.Request.ID == first.Request.ID || replaced.Request.Resolution != nil || replaced.Request.Status != DecisionRequestOpen {
		t.Fatalf("replacement=%+v", replaced.Request)
	}
	// An answer addressed to the replaced request is refused.
	if _, err := s.ResolveDecisionRequest(ctx, task.ID, "director-1", DecisionResolveInput{RequestID: first.Request.ID, Kind: DecisionRequestAnswered, Option: "A"}); !errors.Is(err, ErrDecisionRequestStale) {
		t.Fatalf("answer to replaced request: %v", err)
	}
	answered, err := s.ResolveDecisionRequest(ctx, task.ID, "director-1", DecisionResolveInput{RequestID: replaced.Request.ID, Kind: DecisionRequestAnswered, Option: "B", Responder: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	// A third request after the answer starts clean.
	third := decisionInput()
	third.Question = "후속: 백필은 언제?"
	fresh, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", third)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Request.Revision != 3 || fresh.Request.Resolution != nil || fresh.Request.Status != DecisionRequestOpen {
		t.Fatalf("third=%+v", fresh.Request)
	}
	full, _, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	history := DecisionRequestHistory(full)
	if len(history) != 3 {
		t.Fatalf("history=%+v", history)
	}
	byID := map[string]DecisionRequestEntry{}
	for _, entry := range history {
		byID[entry.Request.ID] = entry
	}
	if history[0].Request.ID != fresh.Request.ID || !history[0].Current || history[0].Request.Resolution != nil {
		t.Fatalf("current=%+v", history[0])
	}
	r2 := byID[answered.Request.ID]
	if r2.Current || r2.Request.Status != DecisionRequestAnswered || r2.Request.Resolution.Option != "B" || r2.SupersededBy != "" || len(r2.Options.Options) != 2 {
		t.Fatalf("r2=%+v", r2)
	}
	r1 := byID[first.Request.ID]
	if r1.SupersededBy != replaced.Request.ID || r1.Request.Resolution != nil || len(r1.Options.Options) != 3 {
		t.Fatalf("r1=%+v options=%+v", r1, r1.Options)
	}
	if DecisionRequestState(r1.Request, full.State, r1.SupersededBy != "", time.Now()) != DecisionViewSuperseded {
		t.Fatalf("r1 not superseded")
	}
}

// A9 duplicate send: a retry returns the same request_id and writes nothing;
// a retried resolution is idempotent; a different one is refused.
func TestDecisionRequestDuplicateSend(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	task := decisionTask(t, s, "claimed")
	first, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", decisionInput())
	if err != nil {
		t.Fatal(err)
	}
	before, _, _ := s.GetTask(ctx, task.ID)
	again, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", decisionInput())
	if err != nil {
		t.Fatal(err)
	}
	after, _, _ := s.GetTask(ctx, task.ID)
	if !again.Duplicate || again.Request.ID != first.Request.ID || len(after.Events) != len(before.Events) {
		t.Fatalf("dup=%v id=%s events %d→%d", again.Duplicate, again.Request.ID, len(before.Events), len(after.Events))
	}
	resolve := DecisionResolveInput{RequestID: first.Request.ID, Kind: DecisionRequestAnswered, Option: "A"}
	if _, err := s.ResolveDecisionRequest(ctx, task.ID, "director-1", resolve); err != nil {
		t.Fatal(err)
	}
	dup, err := s.ResolveDecisionRequest(ctx, task.ID, "director-1", resolve)
	if err != nil || !dup.Duplicate {
		t.Fatalf("resolution retry dup=%v err=%v", dup.Duplicate, err)
	}
	resolve.Option = "B"
	if _, err := s.ResolveDecisionRequest(ctx, task.ID, "director-1", resolve); !errors.Is(err, ErrDecisionRequestResolved) {
		t.Fatalf("second answer: %v", err)
	}
	// A retry of the (now answered) request is still that request — it does
	// not re-open the question under a new id.
	retry, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", decisionInput())
	if err != nil || !retry.Duplicate || retry.Request.ID != first.Request.ID || retry.Request.Status != DecisionRequestAnswered {
		t.Fatalf("retry after answer=%+v err=%v", retry, err)
	}
}

// A4: requests are listed whatever the task state; an open request on a
// merged task is uncleaned, not answered, and closing it keeps its history.
// Its decision rows are never read as merges.
func TestDecisionRequestStateIndependentAndUncleaned(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	backlog := decisionTask(t, s, "backlog")
	if _, err := s.RecordDecisionRequest(ctx, backlog.ID, "director-1", decisionInput()); err != nil {
		t.Fatal(err)
	}
	work := decisionTask(t, s, "in_progress")
	open, err := s.RecordDecisionRequest(ctx, work.ID, "director-1", decisionInput())
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"verifying", "merged"} {
		if _, err := s.TransitionTask(ctx, work.ID, to, "dr-test", "step", nil); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListOpenDecisionRequests(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("open list=%d", len(list))
	}
	if pending, uncleaned := DecisionRequestCounts(list); pending != 1 || uncleaned != 1 {
		t.Fatalf("pending=%d uncleaned=%d", pending, uncleaned)
	}
	merged, _, _ := s.GetTask(ctx, work.ID)
	if state := DecisionRequestState(*merged.Refs.DecisionRequest, merged.State, false, time.Now()); state != DecisionViewUncleaned {
		t.Fatalf("merged open state=%s", state)
	}
	decisions, _, err := s.CountNeedsDecision(ctx)
	if err != nil || decisions != 1 {
		t.Fatalf("CountNeedsDecision=%d err=%v (backlog request pending; merged one is uncleaned)", decisions, err)
	}
	if _, err := s.RecordDecisionRequest(ctx, work.ID, "director-1", func() DecisionRequestInput {
		in := decisionInput()
		in.Question = "new"
		in.Supersedes = open.Request.ID
		return in
	}()); !errors.Is(err, ErrTaskTerminal) {
		t.Fatalf("new request on merged task: %v", err)
	}
	closed, err := s.ResolveDecisionRequest(ctx, work.ID, "director-1", DecisionResolveInput{RequestID: open.Request.ID, Kind: DecisionRequestWithdrawn, Text: "머지로 무의미해짐"})
	if err != nil || closed.Task.State != "merged" {
		t.Fatalf("withdraw on merged: %+v %v", closed.Task.State, err)
	}
	full, _, _ := s.GetTask(ctx, work.ID)
	history := DecisionRequestHistory(full)
	if len(history) != 1 || history[0].Request.Status != DecisionRequestWithdrawn || history[0].Request.Resolution.Text != "머지로 무의미해짐" {
		t.Fatalf("history=%+v", history)
	}
	merges, err := s.ListMergedTaskEvents(ctx, 2000)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, m := range merges {
		if m.TaskID == work.ID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("merge events for task=%d, want 1 (decision rows are not merges)", count)
	}
	after, _ := s.ListOpenDecisionRequests(ctx, 0)
	if len(after) != 1 || after[0].ID != backlog.ID {
		t.Fatalf("after close=%+v", after)
	}
}

// A blocking request moves the task to needs_decision in the same write and
// is listed once: as a request, not also as a generic task decision.
func TestDecisionRequestBlockListsOnce(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	task := decisionTask(t, s, "in_progress")
	in := decisionInput()
	in.Block = true
	got, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Task.State != "needs_decision" {
		t.Fatalf("state=%s", got.Task.State)
	}
	full, _, _ := s.GetTask(ctx, task.ID)
	last := full.Events[len(full.Events)-1]
	if last.Kind != TaskEventTransition || last.From != "in_progress" || last.To != "needs_decision" || last.Note != in.Question || last.Refs.DecisionRequest.ID != got.Request.ID {
		t.Fatalf("block event=%+v", last)
	}
	legacy, err := s.ListOpenTaskDecisions(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range legacy {
		if d.Task.ID == task.ID {
			t.Fatalf("open request also listed as a generic task decision")
		}
	}
	decisions, _, _ := s.CountNeedsDecision(ctx)
	if decisions != 1 {
		t.Fatalf("CountNeedsDecision=%d, want 1", decisions)
	}
	// The generic paths can no longer re-ask or re-option it.
	if _, err := s.TransitionTask(ctx, task.ID, "hold", "dr-test", "park", &TaskRefs{DecisionOptions: &DecisionOptions{Options: []DecisionOption{{Key: "A", Label: "x"}}}}); !errors.Is(err, ErrDecisionRequestOpen) {
		t.Fatalf("option patch: %v", err)
	}
	if _, err := s.TransitionTask(ctx, task.ID, "hold", "dr-test", "park", &TaskRefs{DecisionRequest: &DecisionRequest{ID: "dr-1-9"}}); err == nil {
		t.Fatalf("decision_request patch accepted")
	}
	if _, err := s.TransitionTask(ctx, task.ID, "hold", "dr-test", "park", nil); err != nil {
		t.Fatalf("plain transition: %v", err)
	}
	if _, err := s.TransitionTask(ctx, task.ID, "needs_decision", "dr-test", "another question", nil); !errors.Is(err, ErrDecisionRequestOpen) {
		t.Fatalf("generic needs_decision while open: %v", err)
	}
	if _, err := s.CreateTask(ctx, Task{Lane: "b618", Title: "x", Kind: "implement", CreatedBy: "dr-test", Refs: TaskRefs{DecisionRequest: &DecisionRequest{ID: "dr-1-1"}}}); err == nil {
		t.Fatalf("create with decision_request accepted")
	}
}

// Rejections: zero options, unknown task, missing default action, default
// option outside the set, and the 120-BYTE label limit with multibyte text.
func TestDecisionRequestRejections(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	task := decisionTask(t, s, "in_progress")
	cases := map[string]func(*DecisionRequestInput){
		"zero options":     func(in *DecisionRequestInput) { in.Options.Options = nil },
		"no default":       func(in *DecisionRequestInput) { in.DefaultAction = "" },
		"default not key":  func(in *DecisionRequestInput) { in.DefaultOption = "F" },
		"two recommended":  func(in *DecisionRequestInput) { in.Options.Options[1].Recommended = true },
		"duplicate key":    func(in *DecisionRequestInput) { in.Options.Options[1].Key = "A" },
		"no question":      func(in *DecisionRequestInput) { in.Question = " " },
		"41 hangul label":  func(in *DecisionRequestInput) { in.Options.Options[0].Label = strings.Repeat("가", 41) },
		"121 byte mixed":   func(in *DecisionRequestInput) { in.Options.Options[0].Label = strings.Repeat("가", 39) + "abcd" },
		"multiline action": func(in *DecisionRequestInput) { in.DefaultAction = "보류\n다음" },
	}
	for name, mutate := range cases {
		in := decisionInput()
		mutate(&in)
		if _, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", in); !errors.Is(err, ErrInvalidDecisionRequest) {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	in := decisionInput()
	in.Options.Options[0].Label = strings.Repeat("가", 41)
	if err := ValidateDecisionRequestInput(in); err == nil || !strings.Contains(err.Error(), "123 bytes") || !strings.Contains(err.Error(), "120 bytes (not characters)") {
		t.Fatalf("byte-limit message=%v", err)
	}
	if _, err := s.RecordDecisionRequest(ctx, 987654321, "director-1", decisionInput()); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("unknown task: %v", err)
	}
	if _, err := s.RecordDecisionRequest(ctx, 0, "director-1", decisionInput()); !errors.Is(err, ErrInvalidDecisionRequest) {
		t.Fatalf("task id 0: %v", err)
	}
	// Exactly 120 bytes is accepted: 40 Hangul syllables, or 39 + 3 ASCII.
	for _, label := range []string{strings.Repeat("가", 40), strings.Repeat("가", 39) + "abc"} {
		in := decisionInput()
		in.Options.Options[0].Label = label
		if len(label) != 120 {
			t.Fatalf("fixture is %d bytes", len(label))
		}
		if err := ValidateDecisionRequestInput(in); err != nil {
			t.Fatalf("120-byte label refused: %v", err)
		}
	}
	good := decisionInput()
	good.Options.Options[0].Label = strings.Repeat("가", 40)
	if _, err := s.RecordDecisionRequest(ctx, task.ID, "director-1", good); err != nil {
		t.Fatalf("120-byte label: %v", err)
	}
	// Resolution rules.
	id := "dr-" + strconv.FormatInt(task.ID, 10) + "-1"
	for name, in := range map[string]DecisionResolveInput{
		"unknown option":     {RequestID: id, Kind: DecisionRequestAnswered, Option: "F"},
		"empty answer":       {RequestID: id, Kind: DecisionRequestAnswered},
		"withdraw no reason": {RequestID: id, Kind: DecisionRequestWithdrawn},
		"bad kind":           {RequestID: id, Kind: "applied"},
	} {
		if _, err := s.ResolveDecisionRequest(ctx, task.ID, "director-1", in); !errors.Is(err, ErrInvalidDecisionRequest) {
			t.Errorf("%s: err=%v", name, err)
		}
	}
	if _, err := s.ResolveDecisionRequest(ctx, 987654321, "director-1", DecisionResolveInput{RequestID: id, Kind: DecisionRequestAnswered, Option: "A"}); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("resolve unknown task: %v", err)
	}
}
