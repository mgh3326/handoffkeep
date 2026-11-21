package store

import (
	"context"
	"strconv"
	"time"
)

// Attention on MySQL (hk 5088). The PostgreSQL statements are kept as they
// are; these are their MySQL texts. Time is passed in, once per placeholder:
// the same Go instant is every `?` below. Every statement reads under
// beginSnapshotA5, the REPEATABLE READ READ ONLY snapshot of design §11.1.

// attentionInboxVisibleMySQL is attentionInboxVisible with one `?`.
const attentionInboxVisibleMySQL = `(s.done_at IS NULL AND (s.snoozed_until IS NULL OR s.snoozed_until<=?))`

// attentionTaskVisibleMySQL is attentionTaskVisible with one `?`.
const attentionTaskVisibleMySQL = `(s.done_at IS NULL OR s.done_at<` + taskActivitySQLMySQL + `) AND (s.snoozed_until IS NULL OR s.snoozed_until<=?)`

// relayDecisionNeededKeyMySQL is the functional key of an open-decision lane
// event: 1 when the row is a [decision-needed] lane.event, else 0. It names no
// AUTO_INCREMENT column (MySQL rejects such a functional key, error 3754), so
// the index relay_events_decision_lane is built on this flag and the InnoDB
// primary key follows it. The index is built on this exact expression
// (migration 20261121-relay-decision-lane), and attentionOpenLaneDecisionSQLMySQL
// compares it with = 1, the form the optimizer matches to the key, so the
// planner ranges over the decision-needed events alone instead of every
// lane.event row. Keep the two texts identical.
const relayDecisionNeededKeyMySQL = `CASE WHEN kind='lane.event' AND text LIKE '[decision-needed]%' THEN 1 ELSE 0 END`

// attentionOpenLaneDecisionSQLMySQL is openLaneDecisionSQL for MySQL. It reads
// relay_events as "e" and is used only where e is the sole table in scope, so
// the key's bare column names resolve to e.
const attentionOpenLaneDecisionSQLMySQL = `(` + relayDecisionNeededKeyMySQL + `) = 1 AND NOT EXISTS (
			SELECT 1 FROM relay_events resolved WHERE resolved.kind='lane.event' AND resolved.owner_lane=e.owner_lane
			AND resolved.text LIKE CONCAT('[decision-answered] #',e.id,':%') AND resolved.id>e.id
		)`

// attentionLaneSQLMySQL is attentionLaneSQL for MySQL, with one `?`: the read
// time of the live test of the decision_requests row.
const attentionLaneSQLMySQL = `relay_events e WHERE ` + attentionOpenLaneDecisionSQLMySQL + ` AND NOT EXISTS (
		SELECT 1 FROM decision_requests d WHERE d.source='lane_event' AND d.source_ref=CAST(e.id AS CHAR) AND d.status='open' AND (d.expires_at IS NULL OR d.expires_at>?))`

// attentionStalledSQLMySQL is attentionStalledSQL with one `?`.
const attentionStalledSQLMySQL = `state IN (` + startedStatesSQL + `) AND NOT awaiting_decision AND ` + taskActivitySQLMySQL + ` < ?`

// attentionDecisionCountsMySQL takes the read time nine times: open (2) and
// visible (1) for requests, live (1) and visible (1) for lane events, open (2),
// visible (1) and the overdue test (1) for the overdue count.
const attentionDecisionCountsMySQL = `SELECT
		(SELECT COUNT(*) FROM (SELECT id FROM decision_requests WHERE ` + reportDecisionOpenSQL + `) d
			LEFT JOIN operator_inbox_state s ON s.item_key=CONCAT('decision:request:',d.id) WHERE ` + attentionInboxVisibleMySQL + `),
		(SELECT COUNT(*) FROM (SELECT e.id FROM ` + attentionLaneSQLMySQL + `) lane
			LEFT JOIN operator_inbox_state s ON s.item_key=CONCAT('decision:lane_event:',lane.id) WHERE ` + attentionInboxVisibleMySQL + `),
		(SELECT COUNT(*) FROM (SELECT id,due_at FROM decision_requests WHERE ` + reportDecisionOpenSQL + `) d
			LEFT JOIN operator_inbox_state s ON s.item_key=CONCAT('decision:request:',d.id) WHERE ` + attentionInboxVisibleMySQL + ` AND d.due_at<=?)`

// attentionRequestItemsMySQL takes the read time four times (open twice,
// visible, the overdue ordering) and then the limit.
const attentionRequestItemsMySQL = `SELECT d.id,d.source_ref,d.lane,d.question,d.created_at,d.due_at,s.read_at,s.done_at,s.snoozed_until
		FROM (SELECT id,source_ref,lane,question,created_at,due_at FROM decision_requests WHERE ` + reportDecisionOpenSQL + `) d
		LEFT JOIN operator_inbox_state s ON s.item_key=CONCAT('decision:request:',d.id)
		WHERE ` + attentionInboxVisibleMySQL + `
		ORDER BY (d.due_at IS NOT NULL AND d.due_at<=?) DESC, (d.due_at IS NULL) ASC, d.due_at ASC, d.created_at DESC, d.id DESC LIMIT ?`

