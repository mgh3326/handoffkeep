package store

import (
	"context"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// decisionAnswerNoticeEventID is the outbox id of one answered decision
// request. It names the resolution, so each answer owns exactly one row: a
// replayed resolve returns before any write, and a drained retry re-sends
// the same id for hub-side dedupe.
func decisionAnswerNoticeEventID(requestID string, resolutionID int64) string {
	return "decision-answered-" + requestID + "-" + strconv.FormatInt(resolutionID, 10)
}

// decisionDenyNoticeEventID is the outbox id of one deny. It names the deny
// attempt, which is written at most once per pending attempt, so a replayed
// deny returns before any write and owns no second row.
func decisionDenyNoticeEventID(requestID string, denyAttemptID int64) string {
	return "decision-denied-" + requestID + "-" + strconv.FormatInt(denyAttemptID, 10)
}

// SetConsoleURL sets the console base URL the answer notification links to.
// Empty leaves the link out.
func (s *Store) SetConsoleURL(base string) {
	s.consoleURL.Store(&base)
}

func (s *Store) decisionConsoleURL() string {
	if p := s.consoleURL.Load(); p != nil {
		return *p
	}
	return ""
}

// queueDecisionAnswerNotice writes the lane notification for one successful
// resolution, inside the resolve transaction, so a rollback takes the
// decision event and the notice together. The lane is the task's lane: the
// request's requested_by is the filing client's identity, not a hub lane, so
// routing to it would post to a lane that may not exist.
func (s *Store) queueDecisionAnswerNotice(ctx context.Context, tx pgx.Tx, x Task, resolved DecisionRequest, in ResolveDecisionInput, decider string, resolutionID int64, at time.Time) error {
	lane := x.Lane
	taskID := strconv.FormatInt(x.ID, 10)
	prefix := "[hk] 결정 답변 " + resolved.ID + " task " + taskID + ": "
	choice := decisionAnswerNoticeChoice(x.Refs.DecisionOptions, in, resolved.Status)
	if resolved.Status != DecisionRequestAnswered && choice != resolved.Status {
		// The pointer carries the state: a withdrawn or default_applied
		// request says so, whatever its text.
		choice = resolved.Status + ": " + choice
	}
	body := choice + " by " + decider + " via " + in.Channel
	if base := s.decisionConsoleURL(); base != "" {
		body += " | " + base + "/ui/tasks/" + taskID
	}
	_, err := insertNotificationOutboxOnce(ctx, pgTransaction{tx}, OutboxKindDecisionAnswered, lane, decisionAnswerNoticeEventID(resolved.ID, resolutionID), assistantLaneText(prefix, body), at)
	if err != nil {
		return err
	}
	asker, ok := decisionAskerNoticeLane(lane, resolved, in)
	if !ok {
		return nil
	}
	_, err = insertNotificationOutboxOnce(ctx, pgTransaction{tx}, OutboxKindDecisionAnswered, asker, decisionAskerAnswerNoticeEventID(resolved.ID, resolutionID), assistantLaneText(prefix, body), at)
	return err
}

// decisionAskerNoticeLane names the lane that gets a second answer notice:
// the lane that asked, when it differs from the task's lane. The task lane
// keeps its notice either way. A session answer that the asker lane itself
// recorded gets no second notice: the asker's session saw the answer it
// wrote (hk 5527). Console and other channels, and a session answer recorded
// by another lane, keep the asker's copy.
func decisionAskerNoticeLane(taskLane string, resolved DecisionRequest, in ResolveDecisionInput) (string, bool) {
	if resolved.RequestLane == "" || resolved.RequestLane == taskLane {
		return "", false
	}
	if in.Channel == DecisionChannelSession && in.RecordingLane == resolved.RequestLane {
		return "", false
	}
	return resolved.RequestLane, true
}

// decisionAskerAnswerNoticeEventID is the outbox id of the asker's copy of an
// answer. It is distinct from the task lane's id, so both rows can coexist.
func decisionAskerAnswerNoticeEventID(requestID string, resolutionID int64) string {
	return decisionAnswerNoticeEventID(requestID, resolutionID) + "-asker"
}

// queueDecisionDenyNotice writes the lane notification for one operator deny,
// inside the deny transaction. A deny closes nothing: the pointer names the
// request, the denied attempt and the state the request stays in (open), so
// the asking lane learns its relayed answer was refused and must wait for a
// verified one.
func (s *Store) queueDecisionDenyNotice(ctx context.Context, tx pgx.Tx, x Task, request DecisionRequest, in ResolveDecisionInput, denyAttemptID int64, at time.Time) error {
	taskID := strconv.FormatInt(x.ID, 10)
	prefix := "[hk] 결정 거부 " + request.ID + " task " + taskID + ": "
	body := "attempt " + strconv.FormatInt(in.DeniesAttemptID, 10) + " denied by " + in.Actor + " via " + in.Channel + "; request stays " + request.Status
	if in.Receipt != "" {
		body += " | " + in.Receipt
	}
	if base := s.decisionConsoleURL(); base != "" {
		body += " | " + base + "/ui/tasks/" + taskID
	}
	_, err := insertNotificationOutboxOnce(ctx, pgTransaction{tx}, OutboxKindDecisionDenied, x.Lane, decisionDenyNoticeEventID(request.ID, denyAttemptID), assistantLaneText(prefix, body), at)
	return err
}

// decisionAnswerChoice names what was chosen: the option key with its label
// when the request defines one, else the free text, else the resolution kind.
func decisionAnswerChoice(options *DecisionOptions, in ResolveDecisionInput, kind string) string {
	if in.Option == "" {
		if in.Text != "" {
			return in.Text
		}
		return kind
	}
	if options != nil {
		for _, o := range options.Options {
			if o.Key == in.Option && o.Label != "" {
				return in.Option + " " + o.Label
			}
		}
	}
	return in.Option
}

// decisionAnswerNoticeTextMaxRunes bounds the free text quoted in an answer
// notice; the full text stays in the resolution record behind the link.
const decisionAnswerNoticeTextMaxRunes = 200

// decisionAnswerNoticeFullTextNote marks a clipped quote: the notice keeps
// the link to the task, and the record behind it keeps the full text.
const decisionAnswerNoticeFullTextNote = " (full text in record)"

// decisionAnswerNoticeChoice names what was chosen for the lane notice: the
// option choice, plus the free text when the answer carries any, after the
// option. A clipped text carries "…" and the full-text note. An option-only
// answer is just the choice.
func decisionAnswerNoticeChoice(options *DecisionOptions, in ResolveDecisionInput, kind string) string {
	choice := decisionAnswerChoice(options, in, kind)
	if in.Text == "" {
		return choice
	}
	clipped, wasClipped := clipAnswerNoticeText(in.Text)
	note := ""
	if wasClipped {
		note = decisionAnswerNoticeFullTextNote
	}
	if in.Option == "" {
		return clipped + note
	}
	return choice + " — " + clipped + note
}

// clipAnswerNoticeText cuts s to decisionAnswerNoticeTextMaxRunes code
// points, marking the cut with "…", and reports whether it cut.
func clipAnswerNoticeText(s string) (string, bool) {
	if utf8.RuneCountInString(s) <= decisionAnswerNoticeTextMaxRunes {
		return s, false
	}
	return string([]rune(s)[:decisionAnswerNoticeTextMaxRunes]) + "…", true
}
