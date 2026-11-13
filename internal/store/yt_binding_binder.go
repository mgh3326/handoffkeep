package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The automatic binder's durable state (hk 4492, design/2026-11-13/yt-comment-binding-lifecycle
// §6.6, §6.9). A pass reads the task_comments ids above yt_binding_cursor and
// records one yt_binding_intents row per task it sees that has no binding and
// no pending issue_create intent; "seen" therefore survives a restart. The
// worker then binds the intents whose next_attempt_at has come, and settle
// records the outcome: delete on a binding, refused/error with a backoff,
// excluded when the operator unbound the task and the binder must not undo it
// (I7). The tables are the binder's own: no lane holder reads them.

// YTBindingIntentState is one yt_binding_intents.state.
type YTBindingIntentState string

const (
	// YTBindingIntentPending is an intent the binder has not tried yet.
	YTBindingIntentPending YTBindingIntentState = "pending"
	// YTBindingIntentRefused is a task the binder classified and refused
	// (no_issue, ambiguous, history_ambiguous, other_tenant); it is retried
	// after the backoff.
	YTBindingIntentRefused YTBindingIntentState = "refused"
	// YTBindingIntentError is a task whose reads failed; it is retried after
	// the backoff.
	YTBindingIntentError YTBindingIntentState = "error"
	// YTBindingIntentExcluded is a task the operator unbound: no automatic
	// binding ever, and no YouTrack call. Terminal.
	YTBindingIntentExcluded YTBindingIntentState = "excluded"
)

// YTBindingIntent is one row of yt_binding_intents.
type YTBindingIntent struct {
	TaskID        int64
	Tenant        string
	State         YTBindingIntentState
	Reason        string
	Attempts      int
	LastError     string
	NextAttemptAt time.Time
	RecordedAt    time.Time
}

// YTBindingScan is what one candidate scan read and queued.
type YTBindingScan struct {
	// Comments is how many task_comments ids the pass read above the cursor.
	Comments int
	// Tasks is how many new intents the pass made durable.
	Tasks int
}

// ytBindingIntentBackoff is how long a refused or failed attempt waits before
// the next one: 1 h, 6 h, 24 h, then 7 days. attempts is the count after the
// failure (clamped to 8), so a task that can never bind costs one GET a week.
func ytBindingIntentBackoff(attempts int) time.Duration {
	switch {
	case attempts <= 1:
		return time.Hour
	case attempts == 2:
		return 6 * time.Hour
	case attempts == 3:
		return 24 * time.Hour
	default:
		return 7 * 24 * time.Hour
	}
}

