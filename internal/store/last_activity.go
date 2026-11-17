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
	if s != nil && s.dialect == mysqlDialect {
		return 0, ErrDialectUnsupported
	}

	rows, err := s.db().Query(ctx, `SELECT t.id, a.at FROM tasks t JOIN (SELECT task_id, max(at) AS at FROM task_events WHERE `+activityEventSQL+` GROUP BY task_id) a ON a.task_id = t.id WHERE a.at > t.last_activity_at ORDER BY t.id LIMIT $1`, limit)
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
		var id int64
		err := s.db().QueryRow(ctx, `UPDATE tasks SET last_activity_at = GREATEST(last_activity_at, $2) WHERE id = $1 AND last_activity_at < $2 RETURNING id`, c.id, c.at).Scan(&id)
		if errors.Is(err, ErrNoRows) {
			continue
		}
		if err != nil {
			return repaired, err
		}
		repaired++
	}
	return repaired, nil
}
