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
//
// Passes are serialised by a transaction-scoped advisory lock taken before the
// candidate read. Two passes that straddle a window boundary would otherwise
// both read an op as unmarked and announce it in two windows.
func (s *Store) EnqueueYTWriteOpAlerts(ctx context.Context, now time.Time) (int, error) {
	now = now.UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('yt_write_op_alerts', 0))`); err != nil {
		return 0, err
	}

	window := now.Truncate(ytWriteOpAlertWindow)
	created := 0
	for _, class := range []string{ytWriteOpAlertStuck, ytWriteOpAlertRejected} {
		cands, err := s.ytWriteOpAlertCandidates(ctx, tx, class, now.Add(-ytWriteOpAlertDelay))
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
// a concurrent resolution. A stuck op is prepared since before cutoff, or
// dispatched since before cutoff, or remote_unknown since before cutoff (no
// state-entry time is kept for remote_unknown, so its creation time stands in;
// its write may already have landed, and only the operator resolves it). A
// rejected op is rejected, or applied with an unpublished binding (the
// operator's rejected view lists it too).
//
// A stuck prepared op of a binding-gated kind is counted under why it is held
// (ytHeldClass: held_binding, held_authority_youtrack, held_mode, ...), not
// under "prepared", so one notice tells the operator which command applies;
// an op its gate admits, and any other kind, stays "prepared".
func (s *Store) ytWriteOpAlertCandidates(ctx context.Context, tx pgx.Tx, class string, cutoff time.Time) ([]ytWriteOpAlertCandidate, error) {
	// The binding-conflict reason is a literal, not a parameter, so a cached
	// generic plan can still use the partial index yt_write_ops_alert_scan.
	var q string
	var args []any
	switch class {
	case ytWriteOpAlertStuck:
		q = `SELECT o.state, o.reason, ` + ytHeldOpColumns + ` FROM yt_write_ops o
			WHERE ((o.state='prepared' AND o.created_at <= $1) OR (o.state='dispatched' AND o.dispatched_at <= $1)
				OR (o.state='remote_unknown' AND o.created_at <= $1))
			AND NOT EXISTS (SELECT 1 FROM yt_write_op_alerts a WHERE a.op_id=o.id AND a.alert_class='stuck')
			ORDER BY o.id LIMIT $2 FOR SHARE OF o`
		args = []any{cutoff.UTC(), ytWriteOpAlertBatchMax}
	case ytWriteOpAlertRejected:
		q = `SELECT o.state, o.reason, ` + ytHeldOpColumns + ` FROM yt_write_ops o
			WHERE (o.state='rejected' OR (o.state='applied' AND o.reason='` + ytwrite.ReasonBindingConflict + `'))
			AND NOT EXISTS (SELECT 1 FROM yt_write_op_alerts a WHERE a.op_id=o.id AND a.alert_class='rejected')
			ORDER BY o.id LIMIT $1 FOR SHARE OF o`
		// Only the limit: this class has no time condition, so a cutoff
		// argument would have no placeholder to bind.
		args = []any{ytWriteOpAlertBatchMax}
	default:
		return nil, fmt.Errorf("unknown yt op alert class %q", class)
	}
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type row struct {
		op            ytHeldOp
		state, reason string
	}
	var found []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.state, &r.reason, &r.op.id, &r.op.principal, &r.op.kind, &r.op.tenant, &r.op.entityKind, &r.op.entityID, &r.op.target, &r.op.createdAt); err != nil {
			return nil, err
		}
		found = append(found, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	sides := map[YTAuthorityKind]YTAuthoritySide{}
	var out []ytWriteOpAlertCandidate
	for _, r := range found {
		label := r.state
		switch {
		case class == ytWriteOpAlertRejected:
			label = r.reason
			if label == "" {
				label = "unspecified"
			}
		case r.state == string(ytwrite.StatePrepared) && ytHeldInScope(r.op):
			kind := ytAuthorityKindOf(r.op.principal)
			side, ok := sides[kind]
			if !ok {
				if side, err = ytAuthorityTx(ctx, tx, kind); err != nil {
					return nil, err
				}
				sides[kind] = side
			}
			held, err := s.classifyHeldYTOpTx(ctx, tx, r.op, side)
			if err != nil {
				return nil, err
			}
			if held != ytHeldReady {
				label = string(held)
			}
		}
		out = append(out, ytWriteOpAlertCandidate{id: r.op.id, label: label})
	}
	return out, nil
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
