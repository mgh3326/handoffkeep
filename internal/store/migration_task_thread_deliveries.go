package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261111-task-thread-deliveries adds the Discord task-thread outbox
// (hk 3990; hk:doc design/2026-11-11/operator-surfaces §1, task_threads.go).
// task_thread_deliveries holds one row per (task, destination): the thread
// the poster created for the task, the last task event it posted, the keys of
// the other records it posted (review gates, deploys, decisions), and the
// claim that guards a send. No message text is stored: every line is rendered
// again from the hk record it describes. task_thread_cursors holds, per
// destination, how far the poster has scanned task_events for started
// transitions (scan_event_id) and the newest event when the poster first ran
// (epoch_event_id): no task that started before it gets a thread, so turning
// the poster on posts nothing old.
//
// Both are new tables with no foreign key, so the migration locks nothing
// that exists (ACCESS EXCLUSIVE on the new tables only) and the previous
// release, which never reads them, is unaffected.
var taskThreadDeliveriesMigration = []string{
	`CREATE TABLE IF NOT EXISTS task_thread_deliveries (
		task_id BIGINT NOT NULL,
		destination TEXT NOT NULL CHECK(destination IN ('discord')),
		thread_id TEXT CHECK(thread_id ~ '^[0-9]{1,32}$'),
		thread_name TEXT NOT NULL,
		start_event_id BIGINT NOT NULL,
		since TIMESTAMPTZ NOT NULL,
		last_event_id BIGINT NOT NULL,
		posted TEXT[] NOT NULL DEFAULT '{}',
		state TEXT NOT NULL CHECK(state IN ('pending','creating','active','needs_operator','failed')),
		claim_key TEXT NOT NULL DEFAULT '',
		claimed_at TIMESTAMPTZ,
		next_attempt_at TIMESTAMPTZ NOT NULL,
		attempts INT NOT NULL DEFAULT 0,
		last_error TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL,
		PRIMARY KEY(task_id, destination),
		CHECK((state = 'active') = (thread_id IS NOT NULL) OR state IN ('needs_operator','failed')),
		CHECK((claim_key = '') = (claimed_at IS NULL))
	)`,
	`CREATE INDEX IF NOT EXISTS task_thread_deliveries_due ON task_thread_deliveries(destination, state, next_attempt_at)`,
	`CREATE TABLE IF NOT EXISTS task_thread_cursors (
		destination TEXT PRIMARY KEY CHECK(destination IN ('discord')),
		epoch_event_id BIGINT NOT NULL,
		scan_event_id BIGINT NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL
	)`,
}

func init() {
	registerMigration(migration{ID: "20261111-task-thread-deliveries", Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, taskThreadDeliveriesMigration)
	}})
}
