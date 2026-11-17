package store

import (
	"context"
	"errors"
	"time"
)

// RepairLastActivity raises tasks.last_activity_at to the newest activity
// event of up to limit tasks whose clock is behind it, as after a rollback to a
// binary older than v44 that wrote task_events without moving the column. It
// never lowers the clock or touches updated_at. Candidates come from one read;
// each task is repaired in its own statement with insertEvent's guarded UPDATE
// (a single-task UPDATE, lock level 4), so no row lock spans tasks. Each
// repaired task writes one sync_log row through sync_tasks.
func (s *Store) RepairLastActivity(ctx context.Context, limit int) (int, error) {
	rows, err := s.db().Query(ctx, s.q(`SELECT t.id, a.at FROM tasks t JOIN (SELECT task_id, max(at) AS at FROM task_events WHERE `+activityEventSQL+` GROUP BY task_id) a ON a.task_id = t.id WHERE a.at > t.last_activity_at ORDER BY t.id LIMIT $1`,
		`SELECT t.id, a.at FROM tasks t JOIN (SELECT task_id, max(at) AS at FROM task_events WHERE `+activityEventSQL+` GROUP BY task_id) a ON a.task_id = t.id WHERE a.at > t.last_activity_at ORDER BY t.id LIMIT ?`), limit)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		id int64
		at time.Time
	}
	var todo []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.at); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, c)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	repaired := 0
	for _, c := range todo {
		moved, err := s.repairLastActivityOne(ctx, c.id, c.at)
		if err != nil {
			return repaired, err
		}
		if moved {
			repaired++
		}
	}
	return repaired, nil
}

// repairLastActivityOne raises one task's clock and reports whether it moved.
// On MySQL the write runs in its own transaction, so its sync_log row is
// numbered at commit; the guarded UPDATE counts matched rows, and a match means
// the clock was behind.
func (s *Store) repairLastActivityOne(ctx context.Context, id int64, at time.Time) (bool, error) {
	if s.dialect != mysqlDialect {
		var got int64
		err := s.db().QueryRow(ctx, `UPDATE tasks SET last_activity_at = GREATEST(last_activity_at, $2) WHERE id = $1 AND last_activity_at < $2 RETURNING id`, id, at).Scan(&got)
		if errors.Is(err, ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}
	var moved bool
	err := s.retryTx(ctx, func() error {
		moved = false
		tx, release, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer release()
		defer tx.Rollback(ctx)
		n, err := tx.Exec(ctx, `UPDATE tasks SET last_activity_at = GREATEST(last_activity_at, ?) WHERE id = ? AND last_activity_at < ?`, at, id, at)
		if err != nil {
			return err
		}
		moved = n > 0
		return tx.Commit(ctx)
	})
	return moved, err
}
