package store

import (
	"context"
	"fmt"
	"time"
)

const ytWriteOpAlertDelay = 10 * time.Minute

// EnqueueYTWriteOpAlerts records one outbox event per op and condition. The
// row lock keeps a pending alert from racing a concurrent resolution; the
// deterministic event id makes each condition idempotent across workers.
func (s *Store) EnqueueYTWriteOpAlerts(ctx context.Context, now time.Time) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `SELECT id,kind,entity_id,state,created_at FROM yt_write_ops
		WHERE (state IN ('prepared','dispatched','remote_unknown') AND created_at <= $1
				AND NOT EXISTS (SELECT 1 FROM notification_outbox n WHERE n.event_id='yt-op-'||yt_write_ops.id::text||'-pending'))
			OR (state='rejected'
				AND NOT EXISTS (SELECT 1 FROM notification_outbox n WHERE n.event_id='yt-op-'||yt_write_ops.id::text||'-rejected'))
		ORDER BY id FOR SHARE`, now.UTC().Add(-ytWriteOpAlertDelay))
	if err != nil {
		return 0, err
	}
	type candidate struct {
		id                    int64
		kind, entityID, state string
		createdAt             time.Time
	}
	var candidates []candidate
	for rows.Next() {
		var x candidate
		if err := rows.Scan(&x.id, &x.kind, &x.entityID, &x.state, &x.createdAt); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, x)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	created := 0
	for _, x := range candidates {
		condition := "pending"
		ageAt := now.UTC()
		if x.state == "rejected" {
			condition = "rejected"
		}
		age := int(ageAt.Sub(x.createdAt.UTC()).Minutes())
		if age < 0 {
			age = 0
		}
		text := fmt.Sprintf("[yt-ops] %s op=%d kind=%s task=%s age=%dm | handoffkeep yt ops --task %s", condition, x.id, x.kind, x.entityID, age, x.entityID)
		inserted, err := insertNotificationOutboxOnce(ctx, tx, OutboxKindYTWriteOp, "handoffkeep-design", fmt.Sprintf("yt-op-%d-%s", x.id, condition), text, now.UTC())
		if err != nil {
			return 0, err
		}
		if inserted {
			created++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return created, nil
}
