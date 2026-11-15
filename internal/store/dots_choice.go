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
	return c.ResultDoc == "" || validDocKey(c.ResultDoc)
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

// deliverDotsChoiceAnswerTx finishes a declaring dots request whose RESULT
// decision was closed: the answer is the request's result, so the task
// completes with the decision and the answer as its evidence, and the answer
// reaches the requester lane (and the request body's "[알릴 레인]" lanes) once
// per resolution. Both answer paths call it inside their own transaction, so a
// replayed answer writes nothing. A task whose decision is not the one its
// RESULT opened is left alone.
func (s *Store) deliverDotsChoiceAnswerTx(ctx context.Context, tx pgx.Tx, x Task, resolved DecisionRequest, choice, actor string, resolutionID int64, at time.Time) (Task, error) {
	declared := x.Refs.DotsChoice
	if declared == nil || declared.ResultDoc == "" || resolved.Doc != declared.ResultDoc {
		return x, nil
	}
	// Only an answer is the request's result: a withdrawn or default_applied
	// resolution closes the request without a choice, so nothing is delivered
	// and the request stays open for a replacement RESULT. A task already
	// settled (dropped, duplicated, merged) keeps its closure — the answer is
	// still recorded, as cleanup, but must not fail the resolve.
	if resolved.Status != DecisionRequestAnswered || IsSettledState(x.State) {
		return x, nil
	}
	// The answer resumes a parked request: its decision is closed, so the
	// request may close too. Without this an icebox task has no edge to
	// completed and the answer would roll back.
	if x.State == StateIcebox {
		resumed, err := s.transitionTaskTx(ctx, tx, x.ID, TransitionInput{To: StateBacklog, By: actor, Note: "decision answered: resume to close"}, false)
		if err != nil {
			return Task{}, err
		}
		x = resumed
	}
	answer := choice
	if resolved.Resolution != nil && resolved.Resolution.Text != "" && resolved.Resolution.Text != choice {
		answer = choice + " — " + resolved.Resolution.Text
	}
	evidence := "decision " + resolved.ID + " " + resolved.Status + ": "
	evidence += clipBytes(answer, closureEvidenceMaxBytes-len(evidence))
	lanes := []string{}
	if x.ParentLane != "" {
		lanes = append(lanes, x.ParentLane)
	}
	notified, _, err := dotsNotifyLanesTx(ctx, tx, x)
	if err != nil {
		return Task{}, err
	}
	lanes = append(lanes, notified...)
	text := dotsLaneText("[dots] RESULT hk-" + strconv.FormatInt(x.ID, 10) + " " + firstRunes(x.Title, 60) + " :: 결정 " + resolved.ID + ": " + firstRunes(answer, 220) + " → hk:doc " + declared.ResultDoc)
	if base := s.decisionConsoleURL(); base != "" {
		text += " | " + base + "/ui/tasks/" + strconv.FormatInt(x.ID, 10)
	}
	baseEvent := "dots-answer-" + resolved.ID + "-" + strconv.FormatInt(resolutionID, 10)
	seen := map[string]bool{}
	for _, lane := range lanes {
		if lane == "" || seen[lane] {
			continue
		}
		seen[lane] = true
		eventID := baseEvent
		if lane != x.ParentLane {
			eventID += "-" + lane
		}
		if _, err := insertNotificationOutboxOnce(ctx, tx, OutboxKindDotsReport, lane, eventID, text, at); err != nil {
			return Task{}, err
		}
	}
	// A task still waiting on another blocking decision, or in a state whose
	// edges do not reach completed, cannot close now: the answer is recorded
	// and delivered, and the closure stays with whatever resolves that state.
	if x.AwaitingDecision || !taskTransitionAllowed(x.State, StateCompleted) {
		return x, nil
	}
	completed, err := s.transitionTaskTx(ctx, tx, x.ID, TransitionInput{To: StateCompleted, By: actor, Evidence: evidence}, false)
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
