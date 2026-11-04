package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Store) SetTaskSubscriber(ctx context.Context, id int64, subscriber, source, by string, active bool) (Task, bool, error) {
	if id < 1 || subscriber == "" || !validText(subscriber, 128) || source == "" || !validText(source, 128) || by == "" || !validText(by, 128) {
		return Task{}, false, errors.New("invalid subscriber")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, false, err
	}
	defer tx.Rollback(ctx)
	var x Task
	if err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, id), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Task{}, false, ErrTaskNotFound
		}
		return Task{}, false, err
	}
	if err = s.wrkTaskFenceTx(ctx, tx, x, "", true); err != nil {
		return Task{}, false, err
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_subscribers WHERE task_id=$1 AND subscriber=$2 AND archived_at IS NULL)`, id, subscriber).Scan(&exists); err != nil {
		return Task{}, false, err
	}
	if active == exists {
		return x, false, tx.Commit(ctx)
	}
	now := time.Now().UTC()
	if active {
		_, err = tx.Exec(ctx, `INSERT INTO task_subscribers(task_id,subscriber,source,created_at) VALUES($1,$2,$3,$4) ON CONFLICT(task_id,subscriber) DO UPDATE SET archived_at=NULL,source=EXCLUDED.source`, id, subscriber, source, now)
	} else {
		_, err = tx.Exec(ctx, `UPDATE task_subscribers SET archived_at=$3 WHERE task_id=$1 AND subscriber=$2`, id, subscriber, now)
	}
	if err != nil {
		return Task{}, false, err
	}
	from, to := "", subscriber
	if !active {
		from, to = subscriber, ""
	}
	if _, _, err = insertEvent(ctx, tx, taskEventInput{TaskID: id, Kind: "metadata", From: x.State, To: x.State, By: by, Refs: x.Refs, At: now, Field: "subscribers", FromValue: from, ToValue: to}); err != nil {
		return Task{}, false, err
	}
	if err = scanTask(tx.QueryRow(ctx, `UPDATE tasks SET updated_at=$2 WHERE id=$1 RETURNING `+taskColumns, id, now), &x); err != nil {
		return Task{}, false, err
	}
	return x, true, tx.Commit(ctx)
}
