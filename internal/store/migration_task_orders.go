package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const taskOrdersMigrationID = "20261118-task-orders"

func init() {
	registerMigration(migration{
		ID:    taskOrdersMigrationID,
		After: []string{"20261114-deploy-lag-outbox"},
		Apply: func(ctx context.Context, tx pgx.Tx) error { return execAll(ctx, tx, taskOrdersStatements) },
	})
}

// task_orders records the operator's "set going" for one task (hk 5125,
// docs/task-orders.md). The statements are expand-only: a new table, and a
// wider notification_outbox_kind_check that admits 'task_order' and rejects
// nothing it accepted, so the previous release keeps writing its own kinds.
//
// The CREATE TABLE and its index lock only the new table; its foreign key takes
// a brief SHARE ROW EXCLUSIVE on tasks. The constraint swap takes ACCESS
// EXCLUSIVE on notification_outbox for the drop and the add; NOT VALID skips
// the scan of old rows. The kind list is the union of every earlier rewrite of
// the constraint (20261114-deploy-lag-outbox is the latest) plus task_order.
//
// The row's state is the pair acked_at / withdrawn_at: both unset is an open
// order, which waits on target_lane; the CHECK makes the two closes exclusive,
// so the lane wait clears at most once. verified follows the channel: a dots
// relay is never verified.
var taskOrdersStatements = []string{
	`CREATE TABLE task_orders (
 id TEXT PRIMARY KEY CHECK (id ~ '^ord-[a-z2-7]{10}$'),
 task_id BIGINT NOT NULL REFERENCES tasks(id),
 words TEXT NOT NULL CHECK (btrim(words) <> ''),
 channel TEXT NOT NULL CHECK (channel IN ('console','dots','cli')),
 actor TEXT NOT NULL CHECK (actor <> ''),
 verified BOOLEAN NOT NULL,
 idempotency_key TEXT NOT NULL CHECK (idempotency_key <> ''),
 created_at TIMESTAMPTZ NOT NULL,
 target_lane TEXT NOT NULL CHECK (target_lane <> ''),
 acked_at TIMESTAMPTZ, acked_by TEXT, ack_comment_id BIGINT,
 withdrawn_at TIMESTAMPTZ, withdrawn_by TEXT, withdraw_reason TEXT,
 CONSTRAINT task_orders_verified_channel CHECK (verified = (channel <> 'dots')),
 CONSTRAINT task_orders_one_close CHECK (acked_at IS NULL OR withdrawn_at IS NULL),
 CONSTRAINT task_orders_ack_by CHECK ((acked_at IS NULL) = (acked_by IS NULL)),
 CONSTRAINT task_orders_withdraw_by CHECK ((withdrawn_at IS NULL) = (withdrawn_by IS NULL)),
 CONSTRAINT task_orders_actor_key UNIQUE (actor, idempotency_key))`,
	`CREATE INDEX task_orders_task ON task_orders (task_id, id)`,
	`ALTER TABLE notification_outbox DROP CONSTRAINT IF EXISTS notification_outbox_kind_check`,
	`ALTER TABLE notification_outbox ADD CONSTRAINT notification_outbox_kind_check CHECK(kind IN ('decision_answered','chat_answer','dots_report','dots_decision','dots_blocked','dots_followup','yt_write_op','decision_denied','disposition_answered','deploy_result','deploy_lag','task_order')) NOT VALID`,
}
