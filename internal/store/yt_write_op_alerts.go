package store

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

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
	var out int
	err := s.retryTx(ctx, func() error {
		var err error
		out, err = s.enqueueYTWriteOpAlertsOnce(ctx, now)
		return err
	})
	return out, err
}

// enqueueYTWriteOpAlertsOnce is one attempt of EnqueueYTWriteOpAlerts's transaction.
func (s *Store) enqueueYTWriteOpAlertsOnce(ctx context.Context, now time.Time) (int, error) {
	now = now.UTC()
	tx, release, err := s.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	defer tx.Rollback(ctx)
	if err := s.ytAlertPassLock(ctx, tx); err != nil {
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
		if _, err := tx.Exec(ctx, s.ytAlertMarkQ(len(ids)), s.ytAlertMarkArgs(ids, class, now)...); err != nil {
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

// ytWriteOpAlertRow is one op a class's read returns: its state and reason,
// and the held-op columns the notice names.
type ytWriteOpAlertRow struct {
	op            ytHeldOp
	state, reason string
}

// ytWriteOpAlertArm is one read of a class's candidates: SQL without its
// locking clause (ytReadWriteOpAlertArm adds it) and its arguments.
type ytWriteOpAlertArm struct {
	sql  string
	args []any
}

// ytWriteOpAlertArms returns the reads that together list class's candidates,
// each bounded to the batch. PostgreSQL reads them in one statement, which its
// partial index yt_write_ops_alert_scan serves. MySQL reads one arm per state:
// a single statement that ORs the states and orders by id under a limit lets
// MySQL walk the primary key in id order and examine the table until it has
// the batch (hk 5878). An arm is an equality on state, so yt_write_ops_state_id
// serves its order and its limit, and the arms are disjoint.
func ytWriteOpAlertArms(tx transaction, class string, cutoff time.Time) ([]ytWriteOpAlertArm, error) {
	cut := cutoff.UTC()
	batch := ytWriteOpAlertBatchMax
	binding := ytwrite.ReasonBindingConflict
	if dialectFor(tx) != mysqlDialect {
		switch class {
		case ytWriteOpAlertStuck:
			// PostgreSQL reuses $1 for the three cutoffs.
			return []ytWriteOpAlertArm{{sql: `SELECT o.state, o.reason, ` + ytHeldOpColumns + ` FROM yt_write_ops o
				WHERE ((o.state='prepared' AND o.created_at <= $1) OR (o.state='dispatched' AND o.dispatched_at <= $1)
					OR (o.state='remote_unknown' AND o.created_at <= $1))
				AND NOT EXISTS (SELECT 1 FROM yt_write_op_alerts a WHERE a.op_id=o.id AND a.alert_class='stuck')
				ORDER BY o.id LIMIT $2`, args: []any{cut, batch}}}, nil
		case ytWriteOpAlertRejected:
			// Only the limit: this class has no time condition.
			return []ytWriteOpAlertArm{{sql: `SELECT o.state, o.reason, ` + ytHeldOpColumns + ` FROM yt_write_ops o
				WHERE (o.state='rejected' OR (o.state='applied' AND o.reason='` + binding + `'))
				AND NOT EXISTS (SELECT 1 FROM yt_write_op_alerts a WHERE a.op_id=o.id AND a.alert_class='rejected')
				ORDER BY o.id LIMIT $1`, args: []any{batch}}}, nil
		}
		return nil, fmt.Errorf("unknown yt op alert class %q", class)
	}
	const stuckNotAlerted = `AND NOT EXISTS (SELECT 1 FROM yt_write_op_alerts a WHERE a.op_id=o.id AND a.alert_class='stuck')`
	const rejectedNotAlerted = `AND NOT EXISTS (SELECT 1 FROM yt_write_op_alerts a WHERE a.op_id=o.id AND a.alert_class='rejected')`
	sel := `SELECT o.state, o.reason, ` + ytHeldOpColumns + ` FROM yt_write_ops o WHERE `
	switch class {
	case ytWriteOpAlertStuck:
		// Each state's cutoff is bound where its time condition is.
		return []ytWriteOpAlertArm{
			{sql: sel + `o.state='prepared' AND o.created_at <= ? ` + stuckNotAlerted + ` ORDER BY o.id LIMIT ?`, args: []any{cut, batch}},
			{sql: sel + `o.state='dispatched' AND o.dispatched_at <= ? ` + stuckNotAlerted + ` ORDER BY o.id LIMIT ?`, args: []any{cut, batch}},
			{sql: sel + `o.state='remote_unknown' AND o.created_at <= ? ` + stuckNotAlerted + ` ORDER BY o.id LIMIT ?`, args: []any{cut, batch}},
		}, nil
	case ytWriteOpAlertRejected:
		return []ytWriteOpAlertArm{
			{sql: sel + `o.state='rejected' ` + rejectedNotAlerted + ` ORDER BY o.id LIMIT ?`, args: []any{batch}},
			{sql: sel + `o.state='applied' AND o.reason='` + binding + `' ` + rejectedNotAlerted + ` ORDER BY o.id LIMIT ?`, args: []any{batch}},
		}, nil
	}
	return nil, fmt.Errorf("unknown yt op alert class %q", class)
}

// ytReadWriteOpAlertArm runs one arm under the transaction's row locks: the
// candidates are share-locked until the pass commits, as the single statement
// locked them.
func ytReadWriteOpAlertArm(ctx context.Context, tx transaction, arm ytWriteOpAlertArm) ([]ytWriteOpAlertRow, error) {
	lock := ` FOR SHARE OF o`
	if dialectFor(tx) == mysqlDialect {
		lock = ` FOR SHARE`
	}
	rows, err := tx.Query(ctx, arm.sql+lock, arm.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ytWriteOpAlertRow
	for rows.Next() {
		var r ytWriteOpAlertRow
		if err := rows.Scan(&r.state, &r.reason, &r.op.id, &r.op.principal, &r.op.kind, &r.op.tenant, &r.op.entityKind, &r.op.entityID, &r.op.target, &r.op.createdAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ytCapByID sorts rows by id and keeps the first limit. It merges the arms of
// one class: each arm already holds its first limit ids, so the first limit of
// the union is the first limit of the class.
func ytCapByID[T any](rows []T, id func(T) int64, limit int) []T {
	sort.Slice(rows, func(i, j int) bool { return id(rows[i]) < id(rows[j]) })
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
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
func (s *Store) ytWriteOpAlertCandidates(ctx context.Context, tx transaction, class string, cutoff time.Time) ([]ytWriteOpAlertCandidate, error) {
	arms, err := ytWriteOpAlertArms(tx, class, cutoff)
	if err != nil {
		return nil, err
	}
	var found []ytWriteOpAlertRow
	for _, arm := range arms {
		got, err := ytReadWriteOpAlertArm(ctx, tx, arm)
		if err != nil {
			return nil, err
		}
		found = append(found, got...)
	}
	// Every arm is bounded to the batch, so the first batch ids of their union
	// are the first batch ids of the class (an op is in one arm only).
	found = ytCapByID(found, func(r ytWriteOpAlertRow) int64 { return r.op.id }, ytWriteOpAlertBatchMax)
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
// It names at most ytWriteOpAlertIDsShown ids and retains its full text.
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
	return text
}

// ytAlertPassLock serialises alert passes: a transaction-scoped advisory lock
// on PostgreSQL, a transaction-held session lock on MySQL.
func (s *Store) ytAlertPassLock(ctx context.Context, tx transaction) error {
	if dialectFor(tx) == mysqlDialect {
		_, err := mysqlTransactionLock(ctx, tx, mysqlLockKey("ns", "yt-op-alerts"), false)
		return err
	}
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('yt_write_op_alerts', 0))`)
	return err
}

// ytAlertMarkQ is the "mark these ops announced" insert in this store's
// dialect.
func (s *Store) ytAlertMarkQ(n int) string {
	if s.dialect == mysqlDialect {
		return `INSERT INTO yt_write_op_alerts(op_id, alert_class, created_at) VALUES` +
			strings.TrimSuffix(strings.Repeat(`(?,?,?),`, n), `,`) + ` ON DUPLICATE KEY UPDATE op_id=op_id`
	}
	return `INSERT INTO yt_write_op_alerts(op_id, alert_class, created_at)
		SELECT x, $2::text, $3::timestamptz FROM unnest($1::bigint[]) AS x ON CONFLICT DO NOTHING`
}

// ytAlertMarkArgs lays out the mark insert's arguments per dialect.
func (s *Store) ytAlertMarkArgs(ids []int64, class string, now time.Time) []any {
	if s.dialect == mysqlDialect {
		args := make([]any, 0, 3*len(ids))
		for _, id := range ids {
			args = append(args, id, class, now)
		}
		return args
	}
	return []any{ids, class, now}
}