// attentionLaneItemsMySQL takes the read time twice (live, visible) and then the limit.
const attentionLaneItemsMySQL = `SELECT lane.id,lane.owner_lane,lane.text,lane.received_at,s.read_at,s.done_at,s.snoozed_until
		FROM (SELECT e.id,e.owner_lane,e.text,e.received_at FROM ` + attentionLaneSQLMySQL + `) lane
		LEFT JOIN operator_inbox_state s ON s.item_key=CONCAT('decision:lane_event:',lane.id)
		WHERE ` + attentionInboxVisibleMySQL + `
		ORDER BY lane.received_at DESC, lane.id DESC LIMIT ?`

// attentionTaskCountsMySQL takes the stalled threshold twice, then the read time.
const attentionTaskCountsMySQL = `SELECT COALESCE(SUM(CASE WHEN ` + awaitingTaskDecisionSQLMySQL + ` THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN ` + attentionStalledSQLMySQL + ` THEN 1 ELSE 0 END),0)
		FROM tasks LEFT JOIN operator_inbox_state s ON s.item_key=CONCAT('task:',tasks.id)
		WHERE ((` + awaitingTaskDecisionSQLMySQL + `) OR (` + attentionStalledSQLMySQL + `)) AND ` + attentionTaskVisibleMySQL

// attentionTaskItemsMySQL takes the stalled threshold, the read time and the limit.
const attentionTaskItemsMySQL = `SELECT tasks.id,title,lane,state,priority,(` + awaitingTaskDecisionSQLMySQL + `) AS awaiting,` + taskActivitySQLMySQL + ` AS since,s.read_at,s.done_at,s.snoozed_until
		FROM tasks LEFT JOIN operator_inbox_state s ON s.item_key=CONCAT('task:',tasks.id)
		WHERE ((` + awaitingTaskDecisionSQLMySQL + `) OR (` + attentionStalledSQLMySQL + `)) AND ` + attentionTaskVisibleMySQL + `
		ORDER BY since DESC, tasks.id DESC LIMIT ?`

// attentionNowArgs is the read time as the argument list of a statement:
// once on PostgreSQL (one numbered $1), mysqlN times on MySQL (one per `?`).
func attentionNowArgs(tx transaction, now time.Time, pgN, mysqlN int) []any {
	n := pgN
	if dialectFor(tx) == mysqlDialect {
		n = mysqlN
	}
	out := make([]any, n)
	for i := range out {
		out[i] = now
	}
	return out
}

// attentionItemArgs is attentionNowArgs followed by the limit; PostgreSQL's
// statement names the read time $1 and the limit $2.
func attentionItemArgs(tx transaction, now time.Time, mysqlNows, limit int) []any {
	return append(attentionNowArgs(tx, now, 1, mysqlNows), limit)
}

// attentionTaskArgs is the argument list of the task counts (counts) or the
// task items: the stalled threshold, the read time and, for items, the limit.
// MySQL's counts text names the threshold twice.
func attentionTaskArgs(tx transaction, stalledBefore, now time.Time, counts bool, limit int) []any {
	if counts {
		if dialectFor(tx) == mysqlDialect {
			return []any{stalledBefore, stalledBefore, now}
		}
		return []any{stalledBefore, now}
	}
	return []any{stalledBefore, now, limit}
}

// attentionTaskCandidatesMySQL is AttentionTaskCandidates on MySQL: every
// waiting task, newest activity first. The started ids are an IN list; an
// empty list (no task YouTrack shows started) is the always-false condition.
func (s *Store) attentionTaskCandidatesMySQL(ctx context.Context, now time.Time, startedIDs []int64) ([]AttentionTask, error) {
	started := "FALSE"
	args := []any{}
	if len(startedIDs) > 0 {
		started = "tasks.id IN (" + placeholders(len(startedIDs)) + ")"
		for _, id := range startedIDs {
			args = append(args, id)
		}
	}
	query := `SELECT tasks.id,title,lane,state,priority,(` + awaitingTaskDecisionSQLMySQL + `) AS awaiting,` + taskActivitySQLMySQL + ` AS since,s.read_at,s.done_at,s.snoozed_until
		FROM tasks LEFT JOIN operator_inbox_state s ON s.item_key=CONCAT('task:',tasks.id)
		WHERE ((` + awaitingTaskDecisionSQLMySQL + `) OR (` + started + ` AND NOT awaiting_decision AND ` + taskActivitySQLMySQL + ` < ?)) AND ` + attentionTaskVisibleMySQL + `
		ORDER BY since DESC, tasks.id DESC`
	args = append(args, now.Add(-s.StartedStalledHours()), now)
	rows, err := s.db().Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AttentionTask{}
	for rows.Next() {
		var x AttentionTask
		var awaiting bool
		var readAt, doneAt, snoozedUntil *time.Time
		if err := rows.Scan(&x.ID, &x.Title, &x.Lane, &x.State, &x.Priority, &awaiting, &x.Since, &readAt, &doneAt, &snoozedUntil); err != nil {
			return nil, err
		}
		x.Key = "task:" + strconv.FormatInt(x.ID, 10)
		x.Since = x.Since.UTC()
		x.Read = readAt != nil
		x.Woken = inboxWoken(doneAt, snoozedUntil, now)
		x.Reason = AttentionReasonStalled
		if awaiting {
			x.Reason = AttentionReasonAwaitingDecision
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
