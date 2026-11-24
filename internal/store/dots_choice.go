package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

// A dots request may declare that it ends in the operator's choice (hk 4789,
// operator decision 303). The declaration is written at creation, so a RESULT
// is never refused for it: the options are validated with the decision model's
// own rules and every attachment address is checked against this database
// before the request exists. The declaration lives in tasks.refs.dots_choice,
// beside the task's other refs, so it survives blue-green and needs no table.
//
// The flow: RESULT records the result as always and opens the operator's
// decision from the declaration (dots marks its recommendation and supplies
// its reason, its unverified list and its one fix); the task then waits on
// that decision instead of completing. The answer, from the console page or
// through decision_answer, closes the one decision record, completes the task
// with the decision and the answer as evidence, and reaches the requester lane
// as the request's result.

// DotsChoice is a dots request's declared choice.
type DotsChoice struct {
	Options []DecisionOption `json:"options"`
	// ResultDoc is the RESULT document the operator's decision was opened
	// from. It is set in the RESULT transaction; a QUESTION asked about the
	// same request leaves it empty, so answering that question neither
	// completes the task nor reaches the requester lane.
	ResultDoc string `json:"result_doc,omitempty"`
}

// validDotsChoice reports whether a stored declaration is well-formed: the
// decision option shape, with no recommendation (dots names one on RESULT).
func validDotsChoice(c DotsChoice) bool {
	for _, option := range c.Options {
		if option.Recommended {
			return false
		}
	}
	if !validDecisionOptions(DecisionOptions{Options: c.Options}) {
		return false
	}
	return c.ResultDoc == "" || ValidDocKey(c.ResultDoc)
}

// CheckDotsChoice checks a request's declared options before anything is
// written. An empty list is no declaration and passes. It reuses the decision
// request's option rules, so a declared option is exactly an option the
// operator's page can render.
func CheckDotsChoice(options []DecisionOption) error {
	if len(options) == 0 {
		return nil
	}
	for _, option := range options {
		if option.Recommended {
			return errors.New("dots request options take no recommended flag: dots names its recommendation on RESULT")
		}
	}
	return CheckDecisionOptionsDetailed(DecisionOptions{Options: options})
}

// rejectDotsChoiceSecrets refuses a declaration carrying secret-like text. The
// labels and captions are shown to the operator, so they follow the same guard
// as the decision request they later become.
func rejectDotsChoiceSecrets(options []DecisionOption) error {
	values := []string{}
	for _, option := range options {
		values = append(values, option.Label)
		for _, attachment := range option.Attachments {
			values = append(values, attachment.Caption)
		}
	}
	return guard.Reject(strings.Join(values, "\n"))
}

// checkDotsReplyAnnotations checks the RESULT-only fields dots may supply: an
// option key, its reason, the unverified list and the one fix, with the same
// bounds the decision request enforces for them.
func checkDotsReplyAnnotations(in DotsReplyInput) error {
	if in.Recommended != "" && !validDecisionOptionKey(in.Recommended) {
		return errors.New("invalid dots reply: recommended must be an option key A-F")
	}
	if in.RecommendationReason != "" && (!validText(in.RecommendationReason, DecisionRequestRecommendationReasonMaxBytes) || !utf8.ValidString(in.RecommendationReason) || strings.TrimSpace(in.RecommendationReason) == "") {
		return fmt.Errorf("invalid dots reply: recommendation_reason must be 1..%d bytes of valid text", DecisionRequestRecommendationReasonMaxBytes)
	}
	if in.OneFix != "" && (!validText(in.OneFix, DecisionRequestOneFixMaxBytes) || !utf8.ValidString(in.OneFix) || strings.TrimSpace(in.OneFix) == "") {
		return fmt.Errorf("invalid dots reply: one_fix must be 1..%d bytes of valid text", DecisionRequestOneFixMaxBytes)
	}
	if len(in.Unverified) > DecisionRequestMaxUnverified {
		return fmt.Errorf("invalid dots reply: at most %d unverified items are allowed", DecisionRequestMaxUnverified)
	}
	for _, item := range in.Unverified {
		if !validDecisionLine(item, DecisionRequestUnverifiedMaxBytes) {
			return fmt.Errorf("invalid dots reply: each unverified item must be one line of at most %d bytes", DecisionRequestUnverifiedMaxBytes)
		}
	}
	values := append([]string{in.Recommended, in.RecommendationReason, in.OneFix}, in.Unverified...)
	return guard.Reject(strings.Join(values, "\n"))
}

// markDotsChoiceResultTx records the RESULT document whose answer is the
// request's result. It runs in the RESULT transaction, after the decision was
// opened, and only rewrites refs.dots_choice.
func markDotsChoiceResultTx(ctx context.Context, tx pgx.Tx, taskID int64, docKey string) error {
	_, err := tx.Exec(ctx, `UPDATE tasks SET refs=jsonb_set(refs,'{dots_choice,result_doc}',to_jsonb($2::text)) WHERE id=$1`, taskID, docKey)
	return err
}

