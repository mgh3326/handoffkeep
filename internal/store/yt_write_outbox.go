package store

import (
	"context"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// YTWriteOutboxStates are the states an op can be open in, in report order.
var YTWriteOutboxStates = []ytwrite.State{ytwrite.StatePrepared, ytwrite.StateDispatched, ytwrite.StateRemoteUnknown}

// YTWriteOutbox is the open part of the YouTrack write outbox at one instant:
// the ops not yet applied or rejected. GET /metrics reports it.
type YTWriteOutbox struct {
	// Open counts the open ops of each state.
	Open map[ytwrite.State]int64
	// OldestAge is how long the oldest open op of each state has waited: from
	// created_at, or from dispatched_at for a dispatched op. A state with no
	// open op has no entry.
	OldestAge map[ytwrite.State]time.Duration
	// Retrying counts open ops sent more than once (attempts >= 2).
	Retrying int64
	// Reconciling counts open ops whose outcome was checked again (reconciles >= 1).
	Reconciling int64
}

// YTWriteOutboxStatus reads the open outbox at now. It runs one query per open
// state, each matching a partial index (yt_write_ops_due, yt_write_ops_dispatched),
// so a scrape does not scan the applied history. The state is a literal in the
// SQL for the same reason as ytWriteOpAlertCandidates: a generic plan must still
// use the partial index.
func (s *Store) YTWriteOutboxStatus(ctx context.Context, now time.Time) (YTWriteOutbox, error) {
	out := YTWriteOutbox{Open: map[ytwrite.State]int64{}, OldestAge: map[ytwrite.State]time.Duration{}}
	now = now.UTC()
	for _, st := range YTWriteOutboxStates {
		var n, retrying, reconciling int64
		var since *time.Time
		q := `SELECT count(*), min(CASE WHEN state='dispatched' THEN dispatched_at ELSE created_at END),
			count(*) FILTER (WHERE attempts >= 2), count(*) FILTER (WHERE reconciles >= 1)
			FROM yt_write_ops WHERE state='` + string(st) + `'`
		if err := s.pool.QueryRow(ctx, q).Scan(&n, &since, &retrying, &reconciling); err != nil {
			return YTWriteOutbox{}, err
		}
		out.Open[st] = n
		if since != nil {
			out.OldestAge[st] = max(now.Sub(since.UTC()), 0)
		}
		out.Retrying += retrying
		out.Reconciling += reconciling
	}
	return out, nil
}
