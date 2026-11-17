package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// TaskEventListMax bounds one page of task events. It matches the comment
// page bound so the MCP read tools can share limits.
const TaskEventListMax = 500

const taskEventColumns = `id,task_id,"from","to","by",note,refs,at,kind,no_job,source,source_ref,field,from_value,to_value,auto,applied`

// taskEventColumnsMySQL is taskEventColumns for MySQL, which quotes with backticks.
const taskEventColumnsMySQL = "id,task_id,`from`,`to`,`by`,note,refs,at,kind,no_job,source,source_ref,field,from_value,to_value,auto,applied"

// taskEventColumnsFor is the task_events column list for the dialect of x, a
// querier or transaction.
func taskEventColumnsFor(x any) string {
	return sqlFor(x, taskEventColumns, taskEventColumnsMySQL)
}

// scanTaskEvent reads one task_events row. refs is the event's refs snapshot,
// which may also carry a job_rebind payload (TaskEvent.Rebind).
func scanTaskEvent(row interface{ Scan(...any) error }, e *TaskEvent) error {
	var refs []byte
	if err := row.Scan(&e.ID, &e.TaskID, &e.From, &e.To, &e.By, &e.Note, &refs, &e.At, &e.Kind, &e.NoJob, &e.Source, &e.SourceRef, &e.Field, &e.FromValue, &e.ToValue, &e.Auto, &e.Applied); err != nil {
		return err
	}
	if len(refs) == 0 {
		return nil
	}
	var snapshot struct {
		TaskRefs
		JobRebind *TaskJobRebind `json:"job_rebind,omitempty"`
	}
	if err := json.Unmarshal(refs, &snapshot); err != nil {
		return err
	}
	e.Refs = &snapshot.TaskRefs
	e.Rebind = snapshot.JobRebind
	return nil
}

// ListTaskEvents returns a task's events in id order after afterID. Events are
// append-only, so id order is the order the watchers see them; it is also the
// cursor the MCP task_events tool pages on. A missing task is ErrTaskNotFound.
func (s *Store) ListTaskEvents(ctx context.Context, taskID, afterID int64, limit int) ([]TaskEvent, error) {
	if taskID < 1 || afterID < 0 || limit < 1 || limit > TaskEventListMax {
		return nil, errors.New("invalid task event query")
	}
	tx, err := s.beginReportRead(ctx, false)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err = tx.QueryRow(ctx, s.q(`SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1)`, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=?)`), taskID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrTaskNotFound
	}
	rows, err := tx.Query(ctx, s.q(`SELECT `+taskEventColumns+` FROM task_events WHERE task_id=$1 AND id>$2 ORDER BY id ASC LIMIT $3`,
		`SELECT `+taskEventColumnsMySQL+` FROM task_events WHERE task_id=? AND id>? ORDER BY id ASC LIMIT ?`), taskID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskEvent{}
	for rows.Next() {
		var e TaskEvent
		if err := scanTaskEvent(rows, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecentTaskComments returns a task's newest comments in id order, oldest of
// the returned slice first. It reads only the tail; the MCP task_get preview
// must not walk every page of a long comment thread.
func (s *Store) RecentTaskComments(ctx context.Context, taskID int64, limit int) ([]TaskComment, error) {
	if taskID < 1 || limit < 1 || limit > TaskCommentListMax {
		return nil, ErrTaskCommentInvalid
	}
	rows, err := s.db().Query(ctx, s.q(`SELECT `+taskCommentColumns+` FROM (
SELECT `+taskCommentColumns+` FROM task_comments WHERE task_id=$1 ORDER BY id DESC LIMIT $2
) recent ORDER BY id ASC`, `SELECT `+taskCommentColumns+` FROM (
SELECT `+taskCommentColumns+` FROM task_comments WHERE task_id=? ORDER BY id DESC LIMIT ?
) recent ORDER BY id ASC`), taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskComment{}
	for rows.Next() {
		var x TaskComment
		if err := scanTaskComment(rows, &x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// TaskBriefs returns the id/title/state of the named tasks in one read. It is
// the parent and blocker lookup task_get uses, so a detail read never walks a
// related task's full history.
func (s *Store) TaskBriefs(ctx context.Context, ids []int64) (map[int64]TaskChild, error) {
	out := map[int64]TaskChild{}
	if len(ids) == 0 {
		return out, nil
	}
	if len(ids) > 1000 {
		return nil, errors.New("invalid task brief query")
	}
	rows, err := s.queryByIDs(ctx, `SELECT id,title,state FROM tasks WHERE id=ANY($1) ORDER BY id`, `SELECT id,title,state FROM tasks WHERE id IN (%s) ORDER BY id`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c TaskChild
		if err := rows.Scan(&c.ID, &c.Title, &c.State); err != nil {
			return nil, err
		}
		c.Category = TaskCategory(c.State)
		out[c.ID] = c
	}
	return out, rows.Err()
}

// TasksByIDs reads the named tasks in id order. The changes feed uses it to
// attach a summary to each task row without one query per item.
func (s *Store) TasksByIDs(ctx context.Context, ids []int64) ([]Task, error) {
	if len(ids) == 0 {
		return []Task{}, nil
	}
	if len(ids) > 1000 {
		return nil, errors.New("invalid task id batch")
	}
	rows, err := s.queryByIDs(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=ANY($1) ORDER BY id`, `SELECT `+taskColumns+` FROM tasks WHERE id IN (%s) ORDER BY id`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		var x Task
		if err := scanTask(rows, &x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// TaskIDsForComments maps comment ids to their task ids. The changes feed
// reads it so a comment item can name its task even after the comment row is
// gone.
func (s *Store) TaskIDsForComments(ctx context.Context, ids []int64) (map[int64]int64, error) {
	out := map[int64]int64{}
	if len(ids) == 0 {
		return out, nil
	}
	if len(ids) > 1000 {
		return nil, errors.New("invalid comment id batch")
	}
	rows, err := s.queryByIDs(ctx, `SELECT id,task_id FROM task_comments WHERE id=ANY($1)`, `SELECT id,task_id FROM task_comments WHERE id IN (%s)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var commentID, taskID int64
		if err := rows.Scan(&commentID, &taskID); err != nil {
			return nil, err
		}
		out[commentID] = taskID
	}
	return out, rows.Err()
}

// SyncIDAt returns the newest sync_log position recorded at or before t, or 0
// when the log has nothing that old. It turns a changes_since timestamp into
// the cursor the feed pages on, so a caller that starts from a time still
// receives every row recorded after that position exactly once.
func (s *Store) SyncIDAt(ctx context.Context, t time.Time) (int64, error) {
	var id int64
	err := s.db().QueryRow(ctx, s.q(`SELECT COALESCE(max(sync_id),0) FROM sync_log WHERE sync_id IS NOT NULL AND at <= $1`, `SELECT COALESCE(max(sync_id),0) FROM sync_log WHERE sync_id IS NOT NULL AND at <= ?`), t.UTC()).Scan(&id)
	return id, err
}

// queryByIDs runs a statement over a list of ids: the PostgreSQL text takes the
// slice as $1 (= ANY), the MySQL text carries one placeholder per id, in the
// %s of its format.
func (s *Store) queryByIDs(ctx context.Context, pg, mysqlFormat string, ids []int64) (rows, error) {
	if s.dialect == mysqlDialect {
		return s.db().Query(ctx, fmt.Sprintf(mysqlFormat, placeholders(len(ids))), reportIDArgs(ids)...)
	}
	return s.db().Query(ctx, pg, ids)
}
