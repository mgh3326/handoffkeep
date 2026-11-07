package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const projectDesignLaneMigrationID = "20261107-project-design-lane"

func init() {
	registerMigration(migration{ID: projectDesignLaneMigrationID, Apply: applyProjectDesignLaneMigration})
}

// applyProjectDesignLaneMigration records which design session owns each
// project (hk 3420, policy/design-sessions). It is needed because ownership
// exists only as prose in hk docs today, so nothing can answer "who owns
// project X" and spawners type --owner by hand.
//
// task_projects is the small project vocabulary table (18 seeded rows), so the
// ALTER TABLE takes ACCESS EXCLUSIVE on it for a moment only; the constant
// DEFAULT ” makes the add metadata-only, with no table rewrite. The new events
// table and its index lock nothing that exists yet. A bounded lock wait keeps a
// long reader from stalling the project and task API behind the pending
// exclusive lock, the same guard the other dated migrations use.
func applyProjectDesignLaneMigration(ctx context.Context, tx pgx.Tx) error {
	var priorLockTimeout string
	if err := tx.QueryRow(ctx, `SELECT current_setting('lock_timeout')`).Scan(&priorLockTimeout); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return err
	}
	if err := execAll(ctx, tx, []string{
		`ALTER TABLE task_projects ADD COLUMN design_lane TEXT NOT NULL DEFAULT ''`,
		`CREATE TABLE project_owner_events (
 id BIGSERIAL PRIMARY KEY,
 project TEXT NOT NULL REFERENCES task_projects(name),
 previous_lane TEXT NOT NULL DEFAULT '',
 design_lane TEXT NOT NULL DEFAULT '',
 actor TEXT NOT NULL,
 note TEXT NOT NULL DEFAULT '',
 created_at TIMESTAMPTZ NOT NULL)`,
		`CREATE INDEX project_owner_events_project_id ON project_owner_events(project, id DESC)`,
	}); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, priorLockTimeout)
	return err
}
