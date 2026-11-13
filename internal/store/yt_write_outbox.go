package store

import (
	"context"
	"strings"
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

// YTWriteOutboxStatus reads the open outbox at now in one statement, so every
// state is read from the same snapshot: an op moving between states during the
// read is counted once, in one state, never in two or none. The statement is
// one UNION ALL with one branch per open state. Each branch has a literal state
// predicate, so it still uses its partial index (yt_write_ops_due,
// yt_write_ops_dispatched) rather than scanning the applied history.
func (s *Store) YTWriteOutboxStatus(ctx context.Context, now time.Time) (YTWriteOutbox, error) {
	branches := make([]string, 0, len(YTWriteOutboxStates))
	for _, st := range YTWriteOutboxStates {
		branches = append(branches, `SELECT '`+string(st)+`'::text, count(*),
			min(CASE WHEN state='dispatched' THEN dispatched_at ELSE created_at END),
			count(*) FILTER (WHERE attempts >= 2), count(*) FILTER (WHERE reconciles >= 1)
			FROM yt_write_ops WHERE state='`+string(st)+`'`)
	}
	rows, err := s.pool.Query(ctx, strings.Join(branches, " UNION ALL "))
	if err != nil {
		return YTWriteOutbox{}, err
	}
	defer rows.Close()
	now = now.UTC()
	out := YTWriteOutbox{Open: map[ytwrite.State]int64{}, OldestAge: map[ytwrite.State]time.Duration{}}
	for rows.Next() {
		var state string
		var n, retrying, reconciling int64
		var since *time.Time
		if err := rows.Scan(&state, &n, &since, &retrying, &reconciling); err != nil {
			return YTWriteOutbox{}, err
		}
		st := ytwrite.State(state)
		out.Open[st] = n
		if since != nil {
			out.OldestAge[st] = max(now.Sub(since.UTC()), 0)
		}
		out.Retrying += retrying
		out.Reconciling += reconciling
	}
	if err := rows.Err(); err != nil {
		return YTWriteOutbox{}, err
	}
	return out, nil
}