// ScanYTBindingCandidates reads the next page of task_comments above the
// binder's cursor and records an intent for every task it has not seen, in one
// transaction: either the cursor moves and the intents commit, or neither
// does. A task with a binding is left out (it needs no automatic bind). A task
// whose issue may still be created (a yt_issue_intents row, or an unresolved
// issue_create op) gets its intent due in an hour instead of no intent: that
// op publishes the binding when it applies, and if it is rejected the task is
// reconsidered rather than stranded with its comments already past the
// cursor. An existing intent row is never reset, so a backoff survives a
// re-scan.
//
// Locks (lockorder.go): the cursor row FOR UPDATE, then plain reads of
// task_comments, yt_task_bindings and yt_issue_intents, then new rows of
// yt_binding_intents. It takes no lane lock and no task row.
func (s *Store) ScanYTBindingCandidates(ctx context.Context, tenant string, limit int, now time.Time) (YTBindingScan, error) {
	var out YTBindingScan
	if tenant == "" || len(tenant) > 255 || limit < 1 {
		return out, errors.New("invalid binding scan")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	var cursor int64
	if err := tx.QueryRow(ctx, `SELECT last_comment_id FROM yt_binding_cursor WHERE id=1 FOR UPDATE`).Scan(&cursor); err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, `SELECT id, task_id FROM task_comments WHERE id > $1 ORDER BY id LIMIT $2`, cursor, limit)
	if err != nil {
		return out, err
	}
	last := cursor
	seen := map[int64]bool{}
	var taskIDs []int64
	for rows.Next() {
		var id, taskID int64
		if err := rows.Scan(&id, &taskID); err != nil {
			rows.Close()
			return out, err
		}
		out.Comments++
		if id > last {
			last = id
		}
		if !seen[taskID] {
			seen[taskID] = true
			taskIDs = append(taskIDs, taskID)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	now = now.UTC()
	for _, taskID := range taskIDs {
		tag, err := tx.Exec(ctx, `INSERT INTO yt_binding_intents(task_id, tenant, state, next_attempt_at, recorded_at)
			SELECT $1,$2,'pending',
				CASE WHEN EXISTS(SELECT 1 FROM yt_issue_intents WHERE task_id=$1)
					OR EXISTS(SELECT 1 FROM yt_write_ops WHERE kind=$4 AND entity_kind='task' AND entity_id=$1::text
						AND state NOT IN ('applied','rejected'))
				THEN $3 + interval '1 hour' ELSE $3 END,
				$3
			WHERE NOT EXISTS(SELECT 1 FROM yt_task_bindings WHERE task_id=$1)
			ON CONFLICT (task_id) DO NOTHING`, taskID, tenant, now, ytwrite.KindIssueCreate)
		if err != nil {
			return out, err
		}
		out.Tasks += int(tag.RowsAffected())
	}
	if last != cursor {
		if _, err := tx.Exec(ctx, `UPDATE yt_binding_cursor SET last_comment_id=$1 WHERE id=1`, last); err != nil {
			return out, err
		}
	}
	return out, tx.Commit(ctx)
}

// YTBindingIntentsDue lists, in due order, the tenant's intents whose next
// attempt has come, at most limit of them.
func (s *Store) YTBindingIntentsDue(ctx context.Context, tenant string, now time.Time, limit int) ([]YTBindingIntent, error) {
	if tenant == "" || limit < 1 {
		return nil, errors.New("invalid binding intents read")
	}
	rows, err := s.pool.Query(ctx, `SELECT task_id, tenant, state, reason, attempts, last_error, next_attempt_at, recorded_at
		FROM yt_binding_intents
		WHERE tenant=$1 AND state IN ('pending','refused','error') AND next_attempt_at <= $2
		ORDER BY next_attempt_at, task_id LIMIT $3`, tenant, now.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []YTBindingIntent{}
	for rows.Next() {
		var in YTBindingIntent
		var state string
		if err := rows.Scan(&in.TaskID, &in.Tenant, &state, &in.Reason, &in.Attempts, &in.LastError, &in.NextAttemptAt, &in.RecordedAt); err != nil {
			return nil, err
		}
		in.State = YTBindingIntentState(state)
		out = append(out, in)
	}
	return out, rows.Err()
}

// RetryYTBindingIntent records a refused or failed attempt: state and reason,
// the detail, one more attempt (clamped at 8) and the next attempt after that
// attempt's backoff. A terminal (excluded) or absent row is left alone.
func (s *Store) RetryYTBindingIntent(ctx context.Context, taskID int64, state YTBindingIntentState, reason, detail string, now time.Time) error {
	if taskID < 1 || (state != YTBindingIntentRefused && state != YTBindingIntentError) {
		return errors.New("invalid binding intent outcome")
	}
	_, err := s.pool.Exec(ctx, `UPDATE yt_binding_intents SET
			state=$2, reason=$3, last_error=$4,
			attempts=least(attempts+1, 8),
			next_attempt_at=$5 + CASE least(attempts+1, 8)
				WHEN 1 THEN interval '1 hour'
				WHEN 2 THEN interval '6 hours'
				WHEN 3 THEN interval '24 hours'
				ELSE interval '7 days' END,
			recorded_at=$5
		WHERE task_id=$1 AND state IN ('pending','refused','error')`,
		taskID, string(state), clipText(reason, 200), clipText(detail, 2000), now.UTC())
	return err
}

// ExcludeYTBindingIntent makes the task's intent terminal: the operator
// unbound it, so no automatic binding may undo that (I7, design §6.1).
func (s *Store) ExcludeYTBindingIntent(ctx context.Context, taskID int64, reason string, now time.Time) error {
	if taskID < 1 {
		return errors.New("invalid binding intent task")
	}
	_, err := s.pool.Exec(ctx, `UPDATE yt_binding_intents SET state='excluded', reason=$2, last_error='', next_attempt_at=$3, recorded_at=$3
		WHERE task_id=$1 AND state <> 'excluded'`, taskID, clipText(reason, 200), now.UTC())
	return err
}

// DropYTBindingIntent deletes the task's intent: it is bound, or nothing is
// left to do.
func (s *Store) DropYTBindingIntent(ctx context.Context, taskID int64) error {
	if taskID < 1 {
		return errors.New("invalid binding intent task")
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM yt_binding_intents WHERE task_id=$1`, taskID)
	return err
}

// ytBindingLatestActionTx is the action of the task's latest
// yt_binding_history row ("" when it has none), in the caller's transaction.
func ytBindingLatestActionTx(ctx context.Context, q ytRowQuerier, taskID int64) (string, error) {
	var action string
	err := q.QueryRow(ctx, `SELECT action FROM yt_binding_history WHERE task_id=$1 ORDER BY id DESC LIMIT 1`, taskID).Scan(&action)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return action, err
}

// YTBindingExcluded reports whether the operator unbound the task and nothing
// bound it since: the latest yt_binding_history row's action is 'unbound'.
// Counted in, the binder must not bind it again (I7).
func (s *Store) YTBindingExcluded(ctx context.Context, taskID int64) (bool, error) {
	if taskID < 1 {
		return false, errors.New("invalid binding history task")
	}
	action, err := ytBindingLatestActionTx(ctx, s.pool, taskID)
	return action == ytBindingActionUnbound, err
}

// YTHighestCommentID is the task's highest task_comments id (0 when it has
// none). The binder uses it as the watermark under comments authority
// youtrack, where it lists no issue and copies nothing: every comment is
// already canonical in YouTrack.
func (s *Store) YTHighestCommentID(ctx context.Context, taskID int64) (int64, error) {
	if taskID < 1 {
		return 0, errors.New("invalid comment task")
	}
	var id int64
	err := s.pool.QueryRow(ctx, `SELECT coalesce(max(id),0) FROM task_comments WHERE task_id=$1`, taskID).Scan(&id)
	return id, err
}
