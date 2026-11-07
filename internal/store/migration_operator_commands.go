package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Operator commands are actions that touch something outside hk: the console
// records the operator's intent, the owning executor runs it and reports a
// receipt (design/2026-11-07/console-operator-control-design §2). This
// creates the new table only: ACCESS EXCLUSIVE on operator_commands, which
// nothing reads yet, and no lock on an existing table. It deliberately has no
// foreign key to tasks, so creating it takes no lock on tasks — the task link
// is an ordinary nullable bigint the writer validates.
func init() {
	registerMigration(migration{ID: "20261107-operator-commands", Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, []string{
			`CREATE TABLE operator_commands (
				id BIGSERIAL PRIMARY KEY,
				command_key TEXT NOT NULL UNIQUE CHECK(length(command_key) BETWEEN 1 AND 128),
				kind TEXT NOT NULL CHECK(kind IN ('worker.message','job.stop','task.respawn','deploy.approve')),
				target_type TEXT NOT NULL CHECK(target_type IN ('task','job','service')),
				target_id TEXT NOT NULL CHECK(length(target_id) BETWEEN 1 AND 128),
				task_id BIGINT,
				executor_lane TEXT NOT NULL CHECK(length(executor_lane) BETWEEN 1 AND 128),
				payload JSONB NOT NULL,
				actor TEXT NOT NULL,
				channel TEXT NOT NULL,
				state TEXT NOT NULL CHECK(state IN ('recorded','delivered','acked','done','failed','expired','superseded')),
				attempts INT NOT NULL DEFAULT 0,
				last_error_code TEXT NOT NULL DEFAULT '',
				created_at TIMESTAMPTZ NOT NULL,
				delivered_at TIMESTAMPTZ,
				acked_at TIMESTAMPTZ,
				finished_at TIMESTAMPTZ,
				expires_at TIMESTAMPTZ NOT NULL
			)`,
			`CREATE INDEX operator_commands_task ON operator_commands(task_id, created_at DESC, id DESC)`,
			`CREATE INDEX operator_commands_executor ON operator_commands(executor_lane, state)`,
		})
	}})
}
