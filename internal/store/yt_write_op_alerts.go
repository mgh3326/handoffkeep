package store

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

const (
	// ytWriteOpAlertDelay is how long an op may stay prepared or dispatched
	// before it is reported as stuck.
	ytWriteOpAlertDelay = 10 * time.Minute
	// ytWriteOpAlertWindow is the batch: at most one notice per class per
	// window, keyed by the window's start.
	ytWriteOpAlertWindow = 10 * time.Minute
	// ytWriteOpAlertBatchMax bounds the ops one notice covers. Ops beyond it
	// stay unmarked and go in a later window.
	ytWriteOpAlertBatchMax = 500
	// ytWriteOpAlertIDsShown bounds the op ids printed in one notice.
	ytWriteOpAlertIDsShown = 50
)

const (
	ytWriteOpAlertStuck    = "stuck"
	ytWriteOpAlertRejected = "rejected"
)

// ytWriteOpAlertPointer is the command an operator runs to see the ops a notice
// names.
var ytWriteOpAlertPointer = map[string]string{
	ytWriteOpAlertStuck:    "handoffkeep yt ops",
	ytWriteOpAlertRejected: "handoffkeep yt ops --state rejected",
}

// EnqueueYTWriteOpAlerts records the lane notices owed for YouTrack write ops,
// batched: per class (stuck, rejected) and per 10-minute window, one outbox
// row covers every op of that class not yet announced. An op is announced
// once per class (yt_write_op_alerts). A window whose notice for a class
// already exists leaves its ops unmarked, so they go in the next window.
// Returns how many notices it wrote.
func (s *Store) EnqueueYTWriteOpAlerts(ctx context.Context, now time.Time) (int, error) {
	now = now.UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	window := now.Truncate(ytWriteOpAlertWindow)
	created := 0
	for _, class := range []string{ytWriteOpAlertStuck, ytWriteOpAlertRejected} {
		cands, err := ytWriteOpAlertCandidates(ctx, tx, class, now.Add(-ytWriteOpAlertDelay))
		if err != nil {
			return 0, err
		}
		if len(cands) == 0 {
			continue
		}
		eventID := fmt.Sprintf("yt-ops-%s-%d", class, window.Unix())
		inserted, err := insertNotificationOutboxOnce(ctx, tx, OutboxKindYTWriteOp, "handoffkeep-design", eventID, ytWriteOpAlertText(class, cands), now)
		if err != nil {
			return 0, err
		}
		if !inserted {
			continue
		}
		ids := make([]int64, len(cands))
		for i, c := range cands {
			ids[i] = c.id
		}
		if _, err := tx.Exec(ctx, `INSERT INTO yt_write_op_alerts(op_id, alert_class, created_at)
			SELECT x, $2::text, $3::timestamptz FROM unnest($1::bigint[]) AS x ON CONFLICT DO NOTHING`, ids, class, now); err != nil {
			return 0, err
		}
		created++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return created, nil
}

// ytWriteOpAlertCandidate is one op a notice covers, with the label its notice
// counts it under (the state for stuck ops, the reason for rejected ones).
type ytWriteOpAlertCandidate struct {
	id    int64
	label string
}

// ytWriteOpAlertCandidates lists the ops of class not yet announced, oldest
// first, up to the batch bound. The row lock keeps a pending alert from racing
// a concurrent resolution. A stuck op is prepared since before cutoff or
// dispatched since before cutoff. A rejected op is rejected, or applied with
// an unpublished binding (the operator's rejected view lists it too).
func ytWriteOpAlertCandidates(ctx context.Context, tx pgx.Tx, class string, cutoff time.Time) ([]ytWriteOpAlertCandidate, error) {
	// The binding-conflict reason is a literal, not a parameter, so a cached
	// generic plan can still use the partial index yt_write_ops_alert_scan.
	var q string
	switch class {
	case ytWriteOpAlertStuck:
		q = `SELECT o.id, o.state, o.reason FROM yt_write_ops o
			WHERE ((o.state='prepared' AND o.created_at <= $1) OR (o.state='dispatched' AND o.dispatched_at <= $1))
			AND NOT EXISTS (SELECT 1 FROM yt_write_op_alerts a WHERE a.op_id=o.id AND a.alert_class='stuck')
			ORDER BY o.id LIMIT $2 FOR SHARE OF o`
	case ytWriteOpAlertRejected:
		q = `SELECT o.id, o.state, o.reason FROM yt_write_ops o
			WHERE (o.state='rejected' OR (o.state='applied' AND o.reason='` + ytwrite.ReasonBindingConflict + `'))
			AND NOT EXISTS (SELECT 1 FROM yt_write_op_alerts a WHERE a.op_id=o.id AND a.alert_class='rejected')
			ORDER BY o.id LIMIT $2 FOR SHARE OF o`
	default:
		return nil, fmt.Errorf("unknown yt op alert class %q", class)
	}
	rows, err := tx.Query(ctx, q, cutoff.UTC(), ytWriteOpAlertBatchMax)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ytWriteOpAlertCandidate
	for rows.Next() {
		var id int64
		var state, reason string
		if err := rows.Scan(&id, &state, &reason); err != nil {
			return nil, err
		}
		label := state
		if class == ytWriteOpAlertRejected {
			label = reason
			if label == "" {
				label = "unspecified"
			}
		}
		out = append(out, ytWriteOpAlertCandidate{id: id, label: label})
	}
	return out, rows.Err()
}

// ytWriteOpAlertText is one batched notice:
// "[ytwrite] <n> <class> (<label>: <count>, …) op ids <ids> | <command>".
// It names at most ytWriteOpAlertIDsShown ids and stays within the lane-event
// byte cap.
func ytWriteOpAlertText(class string, cands []ytWriteOpAlertCandidate) string {
	counts := map[string]int{}
	for _, c := range cands {
		counts[c.label]++
	}
	labels := make([]string, 0, len(counts))
	for l := range counts {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	parts := make([]string, 0, len(labels))
	for _, l := range labels {
		parts = append(parts, fmt.Sprintf("%s: %d", l, counts[l]))
	}
	shown := min(len(cands), ytWriteOpAlertIDsShown)
	ids := make([]string, 0, shown)
	for _, c := range cands[:shown] {
		ids = append(ids, strconv.FormatInt(c.id, 10))
	}
	more := ""
	if rest := len(cands) - shown; rest > 0 {
		more = fmt.Sprintf(" +%d more", rest)
	}
	text := fmt.Sprintf("[ytwrite] %d %s (%s) op ids %s%s | %s", len(cands), class, strings.Join(parts, ", "), strings.Join(ids, ", "), more, ytWriteOpAlertPointer[class])
	if len(text) > RelayLaneEventMaxBytes {
		text = clipText(text, RelayLaneEventMaxBytes-len("…")) + "…"
	}
	return text
}
