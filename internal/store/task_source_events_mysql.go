package store

import (
	"context"
	"time"
)

// The MySQL side of task_source_events.go. The names carry the srcEv prefix so
// no other porter's helper can collide with them.

// refs->>'pr' and refs->>'job_id' of a tasks row. JSON_UNQUOTE of a JSON null
// is the string 'null' where PostgreSQL's ->> gives SQL NULL, so the value is
// read only when it is a JSON string.
const (
	srcEvRefsPRMySQL    = `CASE WHEN JSON_TYPE(JSON_EXTRACT(refs,'$.pr'))='STRING' THEN JSON_UNQUOTE(JSON_EXTRACT(refs,'$.pr')) END`
	srcEvRefsJobIDMySQL = `CASE WHEN JSON_TYPE(JSON_EXTRACT(refs,'$.job_id'))='STRING' THEN JSON_UNQUOTE(JSON_EXTRACT(refs,'$.job_id')) END`
)

// srcEvDeliveredPRMySQL is taskDeliveredPRSQL for the tasks row aliased t: the
// task's own refs.pr, else the refs.pr of a "[bundle ...]" parent, else the empty string.
const srcEvDeliveredPRMySQL = `COALESCE(NULLIF(CASE WHEN JSON_TYPE(JSON_EXTRACT(t.refs,'$.pr'))='STRING' THEN JSON_UNQUOTE(JSON_EXTRACT(t.refs,'$.pr')) END,''), ` +
	`(SELECT CASE WHEN JSON_TYPE(JSON_EXTRACT(p.refs,'$.pr'))='STRING' THEN JSON_UNQUOTE(JSON_EXTRACT(p.refs,'$.pr')) END FROM tasks p WHERE p.id=t.parent_id AND p.title LIKE '[bundle %'), '')`

// srcEvLockedTasksMySQL returns the tasks whose ids idQuery selects, locked
// FOR UPDATE in ascending id order. PostgreSQL's ORDER BY ... FOR UPDATE
// locks in sort order; InnoDB locks rows in the order its access path visits
// them, which for a state or key index is not id order. So the ids are read
// first, without a lock, and the rows are then locked through one primary-key
// range scan, which is ascending. The callers hold the task-mutation lock
// (level 2), so no writer changes the set between the two statements.
func srcEvLockedTasksMySQL(ctx context.Context, tx transaction, idQuery string, args ...any) ([]Task, error) {
	rs, err := tx.Query(ctx, idQuery, args...)
	if err != nil {
		return nil, err
	}
	ids, err := collectRows(rs, rowTo[int64])
	if err != nil || len(ids) == 0 {
		return []Task{}, err
	}
	idArgs := make([]any, len(ids))
	for i, id := range ids {
		idArgs[i] = id
	}
	rs, err = tx.Query(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id IN (`+placeholders(len(ids))+`) ORDER BY id FOR UPDATE`, idArgs...)
	if err != nil {
		return nil, err
	}
	return collectRows(rs, func(row row) (Task, error) {
		var x Task
		return x, scanTask(row, &x)
	})
}

// srcEvChangesGate refuses a MySQL transaction before a read of the changes
// or change_tasks tables. B5 dependency: those tables are not in the MySQL
// schema yet. Make it return nil with B5, and add the MySQL texts beside the
// PostgreSQL ones it guards (taskHoldsTx, heldMergePRTx, propagateMergeTx;
// prURLSQL, openImplementsSQL and unresolvedRevertSQL have no MySQL spelling
// yet).
func srcEvChangesGate(tx transaction) error {
	if dialectFor(tx) == mysqlDialect {
		return ErrDialectUnsupported
	}
	return nil
}

// srcEvRecordChangeTx is recordChangeTx for a source event. B5 dependency:
// recordChangeTx writes changes, change_tasks and reviews. It writes nothing
// for a PR URL parsePRURL cannot read, so only that case skips it on MySQL;
// every other event refuses before the call.
func srcEvRecordChangeTx(ctx context.Context, tx transaction, x Task, in TaskSourceEvent, at, now time.Time) error {
	if dialectFor(tx) == mysqlDialect {
		if _, _, ok := parsePRURL(in.PR); ok {
			return ErrDialectUnsupported
		}
		return nil
	}
	return recordChangeTx(ctx, pgxTx(tx), x, in, at, now)
}

// srcEvDecisionGate is the B6 dependency of applySourceEventTx.
// mirrorTaskDecisionsTx supersedes every open decision row of a task that
// ends (DecisionEndsTask: completed and canceled categories, so merged,
// deployed, completed and icebox targets). In every other case the call is a
// no-op for these events, which change neither the decision request nor the
// awaiting flag, so MySQL skips it there. The gate runs before the task is
// written. Delete it with B6.
func srcEvDecisionGate(tx transaction, to string) error {
	if dialectFor(tx) == mysqlDialect && DecisionEndsTask(to) {
		return ErrDialectUnsupported
	}
	return nil
}

// srcEvSystemCommentTx adds an event comment. PostgreSQL runs
// insertSystemTaskCommentTx, which also enqueues the comment's YouTrack copy.
// B4 dependency: on MySQL the copy is not ported, so the comment is written
// only while the YouTrack comment gate is off (ytCommentsModeNow), where
// enqueueYTCommentTx writes nothing; in canary or on mode it refuses.
//
// The insert mirrors the PostgreSQL one: one row keyed by systemTaskCommentKey
// that a repeat leaves alone. As there, the caller holds the comment insert
// lock before it locks task rows (ApplyTaskSourceEvent and applyDeployRecordTx
// do); taking it here, after those rows, could cycle with a comment writer
// that holds it and waits for the task row. task_comments is append-only (its
// BEFORE UPDATE trigger refuses an ON DUPLICATE KEY update), so the existing
// row is read instead; a race the lock does not cover ends in the unique key's
// error, which the caller logs. B3 owns the shared comment insert and should
// absorb this.
func (s *Store) srcEvSystemCommentTx(ctx context.Context, tx transaction, taskID int64, source, eventKey, body string, createdAt time.Time) error {
	if dialectFor(tx) != mysqlDialect {
		return s.insertSystemTaskCommentTx(ctx, pgxTx(tx), taskID, source, eventKey, body, createdAt)
	}
	if err := CheckTaskCommentBody(body); err != nil {
		return err
	}
	if s.ytCommentsModeNow() != YTCommentsOff {
		return ErrDialectUnsupported
	}
	key := systemTaskCommentKey(taskID, source, eventKey)
	var have bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_comments WHERE task_id=? AND system_event_key=?)`, taskID, key).Scan(&have); err != nil {
		return err
	}
	if have {
		return nil
	}
	_, err := tx.InsertID(ctx, `INSERT INTO task_comments(task_id,body,author,created_at,system_event_key) VALUES(?,?,?,?,?)`, taskID, body, YTCommentPrincipal, createdAt.UTC(), key)
	return err
}
