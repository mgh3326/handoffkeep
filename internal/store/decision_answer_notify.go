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
// decision event and the notice together. The lane is the request's
// requested_by when that names a lane, else the task's lane.
func (s *Store) queueDecisionAnswerNotice(ctx context.Context, tx pgx.Tx, x Task, resolved DecisionRequest, in ResolveDecisionInput, decider string, resolutionID int64, at time.Time) error {
	lane := x.Lane
	if validName(resolved.RequestedBy) {
		lane = resolved.RequestedBy
	}
	taskID := strconv.FormatInt(x.ID, 10)
	prefix := "[hk] 결정 답변 " + resolved.ID + " task " + taskID + ": "
	body := decisionAnswerChoice(x.Refs.DecisionOptions, in, resolved.Status) + " by " + decider + " via " + in.Channel
	if base := s.decisionConsoleURL(); base != "" {
		body += " | " + base + "/ui/tasks/" + taskID
	}
	_, err := insertNotificationOutboxOnce(ctx, tx, OutboxKindDecisionAnswered, lane, decisionAnswerNoticeEventID(resolved.ID, resolutionID), assistantLaneText(prefix, body), at)
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
