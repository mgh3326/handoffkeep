package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// YouTrackSyncStatus is a read-only snapshot of the sync bookkeeping: the
// cursor, the outbox backlog, the last 24 hours of processed activities, the
// mirrored task count and the newest sync timestamp the store can see.
type YouTrackSyncStatus struct {
	Cursor                *YouTrackCursor
	OutboxPending         int
	OutboxOldestPendingAt *time.Time
	OutboxMaxAttempts     int
	OutboxLastError       string
	Processed24h          map[string]int
	Processed24hTotal     int
	MirroredTasks         int
	PendingReverts        int
	// LastRunAt is the newest timestamp any sync step leaves behind. A cursor
	// row the migration created but no run advanced does not count, so an
	// untouched database reports no run.
	LastRunAt *time.Time
}

// YouTrackSyncStatus reads the sync tables in one snapshot. now is the
// caller's clock: it bounds the 24-hour window.
func (s *Store) YouTrackSyncStatus(ctx context.Context, now time.Time) (YouTrackSyncStatus, error) {
	out := YouTrackSyncStatus{Processed24h: map[string]int{
		YouTrackOutcomeApplied:   0,
		YouTrackOutcomeSkipped:   0,
		YouTrackOutcomeRejected:  0,
		YouTrackOutcomeDuplicate: 0,
	}}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)

	var cursor YouTrackCursor
	err = tx.QueryRow(ctx, `SELECT cursor,last_activity_id,updated_at FROM youtrack_cursor WHERE singleton`).
		Scan(&cursor.Cursor, &cursor.LastActivityID, &cursor.UpdatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return out, err
	default:
		cursor.UpdatedAt = cursor.UpdatedAt.UTC()
		out.Cursor = &cursor
	}

	if err = tx.QueryRow(ctx, `SELECT COUNT(*),MIN(created_at),COALESCE(MAX(attempts),0),
			COALESCE((SELECT last_error FROM youtrack_outbox WHERE done_at IS NULL AND last_error<>'' ORDER BY id DESC LIMIT 1),'')
		FROM youtrack_outbox WHERE done_at IS NULL`).
		Scan(&out.OutboxPending, &out.OutboxOldestPendingAt, &out.OutboxMaxAttempts, &out.OutboxLastError); err != nil {
		return out, err
	}
	if out.OutboxOldestPendingAt != nil {
		t := out.OutboxOldestPendingAt.UTC()
		out.OutboxOldestPendingAt = &t
	}

	rows, err := tx.Query(ctx, `SELECT outcome,COUNT(*) FROM youtrack_processed_activities
		WHERE processed_at >= $1 GROUP BY outcome`, now.Add(-24*time.Hour))
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var outcome string
		var count int
		if err := rows.Scan(&outcome, &count); err != nil {
			rows.Close()
			return out, err
		}
		out.Processed24h[outcome] = count
		out.Processed24hTotal += count
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	if err = tx.QueryRow(ctx, `SELECT COUNT(*) FROM task_origins WHERE provider=$1`, YouTrackProvider).
		Scan(&out.MirroredTasks); err != nil {
		return out, err
	}
	if err = tx.QueryRow(ctx, `SELECT COUNT(*) FROM youtrack_processed_activities WHERE NOT revert_done`).
		Scan(&out.PendingReverts); err != nil {
		return out, err
	}

	// GREATEST ignores NULL, so this is NULL only when no sync step has left a
	// timestamp. The cursor counts after its first advance; the migration's
	// default row alone does not.
	var lastRun *time.Time
	if err = tx.QueryRow(ctx, `SELECT GREATEST(
			(SELECT updated_at FROM youtrack_cursor WHERE singleton AND (cursor<>'' OR last_activity_id<>'')),
			(SELECT MAX(processed_at) FROM youtrack_processed_activities),
			(SELECT MAX(synced_at) FROM task_origins WHERE provider=$1),
			(SELECT MAX(fetched_at) FROM youtrack_field_cache),
			(SELECT MAX(done_at) FROM youtrack_outbox),
			(SELECT MAX(created_at) FROM youtrack_comment_links)
		)`, YouTrackProvider).Scan(&lastRun); err != nil {
		return out, err
	}
	if lastRun != nil {
		t := lastRun.UTC()
		out.LastRunAt = &t
	}
	return out, nil
}