// dotsChoiceDecisionInput builds the operator's decision for a declaring
// request's RESULT. The options are the declaration, with recommended marking
// dots's choice; a free answer is allowed because the operator may answer in
// the dots chat in their own words. recommended must name a declared key.
func dotsChoiceDecisionInput(task Task, docKey string, in DotsReplyInput) (DecisionRequestInput, error) {
	declared := task.Refs.DotsChoice.Options
	options := make([]DecisionOption, len(declared))
	copy(options, declared)
	found := in.Recommended == ""
	for i := range options {
		if in.Recommended != "" && options[i].Key == in.Recommended {
			options[i].Recommended = true
			found = true
		}
	}
	if !found {
		return DecisionRequestInput{}, fmt.Errorf("dots reply: recommended %q is not a declared option key", in.Recommended)
	}
	input := DecisionRequestInput{
		Question:             "[dots] " + task.Title + " — 어느 쪽으로 할까요?",
		Options:              DecisionOptions{Options: options, AllowFree: true},
		RecommendationReason: in.RecommendationReason,
		Unverified:           in.Unverified,
		OneFix:               in.OneFix,
		DefaultAction:        "자동 적용 없음",
		Doc:                  docKey,
		Block:                true,
	}
	// The RESULT is the request's last word: a question still open on the
	// same task is superseded rather than left beside the choice.
	if current := task.Refs.DecisionRequest; current != nil {
		input.Supersedes = current.ID
	}
	return input, nil
}

// reconcileDotsChoiceTx finishes a declaring dots request whose RESULT
// decision was answered: the answer is the request's result, so the task
// completes with the decision and the answer as its evidence, and the answer
// reaches the requester lane (and the request body's "[알릴 레인]" lanes) once
// per decision. It runs at the end of every resolving transaction, because the
// answer may land while another blocking decision still keeps the task open:
// nothing is done then, and the reconcile runs again when that decision
// closes. Outbox ids name the decision, so a second run sends nothing.
func (s *Store) reconcileDotsChoiceTx(ctx context.Context, tx transaction, x Task, at time.Time) (Task, error) {
	declared := x.Refs.DotsChoice
	if declared == nil || declared.ResultDoc == "" {
		return x, nil
	}
	request := x.Refs.DecisionRequest
	if request == nil || request.Status != DecisionRequestAnswered || request.Doc != declared.ResultDoc || request.Resolution == nil {
		return x, nil
	}
	// One verdict before any side effect. A settled task keeps its closure (the
	// answer is recorded as cleanup — the console's cleanup path), and a task
	// still waiting on another blocking decision cannot close now.
	if IsSettledState(x.State) || x.AwaitingDecision {
		return x, nil
	}
	// Verdict made: every action below is unconditional for this call. A parked
	// request resumes first, because icebox has no edge to completed. The
	// recorder of the resolution is this task's actor.
	by := request.Resolution.By
	if x.State == StateIcebox {
		resumed, err := s.transitionTaskTx(ctx, tx, x.ID, TransitionInput{To: StateBacklog, By: by, Note: "decision answered: resume to close"}, false)
		if err != nil {
			return Task{}, err
		}
		x = resumed
	}
	answer := decisionAnswerChoice(x.Refs.DecisionOptions, ResolveDecisionInput{Option: request.Resolution.Option, Text: request.Resolution.Text}, request.Status)
	if request.Resolution.Text != "" && request.Resolution.Text != answer {
		answer += " — " + request.Resolution.Text
	}
	evidence := "decision " + request.ID + " " + request.Status + ": "
	evidence += clipBytes(answer, closureEvidenceMaxBytes-len(evidence))
	lanes := []string{}
	if x.ParentLane != "" {
		lanes = append(lanes, x.ParentLane)
	}
	notified, _, err := dotsNotifyLanesFlowTx(ctx, tx, x)
	if err != nil {
		return Task{}, err
	}
	lanes = append(lanes, notified...)
	text := dotsLaneText("[dots] RESULT hk-" + strconv.FormatInt(x.ID, 10) + " " + firstRunes(x.Title, 60) + " :: 결정 " + request.ID + ": " + firstRunes(answer, 220) + " → hk:doc " + declared.ResultDoc)
	if base := s.decisionConsoleURL(); base != "" {
		text += " | " + base + "/ui/tasks/" + strconv.FormatInt(x.ID, 10)
	}
	seen := map[string]bool{}
	for _, lane := range lanes {
		if lane == "" || seen[lane] {
			continue
		}
		seen[lane] = true
		eventID := "dots-answer-" + request.ID
		if lane != x.ParentLane {
			eventID += "-" + lane
		}
		if _, err := insertNotificationOutboxOnce(ctx, tx, OutboxKindDotsReport, lane, eventID, text, at); err != nil {
			return Task{}, err
		}
	}
	if !taskTransitionAllowed(x.State, StateCompleted) {
		return x, nil
	}
	completed, err := s.transitionTaskTx(ctx, tx, x.ID, TransitionInput{To: StateCompleted, By: by, Evidence: evidence}, false)
	if err != nil {
		return Task{}, err
	}
	return completed, nil
}

// clipBytes cuts s to at most n bytes without splitting a UTF-8 rune.
func clipBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
