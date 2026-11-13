package store

import (
	"context"
	"strconv"
	"time"

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
	choice := decisionAnswerChoice(x.Refs.DecisionOptions, in, resolved.Status)
	if resolved.Status != DecisionRequestAnswered && choice != resolved.Status {
		// The pointer carries the state: a withdrawn or default_applied
		// request says so, whatever its text.
		choice = resolved.Status + ": " + choice
	}
	body := choice + " by " + decider + " via " + in.Channel
	if base := s.decisionConsoleURL(); base != "" {
		body += " | " + base + "/ui/tasks/" + taskID
	}
	_, err := insertNotificationOutboxOnce(ctx, tx, OutboxKindDecisionAnswered, lane, decisionAnswerNoticeEventID(resolved.ID, resolutionID), assistantLaneText(prefix, body), at)
	return err
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
	_, err := insertNotificationOutboxOnce(ctx, tx, OutboxKindDecisionDenied, x.Lane, decisionDenyNoticeEventID(request.ID, denyAttemptID), assistantLaneText(prefix, body), at)
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
